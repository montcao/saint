package tui

import (
	"os/exec"

	"github.com/montcao/saint/internal/sandbox"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// wedgedSandbox stands in for an agent that has stopped responding - the exact
// state a session reaches when its API calls hang (for example when the macOS
// firewall silently drops packets to the proxy instead of refusing them).
type wedgedSandbox struct{ fakeSandbox }

func (w *wedgedSandbox) AttachCmd(name string, _ sandbox.AttachOptions) (*exec.Cmd, error) {
	w.attached = append(w.attached, name)
	return exec.Command("/bin/sh", "-c", "trap '' HUP INT TERM; while :; do sleep 1; done"), nil
}

func attachedToWedgedAgent(t *testing.T) Model {
	t.Helper()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.loader.Sandbox = &wedgedSandbox{}
	m.service.Sandbox = m.loader.Sandbox
	m.width, m.height = 100, 40

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.mode != modeAttached || got.pane == nil {
		t.Fatalf("setup: mode=%v pane=%v", got.mode, got.pane)
	}
	t.Cleanup(got.closePane)
	return got
}

// The core regression: a user must always be able to leave. Every ordinary key
// is forwarded to the agent, so if the agent is wedged these are the only ways
// out - and they must work without it.
func TestForceQuitEscapesAWedgedAgent(t *testing.T) {
	m := attachedToWedgedAgent(t)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	got := updated.(Model)

	if cmd == nil {
		t.Fatal("ctrl+q produced no command; want tea.Quit")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("ctrl+q produced no message")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("msg = %T, want tea.QuitMsg", msg)
	}
	if got.pane != nil {
		t.Error("force quit left the pane attached; the exec client would be orphaned")
	}
}

func TestDetachEscapesAWedgedAgent(t *testing.T) {
	m := attachedToWedgedAgent(t)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlBackslash})
	got := updated.(Model)

	if got.mode != modeList {
		t.Fatalf("mode = %v, want modeList", got.mode)
	}
	if got.pane != nil {
		t.Fatal("pane still attached after detach")
	}
}

// Quitting from the list must also tear down a pane, so no `container exec`
// client is left behind.
func TestQuitFromListClosesAnyPane(t *testing.T) {
	m := attachedToWedgedAgent(t)
	m.mode = modeList // as if the pane outlived a mode change

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	got := updated.(Model)

	if cmd == nil {
		t.Fatal("q produced no command")
	}
	if got.pane != nil {
		t.Error("quit left a pane behind")
	}
}

// When the session dies on its own, the model must leave attached mode too.
// Otherwise the pane is gone but keys still route to updateAttached and the
// next keystroke is silently swallowed.
func TestSessionEndingLeavesAttachedMode(t *testing.T) {
	m := attachedToWedgedAgent(t)
	id := m.pane.id

	updated, _ := m.Update(paneExitedMsg{id: id})
	got := updated.(Model)

	if got.mode != modeAttached {
		// expected: it should have left attached mode
	}
	if got.mode != modeList {
		t.Fatalf("mode = %v, want modeList after the session ended", got.mode)
	}
	if got.pane != nil {
		t.Fatal("pane not cleared")
	}
}

// Both escapes must be visible while attached; they are the only way out.
func TestPaneAdvertisesBothEscapes(t *testing.T) {
	m := attachedToWedgedAgent(t)

	view := m.View()
	if !strings.Contains(view, "ctrl+\\ detach") {
		t.Error("pane does not show the detach key")
	}
	if !strings.Contains(view, "ctrl+q quit") {
		t.Error("pane does not show the force-quit key")
	}
}

// A short terminal must not produce a view taller than the screen: an
// overflowing alt-screen render garbles into something that reads as frozen.
func TestAttachedLayoutFitsShortTerminals(t *testing.T) {
	for _, height := range []int{18, 20, 24, 30, 40, 60} {
		m := attachedToWedgedAgent(t)
		m.height, m.width = height, 100

		listRows, paneRows := m.attachedSplit()
		if listRows < 1 {
			t.Fatalf("height=%d listRows=%d, want at least 1", height, listRows)
		}
		if paneRows < minPaneRows {
			t.Fatalf("height=%d paneRows=%d, want at least %d", height, paneRows, minPaneRows)
		}
		if total := attachedChrome + listRows + paneRows; height >= 24 && total > height {
			t.Fatalf("height=%d total=%d overflows the screen", height, total)
		}
	}
}

// The escape keys moved out of the pane's title bar and into the header's
// command row. That row is saint's own chrome and keeps drawing whatever the
// agent inside the pane is doing, which is the case the escapes exist for - so
// it has to actually carry them.
func TestAttachedHeaderAlwaysShowsBothEscapes(t *testing.T) {
	t.Parallel()

	for _, width := range []int{80, 120, 200} {
		m := newRenderModel(t, width, 30)
		m.mode = modeAttached

		header := m.renderHeader()
		for _, want := range []string{"detach", "ctrl+q"} {
			if !strings.Contains(header, want) {
				t.Errorf("attached header at width %d is missing %q:\n%s", width, want, header)
			}
		}
	}
}
