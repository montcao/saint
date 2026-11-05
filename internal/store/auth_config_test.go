package store

import (
	"strings"
	"testing"
)

func TestProxyAuthOrDefault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: ProxyAuthAuto},
		{in: "auto", want: ProxyAuthAuto},
		{in: "key", want: ProxyAuthKey},
		{in: "KEY", want: ProxyAuthKey},
		{in: "  subscription  ", want: ProxyAuthSubscription},
		// A typo must not stop saint from starting; doctor reports it instead.
		{in: "subscribe", want: ProxyAuthAuto},
	}

	for _, tc := range cases {
		if got := (Config{ProxyAuth: tc.in}).ProxyAuthOrDefault(); got != tc.want {
			t.Errorf("ProxyAuthOrDefault(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProxyAuthIsValid(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{"", "auto", "key", "subscription"} {
		if !(Config{ProxyAuth: valid}).ProxyAuthIsValid() {
			t.Errorf("ProxyAuthIsValid(%q) = false, want true", valid)
		}
	}
	if (Config{ProxyAuth: "subscribe"}).ProxyAuthIsValid() {
		t.Error("ProxyAuthIsValid(\"subscribe\") = true, want false")
	}
}

func TestCredentialSpec(t *testing.T) {
	t.Parallel()

	cfg := Config{Credentials: map[string]string{
		CredentialClaudeSubscription: "  op://Private/Claude/token  ",
	}}
	if got, want := cfg.CredentialSpec(CredentialClaudeSubscription), "op://Private/Claude/token"; got != want {
		t.Errorf("CredentialSpec() = %q, want %q", got, want)
	}
	// An unset credential means the keychain, which Parse treats as the default.
	if got := cfg.CredentialSpec(CredentialAnthropic); got != "" {
		t.Errorf("CredentialSpec() for an unset name = %q, want empty", got)
	}
	if got := (Config{}).CredentialSpec(CredentialOpenAI); got != "" {
		t.Errorf("CredentialSpec() with no map = %q, want empty", got)
	}
}

// Sessions created before proxy_auth existed must not be treated as
// subscription sessions: the proxy would stop injecting a key they still need.
func TestSessionProxyAuthDefaultsToKey(t *testing.T) {
	t.Parallel()

	if got := (Session{}).ProxyAuthOrDefault(); got != ProxyAuthKey {
		t.Errorf("ProxyAuthOrDefault() = %q, want %q", got, ProxyAuthKey)
	}
	if got := (Session{ProxyAuth: ProxyAuthSubscription}).ProxyAuthOrDefault(); got != ProxyAuthSubscription {
		t.Errorf("ProxyAuthOrDefault() = %q, want %q", got, ProxyAuthSubscription)
	}
	// Auto is a config value, not a session value: a session is one or the other.
	if got := (Session{ProxyAuth: ProxyAuthAuto}).ProxyAuthOrDefault(); got != ProxyAuthKey {
		t.Errorf("ProxyAuthOrDefault() = %q, want %q", got, ProxyAuthKey)
	}
}

func TestDefaultConfigUsesAuto(t *testing.T) {
	t.Parallel()

	if got := DefaultConfig().ProxyAuthOrDefault(); got != ProxyAuthAuto {
		t.Errorf("default proxy_auth = %q, want %q", got, ProxyAuthAuto)
	}
}

// This rule lived in three packages and a change to which credentials count as
// a subscription was applied to two of them, so the same machine reported
// "subscription" on one screen and "api key" on another. One place now.
func TestResolveProxyAuth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		configured  string
		haveSub     bool
		wantMode    string
		wantWhyPart string
	}{
		{configured: ProxyAuthAuto, haveSub: true, wantMode: "subscription", wantWhyPart: "Claude plan"},
		{configured: ProxyAuthAuto, haveSub: false, wantMode: "api key", wantWhyPart: "no subscription token"},
		{configured: "", haveSub: true, wantMode: "subscription", wantWhyPart: "Claude plan"},
		// Pinning to key must not flip just because a credential exists.
		{configured: ProxyAuthKey, haveSub: true, wantMode: "api key", wantWhyPart: "pinned to key"},
		{configured: ProxyAuthSubscription, haveSub: false, wantMode: "api key", wantWhyPart: "no subscription token"},
		{configured: ProxyAuthSubscription, haveSub: true, wantMode: "subscription", wantWhyPart: "Claude plan"},
	}

	for _, tc := range cases {
		mode, why := ResolveProxyAuth(tc.configured, tc.haveSub)
		if mode != tc.wantMode {
			t.Errorf("ResolveProxyAuth(%q, %v) mode = %q, want %q", tc.configured, tc.haveSub, mode, tc.wantMode)
		}
		if !strings.Contains(why, tc.wantWhyPart) {
			t.Errorf("ResolveProxyAuth(%q, %v) why = %q, want it to mention %q", tc.configured, tc.haveSub, why, tc.wantWhyPart)
		}
	}
}

// Both subscription credentials count. Only the stored token counting is the
// bug that made doctor and the rail disagree with `saint auth`.
func TestIsSubscriptionCredential(t *testing.T) {
	t.Parallel()

	for _, name := range []string{CredentialClaudeSubscription, CredentialClaudeCode} {
		if !IsSubscriptionCredential(name) {
			t.Errorf("IsSubscriptionCredential(%q) = false, want true", name)
		}
	}
	for _, name := range []string{CredentialAnthropic, CredentialOpenAI, CredentialAPIKey, ""} {
		if IsSubscriptionCredential(name) {
			t.Errorf("IsSubscriptionCredential(%q) = true, want false", name)
		}
	}
}
