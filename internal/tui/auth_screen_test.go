package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/store"
)

// fakeCredentials records what the screen asked it to do. It never holds a real
// secret, which is also the contract the real implementation has to keep.
type fakeCredentials struct {
	mu     sync.Mutex
	states []CredentialState
	set    map[string]string
	closed []string
	setErr error
}

func newFakeCredentials(states ...CredentialState) *fakeCredentials {
	return &fakeCredentials{states: states, set: map[string]string{}}
}

func (f *fakeCredentials) List(context.Context) ([]CredentialState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CredentialState(nil), f.states...), nil
}

func (f *fakeCredentials) Set(_ context.Context, config, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.set[config] = value
	return nil
}

func (f *fakeCredentials) Clear(_ context.Context, config string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, config)
	return nil
}

func claudeState(present bool) CredentialState {
	return CredentialState{
		Kind: CredentialKind{
			Name:    "claude",
			Config:  store.CredentialClaudeSubscription,
			Summary: "Claude subscription",
			Setup:   "run claude setup-token",
		},
		Source:   "keychain:claude-subscription",
		Present:  present,
		Editable: true,
	}
}

func authModel(t *testing.T, creds Credentials, states ...CredentialState) Model {
	t.Helper()

	m := NewModel(nil, nil).WithCredentials(creds)
	m.width, m.height = 120, 40
	m.mode = modeAuth
	m.auth.states = states
	m.authMode = store.ProxyAuthAuto
	return m
}

func TestResolvedAuthMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured string
		states     []CredentialState
		wantMode   string
		wantWhy    string
	}{
		{name: "auto with a token", configured: store.ProxyAuthAuto, states: []CredentialState{claudeState(true)}, wantMode: "subscription", wantWhy: "Claude plan"},
		{name: "auto without a token", configured: store.ProxyAuthAuto, states: []CredentialState{claudeState(false)}, wantMode: "api key", wantWhy: "no subscription token"},
		// Pinning key must not flip to subscription just because a token exists.
		{name: "key ignores a token", configured: store.ProxyAuthKey, states: []CredentialState{claudeState(true)}, wantMode: "api key", wantWhy: "pinned to key"},
		{name: "subscription without a token", configured: store.ProxyAuthSubscription, states: []CredentialState{claudeState(false)}, wantMode: "api key", wantWhy: "no subscription token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mode, why := resolvedAuthMode(tc.configured, tc.states)
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to mention %q", why, tc.wantWhy)
			}
		})
	}
}

func TestNextAuthModeCycles(t *testing.T) {
	t.Parallel()

	seen := []string{store.ProxyAuthAuto}
	mode := store.ProxyAuthAuto
	for range 3 {
		mode = nextAuthMode(mode)
		seen = append(seen, mode)
	}
	// Back where it started, having visited each mode once.
	if seen[3] != store.ProxyAuthAuto {
		t.Errorf("cycle = %v, want it to return to auto", seen)
	}
	for _, want := range []string{store.ProxyAuthKey, store.ProxyAuthSubscription} {
		if !strings.Contains(strings.Join(seen, " "), want) {
			t.Errorf("cycle = %v, missing %q", seen, want)
		}
	}
}

// Typing a secret must reach the store, and must not be echoed on a screen
// someone may be sharing.
func TestEnteringASecretStoresIt(t *testing.T) {
	t.Parallel()

	creds := newFakeCredentials()
	m := authModel(t, creds, claudeState(false))

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.auth.editing == nil {
		t.Fatal("enter did not open the editor")
	}

	for _, r := range "sk-secret-value" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}
	if view := m.renderAuthScreen(); strings.Contains(view, "sk-secret-value") {
		t.Error("the secret was echoed on screen")
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("storing produced no command")
	}
	if _, ok := cmd().(credentialActionMsg); !ok {
		t.Fatal("storing did not report an action")
	}
	if got := creds.set[store.CredentialClaudeSubscription]; got != "sk-secret-value" {
		t.Errorf("stored %q, want the typed value", got)
	}
	if m.auth.editing != nil {
		t.Error("the editor stayed open after storing")
	}
}

// While a secret is being typed, every key belongs to the editor. A stray "x"
// must not clear the credential being set.
func TestKeysGoToTheEditorWhileTyping(t *testing.T) {
	t.Parallel()

	creds := newFakeCredentials()
	m := authModel(t, creds, claudeState(true))

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	for _, r := range "xmq" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}

	if len(creds.closed) != 0 {
		t.Errorf("a keystroke meant for the value cleared %v", creds.closed)
	}
	if m.mode != modeAuth {
		t.Errorf("mode = %v, want to still be on the auth screen", m.mode)
	}
	if m.auth.editing == nil {
		t.Fatal("the editor closed while typing")
	}
}

func TestEscapeCancelsTheEditorWithoutStoring(t *testing.T) {
	t.Parallel()

	creds := newFakeCredentials()
	m := authModel(t, creds, claudeState(false))

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)

	if m.auth.editing != nil {
		t.Error("esc did not close the editor")
	}
	if len(creds.set) != 0 {
		t.Errorf("esc stored %v", creds.set)
	}
	// Still on the screen: the first esc cancels the edit, not the screen.
	if m.mode != modeAuth {
		t.Errorf("mode = %v, want to remain on the auth screen", m.mode)
	}
}

// A value saint cannot write must say so rather than opening an editor whose
// result would be ignored.
func TestNonEditableCredentialExplainsItself(t *testing.T) {
	t.Parallel()

	state := claudeState(true)
	state.Editable = false
	state.Source = "op://Private/Claude/token"

	m := authModel(t, newFakeCredentials(), state)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if m.auth.editing != nil {
		t.Fatal("an editor opened for a credential saint cannot write")
	}
	if !strings.Contains(m.status, "op://Private/Claude/token") {
		t.Errorf("status = %q, want it to name where the value comes from", m.status)
	}
}

func TestClearRemovesACredential(t *testing.T) {
	t.Parallel()

	creds := newFakeCredentials()
	m := authModel(t, creds, claudeState(true))

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd == nil {
		t.Fatal("x produced no command")
	}
	cmd()
	if len(creds.closed) != 1 || creds.closed[0] != store.CredentialClaudeSubscription {
		t.Errorf("cleared = %v, want the selected credential", creds.closed)
	}
}

func TestAuthScreenShowsWhoPays(t *testing.T) {
	t.Parallel()

	m := authModel(t, newFakeCredentials(), claudeState(true))
	view := m.renderAuthScreen()

	for _, want := range []string{"CREDENTIALS", "claude", "BILLING", "subscription"} {
		if !strings.Contains(view, want) {
			t.Errorf("auth screen is missing %q:\n%s", want, view)
		}
	}
}

// Without a credential store the screen still opens and says why it cannot do
// anything, rather than crashing.
func TestAuthScreenWithoutACredentialStore(t *testing.T) {
	t.Parallel()

	m := NewModel(nil, nil)
	m.width, m.height = 120, 40

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = next.(Model)
	if m.mode != modeAuth {
		t.Fatalf("mode = %v, want the auth screen", m.mode)
	}
	if cmd == nil {
		t.Fatal("opening produced no command")
	}
	msg, ok := cmd().(credentialsMsg)
	if !ok {
		t.Fatalf("msg = %T, want credentialsMsg", msg)
	}
	if !errors.Is(msg.err, msg.err) || msg.err == nil {
		t.Error("no credential store should be reported as an error, not silence")
	}
	m.auth.loadErr = msg.err
	if view := m.renderAuthScreen(); !strings.Contains(view, "not configured") {
		t.Errorf("auth screen does not explain itself:\n%s", view)
	}
}

func TestAuthCommandOpensTheScreen(t *testing.T) {
	t.Parallel()

	for _, input := range []string{":auth", "auth", "credentials", "login"} {
		if got := runCommand(input); got != commandAuth {
			t.Errorf("runCommand(%q) = %v, want commandAuth", input, got)
		}
	}
}
