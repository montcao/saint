package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

// fakeSandbox records which container names each lifecycle call was handed.
type fakeSandbox struct {
	attached  []string
	attachErr error
}

func (f *fakeSandbox) Create(context.Context, string, sandbox.Runtime, []sandbox.Repo, string, string, sandbox.CreateOptions) error {
	return nil
}

func (f *fakeSandbox) List(context.Context) ([]sandbox.Info, error) { return nil, nil }
func (f *fakeSandbox) Stop(context.Context, string) error           { return nil }
func (f *fakeSandbox) Start(context.Context, string) error          { return nil }
func (f *fakeSandbox) Remove(context.Context, string) error         { return nil }

func (f *fakeSandbox) AttachCmd(name string, _ sandbox.AttachOptions) (*exec.Cmd, error) {
	f.attached = append(f.attached, name)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	// cat stands in for an attached agent: it holds the PTY open and echoes
	// input back. A command that exits immediately would make every test that
	// writes to the pane race the process teardown.
	return exec.Command("/bin/cat"), nil
}

func newTestModel(t *testing.T, rows []SessionRow) (Model, *fakeSandbox) {
	t.Helper()

	sb := &fakeSandbox{}
	st := store.New(t.TempDir())
	loader := &SnapshotLoader{Store: st, Sandbox: sb, Inspector: NewRepoInspector("/usr/bin/git", nil)}
	svc := &session.Service{Store: st, Sandbox: sb}

	m := NewModel(loader, svc)
	m.rows = rows
	return m, sb
}

func TestEnterAttachesOnlyToRunningSessions(t *testing.T) {
	t.Parallel()

	rows := []SessionRow{
		{ID: "run1234", Name: "running-one", State: "running"},
		{ID: "stop567", Name: "stopped-one", State: "stopped"},
		{ID: "miss890", Name: "missing-one", State: "missing"},
	}

	tests := []struct {
		name       string
		selected   int
		wantAttach []string
	}{
		{name: "running attaches", selected: 0, wantAttach: []string{"saint-run1234"}},
		{name: "stopped does not attach", selected: 1},
		{name: "missing does not attach", selected: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, sb := newTestModel(t, rows)
			m.selected = tc.selected

			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			got, ok := updated.(Model)
			if !ok {
				t.Fatalf("Update() returned %T", updated)
			}
			t.Cleanup(got.closePane)

			if len(sb.attached) != len(tc.wantAttach) {
				t.Fatalf("attached = %#v, want %#v", sb.attached, tc.wantAttach)
			}
			for i := range tc.wantAttach {
				if sb.attached[i] != tc.wantAttach[i] {
					t.Fatalf("attached[%d] = %q, want %q", i, sb.attached[i], tc.wantAttach[i])
				}
			}
			if got.status == "" {
				t.Fatal("status = empty, want an explanatory line")
			}
		})
	}
}

func TestEnterOnMissingSessionExplainsInsteadOfActing(t *testing.T) {
	t.Parallel()

	m, sb := newTestModel(t, []SessionRow{{ID: "miss890", Name: "gone", State: "missing"}})

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if len(sb.attached) != 0 {
		t.Fatalf("attached = %#v, want none", sb.attached)
	}
	if cmd != nil {
		t.Fatal("cmd != nil, want no action for a missing container")
	}
	if !strings.Contains(got.status, "missing") {
		t.Fatalf("status = %q, want an explanation", got.status)
	}
}

func TestAttachFailureKeepsTUIAliveWithReadableStatus(t *testing.T) {
	t.Parallel()

	m, sb := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	sb.attachErr = errors.New("container: \x1b[31mfailed\x1b[0m\nstderr line two\n" + strings.Repeat("x", 400))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if cmd == nil {
		t.Fatal("cmd = nil, want a refresh so the TUI stays live")
	}
	if got.mode != modeList {
		t.Fatalf("mode = %v, want the list to stay in control", got.mode)
	}
	if got.pane != nil {
		t.Fatal("a failed attach must not leave a pane behind")
	}
	if !strings.HasPrefix(got.status, "attach failed: ") {
		t.Fatalf("status = %q", got.status)
	}
	if strings.ContainsAny(got.status, "\n\r\x1b") {
		t.Fatalf("status kept control characters: %q", got.status)
	}
	if len(got.status) > 160 {
		t.Fatalf("status not bounded: %d chars", len(got.status))
	}
}

// Attaching must embed the session rather than handing over the whole terminal:
// the model stays in modeAttached with a live pane, and the list is still there.
func TestAttachEmbedsSessionInsteadOfTakingOverTerminal(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	t.Cleanup(got.closePane)

	if got.mode != modeAttached {
		t.Fatalf("mode = %v, want modeAttached", got.mode)
	}
	if got.pane == nil {
		t.Fatal("pane = nil, want an embedded session")
	}
	if cmd == nil {
		t.Fatal("cmd = nil, want a pane event wait")
	}
	if got.pane.name != "one" {
		t.Fatalf("pane name = %q, want the session name", got.pane.name)
	}

	view := got.View()
	if !strings.Contains(view, "session: one") {
		t.Fatal("view is missing the pane title")
	}
	if !strings.Contains(view, "SAINT") {
		t.Fatal("view lost saint's own chrome; the session must be embedded, not full screen")
	}
}

// Detaching leaves the session running and returns control to the list.
func TestDetachKeyReturnsToListWithoutQuitting(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	if got.pane == nil {
		t.Fatal("setup: no pane")
	}

	detached, cmd := got.Update(tea.KeyMsg{Type: tea.KeyCtrlBackslash})
	after := detached.(Model)
	t.Cleanup(after.closePane)

	if after.mode != modeList {
		t.Fatalf("mode = %v, want modeList", after.mode)
	}
	if after.pane != nil {
		t.Fatal("pane still attached after detach")
	}
	if cmd == nil {
		t.Fatal("cmd = nil, want a refresh after detaching")
	}
	if !strings.Contains(after.status, "still running") {
		t.Fatalf("status = %q, want it to say the session survives", after.status)
	}
}

// While attached, keys belong to the agent - including the ones that would
// otherwise drive saint.
func TestAttachedKeysGoToTheSessionNotTheTUI(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyRunes, Runes: []rune{':'}},
		{Type: tea.KeyEsc},
	} {
		next, cmd := got.Update(key)
		after := next.(Model)
		if after.mode != modeAttached {
			t.Fatalf("key %v changed mode to %v; it must reach the session", key, after.mode)
		}
		if cmd != nil {
			t.Fatalf("key %v produced a saint command; it must reach the session", key)
		}
		got = after
	}
}

func TestSnapshotLoaderAttachCmdValidatesSessionID(t *testing.T) {
	t.Parallel()

	sb := &fakeSandbox{}
	loader := &SnapshotLoader{Store: store.New(t.TempDir()), Sandbox: sb}

	for _, id := range []string{"", "--privileged", "../escape", "UPPER", "with space"} {
		if _, err := loader.AttachCmd(id); err == nil {
			t.Fatalf("AttachCmd(%q) error = nil, want non-nil", id)
		}
	}
	if len(sb.attached) != 0 {
		t.Fatalf("an invalid id reached the sandbox: %#v", sb.attached)
	}

	cmd, err := loader.AttachCmd("abc123")
	if err != nil {
		t.Fatalf("AttachCmd() error = %v", err)
	}
	if cmd == nil {
		t.Fatal("cmd = nil")
	}
	if len(sb.attached) != 1 || sb.attached[0] != "saint-abc123" {
		t.Fatalf("attached = %#v", sb.attached)
	}
}

// PgUp/PgDn scroll saint's own view of the pane and must never be forwarded
// to the agent - the agent must not receive a keystroke that means something
// different to saint's own overlay.
func TestPgUpPgDownScrollWithoutReachingTheSession(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	fillPaneScrollback(t, got.pane)

	if got.pane.Scrolled() {
		t.Fatal("setup: pane already scrolled")
	}

	next, cmd := got.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	after := next.(Model)
	if after.mode != modeAttached {
		t.Fatalf("mode = %v, want modeAttached", after.mode)
	}
	if cmd != nil {
		t.Fatal("PgUp produced a saint command; it must be handled locally, not forwarded")
	}
	if !after.pane.Scrolled() {
		t.Fatal("PgUp did not scroll the pane")
	}

	next, cmd = after.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	after = next.(Model)
	if cmd != nil {
		t.Fatal("PgDown produced a saint command; it must be handled locally, not forwarded")
	}
	if after.pane.Scrolled() {
		t.Fatal("PgDown did not return the pane to live")
	}
}

// Any ordinary key pressed while scrolled means the user is typing to the
// agent again: the view must snap back to live, and the keystroke must still
// be delivered - scrollback is a passive overlay, not a mode to exit first.
func TestOrdinaryKeySnapsBackToLiveAndStillForwards(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	fillPaneScrollback(t, got.pane)

	next, _ := got.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	got = next.(Model)
	if !got.pane.Scrolled() {
		t.Fatal("setup: PgUp did not scroll the pane")
	}

	next, cmd := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	after := next.(Model)
	if after.mode != modeAttached {
		t.Fatalf("mode = %v, want modeAttached", after.mode)
	}
	if cmd != nil {
		t.Fatal("ordinary key produced a saint command; it must still reach the session")
	}
	if after.pane.Scrolled() {
		t.Fatal("typing while scrolled did not snap the view back to live")
	}
}

// Mouse wheel events scroll the pane the same way PgUp/PgDn do, and only
// while a session is attached - elsewhere a wheel event must no-op rather
// than dereference a pane that does not exist.
func TestMouseWheelScrollsOnlyWhileAttached(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	next, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	after := next.(Model)
	if after.mode == modeAttached {
		t.Fatal("setup: model unexpectedly attached")
	}
	if cmd != nil {
		t.Fatal("wheel event outside an attached session produced a command")
	}

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	fillPaneScrollback(t, got.pane)

	next, _ = got.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	after = next.(Model)
	if !after.pane.Scrolled() {
		t.Fatal("wheel up did not scroll the pane")
	}

	next, _ = after.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	after = next.(Model)
	if after.pane.Scrolled() {
		t.Fatal("wheel down did not return the pane to live")
	}
}

func TestKeyMapAdvertisesAttach(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, nil)
	found := false
	for _, binding := range m.keys.ShortHelp() {
		if binding.Help().Key == "enter" {
			found = true
		}
	}
	if !found {
		t.Fatal("enter binding missing from the help footer")
	}
}

// Shift+arrow scrolls saint's own view and is never forwarded.
//
// The wheel cannot be relied on here: a terminal in the alternate screen may
// translate a notch into a bare arrow key rather than a mouse event, and saint
// then forwards an ordinary "up" to the agent, which walks its prompt history.
// saint cannot distinguish that from someone holding the arrow key. A shifted
// arrow is unambiguous, because no terminal synthesises one for a wheel notch.
func TestShiftArrowScrollsAndIsNotForwarded(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)
	fillPaneScrollback(t, got.pane)

	// The pane is shared by pointer across these model copies, so the sequence
	// has to be linear: probing with a second key would move the same pane.
	next, cmd := got.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	after := next.(Model)
	if !after.pane.Scrolled() {
		t.Fatal("shift+up did not scroll the pane")
	}
	if cmd != nil {
		t.Error("shift+up produced a command; it must be handled entirely by saint")
	}

	// One line up, one line down, back to live. That is the movement PgUp and
	// PgDn cannot make.
	next, cmd = after.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	back := next.(Model)
	if back.pane.Scrolled() {
		t.Error("shift+down did not undo a single-line shift+up")
	}
	if cmd != nil {
		t.Error("shift+down produced a command")
	}
}

// The agent must never see a shifted arrow, or scrolling would also type.
func TestShiftArrowReachesTheAgentAsNothing(t *testing.T) {
	t.Parallel()

	for _, key := range []tea.KeyType{tea.KeyShiftUp, tea.KeyShiftDown} {
		if got := encodeKey(tea.KeyMsg{Type: key}); len(got) != 0 {
			t.Errorf("encodeKey(%v) = %q, want nothing: saint owns this key", key, got)
		}
	}
}
