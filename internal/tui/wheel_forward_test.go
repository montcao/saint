package tui

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

// altScreenPane starts a pane whose program takes the alternate screen and asks
// for SGR mouse reporting, which is what a real agent does - and what no other
// test in this package sets up. The scroll feature shipped broken precisely
// because every fixture was a primary-screen program.
func altScreenPane(t *testing.T, wantMouse bool) *sessionPane {
	t.Helper()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	setup := "\x1b[?1049h"
	if wantMouse {
		setup += "\x1b[?1002h\x1b[?1006h"
	}
	if _, err := pane.emulator.Write([]byte(setup)); err != nil {
		t.Fatalf("emulator.Write() error = %v", err)
	}
	if !pane.emulator.IsAltScreen() {
		t.Fatal("setup: the pane did not enter the alternate screen")
	}
	return pane
}

// sentToGuest drives a bare emulator and returns the bytes it would hand to the
// program in the pane.
//
// A live pane cannot be used for this: pumpReplies is the emulator's only
// reader by design, and a test that read alongside it would race for the bytes.
func sentToGuest(t *testing.T, setup string, send func(*vt.SafeEmulator)) string {
	t.Helper()

	em := vt.NewSafeEmulator(40, 10)
	if _, err := em.Write([]byte(setup)); err != nil {
		t.Fatalf("emulator.Write() error = %v", err)
	}

	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 128)
		n, _ := em.Read(buf)
		out <- string(buf[:n])
	}()

	send(em)
	select {
	case got := <-out:
		return got
	case <-time.After(time.Second):
		return ""
	}
}

// A wheel notch reaches a full-screen agent as a mouse report, never as a
// cursor key. An agent cannot tell a synthesised arrow from a typed one, which
// is why scrolling used to walk its prompt history.
func TestWheelReachesAFullScreenGuestAsAMouseReport(t *testing.T) {
	t.Parallel()

	got := sentToGuest(t, "\x1b[?1049h\x1b[?1002h\x1b[?1006h", func(em *vt.SafeEmulator) {
		em.SendMouse(vt.MouseWheel{X: 4, Y: 2, Button: vt.MouseWheelUp})
	})
	if !strings.HasPrefix(got, "\x1b[<") {
		t.Errorf("guest received %q, want an SGR mouse report", got)
	}
	// 64 is wheel-up in the SGR encoding. An arrow key would be "\x1b[A", and
	// that is the bug this exists to prevent.
	if !strings.Contains(got, "64;") {
		t.Errorf("guest received %q, want a wheel-up button code", got)
	}
	if strings.Contains(got, "\x1b[A") || strings.Contains(got, "\x1b[B") {
		t.Errorf("guest received a cursor key %q; that is prompt-history input, not scrolling", got)
	}
}

// A program that never asked for mouse reporting must receive nothing at all.
// Unsolicited reports would arrive as literal text on its command line.
func TestAGuestThatDidNotAskForMouseReceivesNothing(t *testing.T) {
	t.Parallel()

	got := sentToGuest(t, "\x1b[?1049h", func(em *vt.SafeEmulator) {
		em.SendMouse(vt.MouseWheel{X: 4, Y: 2, Button: vt.MouseWheelUp})
	})
	if got != "" {
		t.Errorf("a guest that requested no mouse mode received %q", got)
	}
}

// An alternate-screen pane hands the notch on; the boolean is what the model
// branches on.
func TestForwardWheelReportsWhetherTheGuestTookIt(t *testing.T) {
	t.Parallel()

	if !altScreenPane(t, true).ForwardWheel(true, 5, 3) {
		t.Error("an alternate-screen pane did not take the wheel")
	}
}

// On the primary screen output really does scroll off into saint's scrollback,
// so the notch stays with saint rather than being offered to a program that may
// not be listening.
func TestAPrimaryScreenPaneKeepsTheWheel(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	if pane.ForwardWheel(true, 1, 1) {
		t.Error("a primary-screen pane forwarded the wheel instead of scrolling itself")
	}
}

// The model must route an attached wheel through the guest, not through the
// scrollback that an alternate-screen program never fills.
func TestAttachedWheelPrefersTheGuest(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	if _, err := got.pane.emulator.Write([]byte("\x1b[?1049h\x1b[?1002h\x1b[?1006h")); err != nil {
		t.Fatalf("emulator.Write() error = %v", err)
	}

	next, _ := got.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, X: 4, Y: 10})
	if next.(Model).pane.Scrolled() {
		t.Error("the model scrolled its own empty scrollback instead of forwarding to the guest")
	}
}

// Coordinates are pane-relative and one-based, and a report for a cell the
// program does not have is worse than one for the nearest cell it does.
func TestPaneCoordinatesAreClampedInsideThePane(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40
	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	cols, rows := got.paneSize()
	for _, tc := range []struct{ x, y int }{{-50, -50}, {0, 0}, {5000, 5000}} {
		x, y := got.paneRelative(tc.x, tc.y)
		if x < 1 || x > max(1, cols) {
			t.Errorf("x for input %d = %d, outside 1..%d", tc.x, x, cols)
		}
		if y < 1 || y > max(1, rows) {
			t.Errorf("y for input %d = %d, outside 1..%d", tc.y, y, rows)
		}
	}
}
