package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

func at(s int) time.Time {
	return time.Date(2026, 9, 14, 12, 0, s, 0, time.UTC)
}

func row(id, state, activity string) SessionRow {
	return SessionRow{ID: id, Name: id, Runtime: "claude", State: state, Activity: activity}
}

// Everything is new when saint starts. A screen where every row is marked is a
// screen with no marks on it.
func TestFirstSnapshotMarksNothing(t *testing.T) {
	t.Parallel()

	seen, marks := noteChanges(nil, nil, []SessionRow{
		row("a", "running", "working"), row("b", "running", "idle"),
	}, at(0))

	if len(marks) != 0 {
		t.Errorf("first snapshot produced %d marks, want none: %v", len(marks), marks)
	}
	if len(seen) != 2 {
		t.Errorf("first snapshot remembered %d sessions, want 2", len(seen))
	}
}

func TestNoteChangesMarksTheTransitionsWorthNoticing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		from SessionRow
		to   SessionRow
		want changeKind
	}{
		{"an agent starts thinking", row("a", "running", "idle"), row("a", "running", "working"), changeStarted},
		// The one a person actually waits for.
		{"an agent finishes", row("a", "running", "working"), row("a", "running", "idle"), changeFinished},
		{"a session stops", row("a", "running", "working"), row("a", "stopped", "-"), changeStopped},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seen, _ := noteChanges(nil, nil, []SessionRow{tc.from}, at(0))
			_, marks := noteChanges(seen, nil, []SessionRow{tc.to}, at(1))

			mark, ok := marks[tc.to.ID]
			if !ok {
				t.Fatalf("no mark for %s", tc.name)
			}
			if mark.Kind != tc.want {
				t.Errorf("mark = %q, want %q", mark.Kind, tc.want)
			}
		})
	}
}

// A session that was not in the previous fleet is new - but only once saint has
// a previous fleet to compare against.
func TestNewSessionIsMarked(t *testing.T) {
	t.Parallel()

	seen, _ := noteChanges(nil, nil, []SessionRow{row("a", "running", "idle")}, at(0))
	_, marks := noteChanges(seen, nil, []SessionRow{
		row("a", "running", "idle"), row("b", "running", "idle"),
	}, at(1))

	if marks["b"].Kind != changeAppeared {
		t.Errorf("new session mark = %q, want %q", marks["b"].Kind, changeAppeared)
	}
	if _, ok := marks["a"]; ok {
		t.Error("an unchanged session was marked")
	}
}

// Nothing changing must not clear a mark nobody has looked at yet: the whole
// point is that it survives until it is seen.
func TestAnUnseenMarkSurvivesAQuietTick(t *testing.T) {
	t.Parallel()

	seen, _ := noteChanges(nil, nil, []SessionRow{row("a", "running", "working")}, at(0))
	_, marks := noteChanges(seen, nil, []SessionRow{row("a", "running", "idle")}, at(1))
	seen, marks = noteChanges(seen, marks, []SessionRow{row("a", "running", "idle")}, at(2))

	if _, ok := marks["a"]; !ok {
		t.Error("a mark nobody has seen was dropped by an uneventful refresh")
	}

	// But coming back after lunch must not present an hour of history as news.
	_, marks = noteChanges(seen, marks, []SessionRow{row("a", "running", "idle")},
		at(2).Add(markTTL+time.Second))
	if _, ok := marks["a"]; ok {
		t.Error("a mark outlived its window")
	}
}

// A mark belongs to a row. A session that is gone cannot wear one.
func TestMarkIsDroppedWhenTheSessionGoes(t *testing.T) {
	t.Parallel()

	seen, _ := noteChanges(nil, nil, []SessionRow{row("a", "running", "working")}, at(0))
	seen, marks := noteChanges(seen, nil, []SessionRow{row("a", "running", "idle")}, at(1))
	if _, ok := marks["a"]; !ok {
		t.Fatal("setup: expected a mark")
	}

	_, marks = noteChanges(seen, marks, []SessionRow{}, at(2))
	if _, ok := marks["a"]; ok {
		t.Error("a mark survived the session it described")
	}
}

// A keypress is the moment saint can honestly infer somebody is looking, and a
// mark's job ends when it has been looked at.
func TestKeypressClearsMarks(t *testing.T) {
	t.Parallel()

	m := blockModel(t, []SessionRow{row("a", "running", "idle")})
	m.marks = map[string]changeMark{"a": {Kind: changeFinished, At: at(0)}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := len(next.(Model).marks); got != 0 {
		t.Errorf("%d marks survived a keypress, want 0", got)
	}
}

// Collapsing is what keeps a fleet of twelve readable. Three reasons to stay
// open and no others.
func TestOnlyAttentionWorthySessionsExpand(t *testing.T) {
	t.Parallel()

	m := blockModel(t, nil)
	working := row("w", "running", "working")
	quiet := row("q", "running", "idle")
	changed := row("c", "running", "idle")
	m.marks = map[string]changeMark{"c": {Kind: changeFinished, At: at(0)}}

	if !m.blockExpanded(working, false) {
		t.Error("a working session collapsed")
	}
	if !m.blockExpanded(changed, false) {
		t.Error("a session that changed and has not been seen collapsed")
	}
	if !m.blockExpanded(quiet, true) {
		t.Error("the selected session collapsed")
	}
	if m.blockExpanded(quiet, false) {
		t.Error("a quiet, unselected, unchanged session stayed open")
	}
}

// The mark has to say what it means. A marker with no word is a flicker.
func TestAMarkedSessionSaysWhatChanged(t *testing.T) {
	t.Parallel()

	m := blockModel(t, []SessionRow{row("gamma", "running", "idle")})
	m.selected = -1
	m.marks = map[string]changeMark{"gamma": {Kind: changeFinished, At: at(0)}}

	got := plain(m.renderTable())
	if !strings.Contains(got, string(changeFinished)) {
		t.Errorf("a marked session does not name the change:\n%s", got)
	}
}

// "idle 40m" is a measurement. Without one, the line says "idle" and stops -
// LastRun is when the session started, not when its agent last did anything.
func TestCollapsedLineNeverInventsAnIdleDuration(t *testing.T) {
	t.Parallel()

	now := at(0)
	m := blockModel(t, nil)
	m.now = func() time.Time { return now }

	measured := row("a", "running", "idle")
	measured.UsageKnown = true
	measured.Usage = usage.Session{Reported: true, LastAt: now.Add(-40 * time.Minute)}
	if got := m.collapsedLine(measured); !strings.Contains(got, "idle 40m") {
		t.Errorf("collapsedLine() = %q, want a measured idle duration", got)
	}

	unmeasured := row("b", "running", "idle")
	unmeasured.LastRun = "3h ago"
	got := m.collapsedLine(unmeasured)
	if !strings.Contains(got, "idle") {
		t.Errorf("collapsedLine() = %q, want it to still say idle", got)
	}
	if strings.Contains(got, "3h") {
		t.Errorf("collapsedLine() = %q: LastRun is when the session started, not how long it has been quiet", got)
	}
}

// Blocks are now variable height, so the visible range has to count lines. A
// fixed divisor either overflows the column or leaves a third of it empty.
func TestVisibleRangeCountsLinesNotSessions(t *testing.T) {
	t.Parallel()

	rows := make([]SessionRow, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, row(string(rune('a'+i)), "running", "idle"))
	}
	rows[0].Activity = "working"

	m := blockModel(t, rows)
	m.selected = -1
	m.height = 30

	lines := strings.Split(strings.TrimRight(plain(m.renderTable()), "\n"), "\n")
	if len(lines) > m.listHeight()+2 {
		t.Errorf("rendered %d lines into a %d-line column", len(lines), m.listHeight())
	}
	// One expanded session and eleven collapsed ones must all fit; a divisor of
	// three would have shown four.
	if !strings.Contains(plain(m.renderTable()), "l") {
		t.Errorf("the last session was dropped:\n%s", plain(m.renderTable()))
	}
}

func TestListStyleConstantsAreStable(t *testing.T) {
	t.Parallel()

	if store.ListStyleBlocks == store.ListStyleTable {
		t.Fatal("the two list styles must be distinguishable")
	}
}
