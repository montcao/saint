package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

func stateFor(config string, present bool, expires time.Time, err error) CredentialState {
	return CredentialState{
		Kind:      CredentialKind{Name: config, Config: config},
		Present:   present,
		ExpiresAt: expires,
		Err:       err,
	}
}

// The host's login is read at session creation, so a session gets whatever is
// left rather than a full lifetime. That has to be visible before the user
// commits, not discovered when the session dies.
func TestCredentialRowShowsRemainingLife(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	f.applyCredentialStates([]CredentialState{
		stateFor(store.CredentialClaudeCode, true, now.Add(6*time.Hour+25*time.Minute), nil),
	}, now)

	view := f.View(90)
	if !strings.Contains(view, "6h 25m left") {
		t.Errorf("credential row does not show remaining life:\n%s", view)
	}
}

// Below the floor the session would be stranded mid-run, so the row says so
// rather than letting the user pick it and get a warning afterwards.
func TestCredentialRowFlagsTooLittleLife(t *testing.T) {
	t.Parallel()

	now := time.Now()
	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	f.applyCredentialStates([]CredentialState{
		stateFor(store.CredentialClaudeCode, true, now.Add(session.MinCredentialLife-time.Minute), nil),
	}, now)

	if view := f.View(90); !strings.Contains(view, "too little left") {
		t.Errorf("credential row does not flag a credential under the floor:\n%s", view)
	}
}

func TestCredentialRowReportsMissingAndBroken(t *testing.T) {
	t.Parallel()

	now := time.Now()
	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	f.applyCredentialStates([]CredentialState{
		stateFor(store.CredentialClaudeCode, false, time.Time{}, nil),
		stateFor(store.CredentialClaudeSubscription, false, time.Time{}, errors.New("keychain is locked")),
	}, now)

	view := f.View(90)
	if !strings.Contains(view, "not set") {
		t.Errorf("row does not report a missing credential:\n%s", view)
	}
	if !strings.Contains(view, "keychain is locked") {
		t.Errorf("row does not report why a credential is unusable:\n%s", view)
	}
}

// A stored token has no expiry saint can see. Saying nothing is correct;
// implying it lasts forever is not.
func TestCredentialRowSaysNothingWhenExpiryIsUnknown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	f.applyCredentialStates([]CredentialState{
		stateFor(store.CredentialClaudeSubscription, true, time.Time{}, nil),
	}, now)

	view := f.View(90)
	if strings.Contains(view, "left)") && strings.Contains(view, "stored token (") {
		t.Errorf("row invented a lifetime for a credential with no known expiry:\n%s", view)
	}
}

func TestCredentialRowCyclesWithArrows(t *testing.T) {
	t.Parallel()

	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	for range formRowCredential {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if f.focus != formRowCredential {
		t.Fatalf("focus = %d, want the credential row", f.focus)
	}

	first := f.selectedCredential().Spec
	f.Update(tea.KeyMsg{Type: tea.KeyRight})
	if f.selectedCredential().Spec == first {
		t.Error("right did not change the credential")
	}
	f.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if got := f.selectedCredential().Spec; got != first {
		t.Errorf("left did not return to %q, got %q", first, got)
	}
	if req := f.Request(); req.Credential != first {
		t.Errorf("Request().Credential = %q, want %q", req.Credential, first)
	}
}

// Per-session choice must not become a per-session question.
func TestCredentialRowStartsOnTheRememberedChoice(t *testing.T) {
	t.Parallel()

	f := newSessionDialogWith("", sandbox.RuntimeClaude, store.CredentialAPIKey)
	if got := f.selectedCredential().Spec; got != store.CredentialAPIKey {
		t.Errorf("selected = %q, want the remembered choice", got)
	}
}

// The shortest-lived option is first, because a shorter-lived credential in a
// guest is the safer default.
func TestShortestLivedCredentialIsFirst(t *testing.T) {
	t.Parallel()

	if credentialOptions[0].Spec != store.CredentialClaudeCode {
		t.Errorf("first option = %q, want the live login", credentialOptions[0].Spec)
	}
}

func TestFormatLife(t *testing.T) {
	t.Parallel()

	cases := map[time.Duration]string{
		0:                            "expired",
		-time.Minute:                 "expired",
		45 * time.Second:             "0m",
		25 * time.Minute:             "25m",
		time.Hour:                    "1h",
		6*time.Hour + 25*time.Minute: "6h 25m",
		2*time.Hour + 30*time.Second: "2h",
	}
	for in, want := range cases {
		if got := formatLife(in); got != want {
			t.Errorf("formatLife(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialLifeLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)

	// No known expiry: say nothing rather than imply permanence.
	if got := credentialLifeLine(SessionRow{}, now); got != "" {
		t.Errorf("credentialLifeLine() = %q, want empty for an unknown expiry", got)
	}
	if got := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(4 * time.Hour)}, now); !strings.Contains(got, "4h left") {
		t.Errorf("credentialLifeLine() = %q, want the remaining life", got)
	}
	// The remedy used to be "recreate soon", which asked the user to rebuild the
	// session by hand. saint does it now, so the line names the key instead.
	if got := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(10 * time.Minute)}, now); !strings.Contains(got, "renews") {
		t.Errorf("credentialLifeLine() = %q, want a warning naming the remedy", got)
	}
	if got := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(-time.Minute)}, now); !strings.Contains(got, "expired") {
		t.Errorf("credentialLifeLine() = %q, want an expired notice", got)
	}
}
