package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/store"
)

func stateFor(name string, present bool) credentialState {
	kind, _ := lookupCredentialKind(name)
	return credentialState{Kind: kind, Source: "keychain:" + string(kind.Account), Present: present}
}

func TestResolvedAuth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured string
		states     []credentialState
		wantMode   string
		wantWhy    string
	}{
		{name: "auto with a token", configured: store.ProxyAuthAuto, states: []credentialState{stateFor("claude", true)}, wantMode: "subscription", wantWhy: "Claude plan"},
		{name: "auto without a token", configured: store.ProxyAuthAuto, states: []credentialState{stateFor("claude", false)}, wantMode: "api key", wantWhy: "no subscription token"},
		{name: "key is pinned", configured: store.ProxyAuthKey, states: []credentialState{stateFor("claude", true)}, wantMode: "api key", wantWhy: "pinned to key"},
		{name: "subscription without a token", configured: store.ProxyAuthSubscription, states: []credentialState{stateFor("claude", false)}, wantMode: "api key", wantWhy: "no subscription token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mode, why := resolvedAuth(store.Config{ProxyAuth: tc.configured}, tc.states)
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to mention %q", why, tc.wantWhy)
			}
		})
	}
}

// The status is what a new user reads first. It has to say which account is
// about to be charged, and what to do about it.
func TestAuthStatus(t *testing.T) {
	t.Parallel()

	states := []credentialState{stateFor("claude", false), stateFor("anthropic", true), stateFor("openai", false)}
	out := strings.Join(authStatus(store.Config{}, states), "\n")

	for _, want := range []string{"credentials", "claude", "not set", "anthropic", "set", "billing", "proxy_auth", "saint auth claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
}

// With a subscription token in place the nudge is pointless and must not appear.
func TestAuthStatusOmitsTheNudgeWhenSubscriptionIsActive(t *testing.T) {
	t.Parallel()

	states := []credentialState{stateFor("claude", true)}
	out := strings.Join(authStatus(store.Config{}, states), "\n")

	if strings.Contains(out, "saint auth claude") {
		t.Errorf("status nudges toward setup that is already done:\n%s", out)
	}
	if !strings.Contains(out, "subscription") {
		t.Errorf("status does not report the active mode:\n%s", out)
	}
}

// A credential must never be printed back, and the status must not leak one
// even if a source somehow returns it in an error.
func TestAuthStatusCarriesNoSecrets(t *testing.T) {
	t.Parallel()

	const secret = "sk-ant-super-secret"
	states := []credentialState{stateFor("anthropic", true)}
	out := strings.Join(authStatus(store.Config{}, states), "\n")

	if strings.Contains(out, secret) {
		t.Errorf("status leaked a secret:\n%s", out)
	}
}

func TestLookupCredentialKind(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"claude", "CLAUDE", "anthropic", "openai"} {
		if _, ok := lookupCredentialKind(name); !ok {
			t.Errorf("lookupCredentialKind(%q) = false, want true", name)
		}
	}
	if _, ok := lookupCredentialKind("gemini"); ok {
		t.Error("lookupCredentialKind() accepted an unknown credential")
	}
	err := unknownCredential("gemini")
	for _, want := range []string{"gemini", "anthropic-login", "anthropic-token", "anthropic-key", "openai-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestFirstLineTrimsHelperNoise(t *testing.T) {
	t.Parallel()

	if got, want := firstLine("  first\nsecond  "), "first"; got != want {
		t.Errorf("firstLine() = %q, want %q", got, want)
	}
	if got := firstLine(strings.Repeat("x", 300)); len(got) > 95 {
		t.Errorf("firstLine() length = %d, want it truncated", len(got))
	}
}

// Skipping approval prompts is the default: the microVM is the boundary, and an
// agent that stops to ask before every edit inside its own sandbox is mostly
// friction. Losing this default silently turned every CLI session into one that
// asks before each action.
func TestNewDefaultsToSkippingPrompts(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	ask := fs.Bool("ask", false, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if skip := !*ask; !skip {
		t.Error("saint new defaults to asking; it should default to skipping")
	}

	if err := fs.Parse([]string{"-ask"}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if skip := !*ask; skip {
		t.Error("-ask did not put the user back in the loop")
	}
}

// Provider first, then what kind of credential it is. The old names mixed the
// two vocabularies: "claude" beside "anthropic" beside "openai" read as three
// vendors when Claude is Anthropic, and one of the three was a credential type.
func TestCredentialNamesAreProviderFirst(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, kind := range credentialKinds {
		if kind.Provider == "" {
			t.Errorf("credential %q names no provider", kind.Name)
		}
		if !strings.HasPrefix(kind.Name, kind.Provider+"-") {
			t.Errorf("credential %q does not start with its provider %q", kind.Name, kind.Provider)
		}
		if kind.Short == "" {
			t.Errorf("credential %q has no short label for use under a provider heading", kind.Name)
		}
		if seen[kind.Name] {
			t.Errorf("duplicate credential name %q", kind.Name)
		}
		seen[kind.Name] = true
	}
}

// Renaming a command someone learned yesterday is not worth a tidier table.
func TestOldCredentialNamesStillResolve(t *testing.T) {
	t.Parallel()

	for old, want := range map[string]string{
		"claude":    "anthropic-token",
		"live":      "anthropic-login",
		"anthropic": "anthropic-key",
		"openai":    "openai-key",
	} {
		kind, ok := lookupCredentialKind(old)
		if !ok {
			t.Errorf("lookupCredentialKind(%q) = false; the old name stopped working", old)
			continue
		}
		if kind.Name != want {
			t.Errorf("lookupCredentialKind(%q) = %q, want %q", old, kind.Name, want)
		}
	}

	// The new names resolve too, and an unknown one still fails.
	for _, name := range []string{"anthropic-login", "anthropic-token", "anthropic-key", "openai-key"} {
		if _, ok := lookupCredentialKind(name); !ok {
			t.Errorf("lookupCredentialKind(%q) = false", name)
		}
	}
	if _, ok := lookupCredentialKind("gemini-key"); ok {
		t.Error("lookupCredentialKind accepted an unknown credential")
	}
}
