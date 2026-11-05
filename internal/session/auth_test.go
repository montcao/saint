package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// stubSource stands in for a credential, with an optional expiry so the
// remaining-life floor can be exercised without a keychain.
type stubSource struct {
	value   string
	err     error
	status  credential.Status
	inspect bool
}

func (s stubSource) Get(context.Context) (string, error) { return s.value, s.err }
func (s stubSource) Describe() string                    { return "stub credential" }

type inspectableStub struct{ stubSource }

func (s inspectableStub) Status(context.Context) (credential.Status, error) {
	if s.err != nil && s.value == "" {
		return credential.Status{}, s.err
	}
	return s.status, nil
}

func sourceFor(src stubSource) func(string) (credential.Source, error) {
	return func(string) (credential.Source, error) {
		if src.inspect {
			return inspectableStub{stubSource: src}, nil
		}
		return src, nil
	}
}

func TestResolveAuth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	live := func(remaining time.Duration) stubSource {
		return stubSource{
			value:   "oauth",
			inspect: true,
			status:  credential.Status{Present: true, ExpiresAt: now.Add(remaining)},
		}
	}

	cases := []struct {
		name        string
		configured  string
		runtime     sandbox.Runtime
		chosen      string
		source      *stubSource
		wantMode    string
		wantToken   string
		wantWarning string
	}{
		{
			// There is no subscription equivalent for the OpenAI runtime.
			name:       "codex never uses a subscription",
			configured: store.ProxyAuthAuto,
			runtime:    sandbox.RuntimeCodex,
			source:     ptr(live(8 * time.Hour)),
			wantMode:   sandbox.AuthKey,
		},
		{
			name:       "key mode never consults a source",
			configured: store.ProxyAuthKey,
			runtime:    sandbox.RuntimeClaude,
			source:     ptr(live(8 * time.Hour)),
			wantMode:   sandbox.AuthKey,
		},
		{
			name:       "a live credential is used",
			configured: store.ProxyAuthAuto,
			runtime:    sandbox.RuntimeClaude,
			source:     ptr(live(6 * time.Hour)),
			wantMode:   sandbox.AuthSubscription,
			wantToken:  "oauth",
		},
		{
			// Choosing the API key is a decision, not a fallback, so it is
			// never warned about even under an explicit subscription setting.
			name:       "an explicit api-key choice is silent",
			configured: store.ProxyAuthSubscription,
			runtime:    sandbox.RuntimeClaude,
			chosen:     store.CredentialAPIKey,
			source:     ptr(live(8 * time.Hour)),
			wantMode:   sandbox.AuthKey,
		},
		{
			// The whole point of the floor: a session that would outlive its
			// credential is stranded mid-run with an unexplainable auth error.
			name:        "a credential under the floor is refused",
			configured:  store.ProxyAuthSubscription,
			runtime:     sandbox.RuntimeClaude,
			source:      ptr(live(5 * time.Minute)),
			wantMode:    sandbox.AuthKey,
			wantWarning: "under the 15m0s minimum",
		},
		{
			name:       "just over the floor is allowed",
			configured: store.ProxyAuthAuto,
			runtime:    sandbox.RuntimeClaude,
			source:     ptr(live(16 * time.Minute)),
			wantMode:   sandbox.AuthSubscription,
			wantToken:  "oauth",
		},
		{
			// A credential under the floor is refused under "auto" too, and
			// unlike a missing credential this one warns: the user did have a
			// subscription available, the session is now billing money instead,
			// and refreshing the login is something they can act on.
			name:        "auto also honours the floor, and says so",
			configured:  store.ProxyAuthAuto,
			runtime:     sandbox.RuntimeClaude,
			source:      ptr(live(time.Minute)),
			wantMode:    sandbox.AuthKey,
			wantWarning: "under the 15m0s minimum",
		},
		{
			name:        "an unreadable source warns when subscription was asked for",
			configured:  store.ProxyAuthSubscription,
			runtime:     sandbox.RuntimeClaude,
			source:      &stubSource{err: errors.New("keychain is locked"), inspect: true},
			wantMode:    sandbox.AuthKey,
			wantWarning: "API rates",
		},
		{
			// Under auto, falling back is the expected outcome and not worth a
			// warning.
			name:       "an unreadable source is quiet under auto",
			configured: store.ProxyAuthAuto,
			runtime:    sandbox.RuntimeClaude,
			source:     &stubSource{err: errors.New("keychain is locked"), inspect: true},
			wantMode:   sandbox.AuthKey,
		},
		{
			name:        "no source configured warns when subscription was asked for",
			configured:  store.ProxyAuthSubscription,
			runtime:     sandbox.RuntimeClaude,
			wantMode:    sandbox.AuthKey,
			wantWarning: "API rates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &Service{Now: func() time.Time { return now }}
			if tc.source != nil {
				svc.Credentials = sourceFor(*tc.source)
			}
			cfg := store.Config{ProxyAuth: tc.configured}

			got := svc.resolveAuth(context.Background(), cfg, tc.runtime, tc.chosen)
			if got.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.Token != tc.wantToken {
				t.Errorf("token = %q, want %q", got.Token, tc.wantToken)
			}
			if tc.wantWarning == "" {
				if got.Warning != "" {
					t.Errorf("warning = %q, want none", got.Warning)
				}
				return
			}
			if !strings.Contains(got.Warning, tc.wantWarning) {
				t.Errorf("warning = %q, want it to mention %q", got.Warning, tc.wantWarning)
			}
		})
	}
}

// A session records what it actually carried, so the UI can show remaining life
// without re-reading a credential that may since have changed.
func TestResolveAuthRecordsTheExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	expires := now.Add(3 * time.Hour)
	svc := &Service{
		Now: func() time.Time { return now },
		Credentials: sourceFor(stubSource{
			value:   "oauth",
			inspect: true,
			status:  credential.Status{Present: true, ExpiresAt: expires},
		}),
	}

	got := svc.resolveAuth(context.Background(), store.Config{}, sandbox.RuntimeClaude, store.CredentialClaudeCode)
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expires)
	}
	if got.Source != store.CredentialClaudeCode {
		t.Errorf("Source = %q, want the chosen spec", got.Source)
	}
}

func ptr[T any](v T) *T { return &v }

// A session created without naming a credential must use the configured
// default, which is what CreateRequest.Credential's documentation promises.
//
// This resolved to the subscription slot instead. On an install whose default
// is the live Claude Code login and whose subscription slot is empty - which is
// what `saint auth` produces when you log in and never run `claude
// setup-token` - every session created from the CLI silently became a key-mode
// session. Under "auto" the fallback warning is suppressed, so nothing said so,
// and the agent failed on its first request with an error naming the proxy
// rather than the credential.
func TestEmptyCredentialUsesTheConfiguredDefault(t *testing.T) {
	t.Parallel()

	var asked []string
	svc := &Service{
		Now: func() time.Time { return time.Unix(0, 0) },
		Credentials: func(spec string) (credential.Source, error) {
			asked = append(asked, spec)
			return stubSource{value: "oauth-token"}, nil
		},
	}

	cfg := store.Config{
		ProxyAuth:         store.ProxyAuthSubscription,
		DefaultCredential: store.CredentialClaudeCode,
	}

	got := svc.resolveAuth(context.Background(), cfg, sandbox.RuntimeClaude, "")
	if len(asked) != 1 || asked[0] != store.CredentialClaudeCode {
		t.Fatalf("resolved credential specs = %v, want [%s]", asked, store.CredentialClaudeCode)
	}
	if got.Mode != sandbox.AuthSubscription {
		t.Errorf("Mode = %q, want subscription", got.Mode)
	}
	if got.Source != store.CredentialClaudeCode {
		t.Errorf("Source = %q, want %q", got.Source, store.CredentialClaudeCode)
	}
	if got.Warning != "" {
		t.Errorf("Warning = %q, want none: the default resolved fine", got.Warning)
	}
}

// An explicit choice still beats the default.
func TestAnExplicitCredentialBeatsTheDefault(t *testing.T) {
	t.Parallel()

	var asked []string
	svc := &Service{
		Now: func() time.Time { return time.Unix(0, 0) },
		Credentials: func(spec string) (credential.Source, error) {
			asked = append(asked, spec)
			return stubSource{value: "token"}, nil
		},
	}
	cfg := store.Config{
		ProxyAuth:         store.ProxyAuthSubscription,
		DefaultCredential: store.CredentialClaudeCode,
	}

	svc.resolveAuth(context.Background(), cfg, sandbox.RuntimeClaude, store.CredentialClaudeSubscription)
	if len(asked) != 1 || asked[0] != store.CredentialClaudeSubscription {
		t.Fatalf("resolved specs = %v, want the explicitly chosen one", asked)
	}
}

// A session's credential is a copy taken when its container was created, not a
// reference to the host's. The host login refreshes; the copy in a running
// guest never does, because a process environment cannot be changed after the
// process exists. Renewing therefore has to replace the container.
func TestRenewCredentialRebuildsTheContainerWithAFreshToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := store.New(dir)
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	stale := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	now := stale.Add(6 * time.Hour)

	existing := store.Session{
		ID: "abc123", Name: "alpha", Runtime: sandbox.RuntimeClaude,
		Repos:               []sandbox.Repo{{Path: dir}},
		CPUs:                2,
		MemoryGB:            4,
		SkipPermissions:     true,
		ProxyAuth:           store.ProxyAuthSubscription,
		Credential:          store.CredentialClaudeCode,
		CredentialExpiresAt: stale,
	}
	if err := st.SaveSessions([]store.Session{existing}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	sb := &recordingSandbox{}
	svc := &Service{
		Store: st, Sandbox: sb,
		Now: func() time.Time { return now },
		Credentials: func(string) (credential.Source, error) {
			return inspectableStub{stubSource{
				value:  "fresh-token",
				status: credential.Status{Present: true, ExpiresAt: now.Add(4 * time.Hour)},
			}}, nil
		},
	}

	got, err := svc.RenewCredential(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("RenewCredential() error = %v", err)
	}

	if !sb.removed {
		t.Error("the old container was not removed; a new environment needs a new container")
	}
	if sb.opts.SubscriptionToken != "fresh-token" {
		t.Errorf("new container got token %q, want the freshly read one", sb.opts.SubscriptionToken)
	}
	if !got.CredentialExpiresAt.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("recorded expiry = %v, want the fresh one", got.CredentialExpiresAt)
	}

	// A renewal, not a new session: everything that identifies it survives.
	if got.ID != "abc123" || got.Name != "alpha" {
		t.Errorf("identity changed: %+v", got)
	}
	if sb.opts.CPUs != 2 || sb.opts.MemoryGB != 4 || !sb.opts.SkipPermissions {
		t.Errorf("session settings were not preserved: %+v", sb.opts)
	}

	// And it is persisted, or the next render shows the stale expiry again.
	saved, err := st.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions() error = %v", err)
	}
	if len(saved) != 1 || !saved[0].CredentialExpiresAt.Equal(now.Add(4*time.Hour)) {
		t.Errorf("the renewed expiry was not saved: %+v", saved)
	}
}

// A credential that cannot be read is a reason to leave the session alone, not
// to replace a working container with a broken one.
func TestRenewCredentialLeavesTheSessionAloneWhenItCannotResolve(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := store.New(dir)
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	if err := st.SaveSessions([]store.Session{{
		ID: "abc123", Runtime: sandbox.RuntimeClaude, Repos: []sandbox.Repo{{Path: dir}},
		ProxyAuth: store.ProxyAuthSubscription, Credential: store.CredentialClaudeCode,
	}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	sb := &recordingSandbox{}
	svc := &Service{
		Store: st, Sandbox: sb,
		Now:         func() time.Time { return time.Unix(0, 0) },
		Credentials: func(string) (credential.Source, error) { return stubSource{value: ""}, nil },
	}

	if _, err := svc.RenewCredential(context.Background(), "abc123"); err == nil {
		t.Fatal("RenewCredential() succeeded with no usable credential")
	}
	if sb.removed {
		t.Error("a working container was removed for a renewal that could not proceed")
	}
}
