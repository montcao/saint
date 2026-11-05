package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// orphanSandbox reports one session container and one orphan.
type orphanSandbox struct {
	fakeSandbox
	removed []string
}

func (o *orphanSandbox) List(context.Context) ([]sandbox.Info, error) {
	return []sandbox.Info{
		{Name: "saint-abc12345", State: sandbox.StateRunning, Runtime: sandbox.RuntimeClaude},
		{Name: "saint-leftover", State: sandbox.StateStopped, Runtime: sandbox.RuntimeCodex},
		{Name: "unrelated-thing", State: sandbox.StateRunning},
	}, nil
}

func (o *orphanSandbox) Remove(_ context.Context, name string) error {
	o.removed = append(o.removed, name)
	return nil
}

// An orphan used to be a number in the header with no way to act on it.
func TestOrphanContainersAppearAsRows(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if err := st.SaveSessions([]store.Session{{ID: "abc12345", Name: "real", Runtime: sandbox.RuntimeClaude}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	loader := &SnapshotLoader{Store: st, Sandbox: &orphanSandbox{}, Inspector: NewRepoInspector("/usr/bin/git", nil)}
	snapshot, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(snapshot.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one session, one orphan)", len(snapshot.Rows))
	}
	// Sessions first, debris last.
	if snapshot.Rows[0].Orphan {
		t.Error("the real session was marked as an orphan")
	}
	orphan := snapshot.Rows[1]
	if !orphan.Orphan {
		t.Fatalf("row = %+v, want it marked as an orphan", orphan)
	}
	if orphan.Container != "saint-leftover" {
		t.Errorf("Container = %q, want saint-leftover", orphan.Container)
	}
	if !strings.Contains(orphan.State, "orphan") {
		t.Errorf("State = %q, want it to say orphan", orphan.State)
	}
	// A container that is not saint's is nobody's business.
	for _, row := range snapshot.Rows {
		if row.Container == "unrelated-thing" {
			t.Error("a container saint does not own was listed")
		}
	}
}

func TestDeletingAnOrphanRemovesTheContainer(t *testing.T) {
	t.Parallel()

	sb := &orphanSandbox{}
	st := store.New(t.TempDir())
	loader := &SnapshotLoader{Store: st, Sandbox: sb, Inspector: NewRepoInspector("/usr/bin/git", nil)}

	m := NewModel(loader, nil)
	m.width, m.height = 120, 40
	m.rows = []SessionRow{{
		ID: "leftover", Name: "saint-leftover", State: "orphan",
		Orphan: true, Container: "saint-leftover",
	}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(Model)
	if m.mode != modeConfirmDelete {
		t.Fatalf("mode = %v, want the confirm dialog", m.mode)
	}
	if m.deleteContainer != "saint-leftover" {
		t.Fatalf("deleteContainer = %q, want saint-leftover", m.deleteContainer)
	}

	// The dialog has to say it is removing a container, not deleting a session
	// record that does not exist.
	dialog := m.renderDeleteDialog()
	if !strings.Contains(dialog, "Remove Container") || !strings.Contains(dialog, "saint-leftover") {
		t.Errorf("dialog does not describe an orphan removal:\n%s", dialog)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("confirming produced no command")
	}
	action, ok := findSessionAction(cmd)
	if !ok {
		t.Fatal("no session action came back from confirming the delete")
	}
	if action.err != nil {
		t.Fatalf("remove reported %v", action.err)
	}
	if len(sb.removed) != 1 || sb.removed[0] != "saint-leftover" {
		t.Errorf("removed = %v, want [saint-leftover]", sb.removed)
	}
}

// An orphan has no session record, so attaching or starting it is meaningless.
// Saying so beats a confusing failure deeper down.
func TestEnterOnAnOrphanExplainsItself(t *testing.T) {
	t.Parallel()

	sb := &orphanSandbox{}
	loader := &SnapshotLoader{Store: store.New(t.TempDir()), Sandbox: sb, Inspector: NewRepoInspector("/usr/bin/git", nil)}

	m := NewModel(loader, nil)
	m.width, m.height = 120, 40
	m.rows = []SessionRow{{
		ID: "leftover", Name: "saint-leftover", State: "orphan (running)",
		Orphan: true, Container: "saint-leftover",
	}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode == modeAttached {
		t.Fatal("saint tried to attach to a container with no session record")
	}
	if !strings.Contains(m.status, "orphan") {
		t.Errorf("status = %q, want it to explain the orphan", m.status)
	}
}

// Cancelling must not leave a target armed for the next confirm dialog.
func TestCancellingDeleteClearsTheTarget(t *testing.T) {
	t.Parallel()

	loader := &SnapshotLoader{Store: store.New(t.TempDir()), Sandbox: &orphanSandbox{}, Inspector: NewRepoInspector("/usr/bin/git", nil)}
	m := NewModel(loader, nil)
	m.width, m.height = 120, 40
	m.rows = []SessionRow{{ID: "leftover", Name: "saint-leftover", Orphan: true, Container: "saint-leftover"}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)

	if m.deleteContainer != "" || m.deleteID != "" {
		t.Errorf("delete target survived cancellation: id=%q container=%q", m.deleteID, m.deleteContainer)
	}
	if m.mode != modeList {
		t.Errorf("mode = %v, want the list", m.mode)
	}
}

// findSessionAction runs a command and looks for a session action in the
// result, descending one level into a batch.
//
// Actions are batched with the busy animation, so the interesting message is no
// longer the only one that comes back.
func findSessionAction(cmd tea.Cmd) (sessionActionMsg, bool) {
	if cmd == nil {
		return sessionActionMsg{}, false
	}
	switch msg := cmd().(type) {
	case sessionActionMsg:
		return msg, true
	case tea.BatchMsg:
		for _, inner := range msg {
			if action, ok := findSessionAction(inner); ok {
				return action, true
			}
		}
	}
	return sessionActionMsg{}, false
}
