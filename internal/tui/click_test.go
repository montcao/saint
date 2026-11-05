package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

func clickModel(t *testing.T, style string, rows []SessionRow) Model {
	t.Helper()

	st := store.New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg, _ := st.EnsureConfig()
	cfg.ListStyle = style
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	sb := &fakeSandbox{}
	m := NewModel(&SnapshotLoader{
		Store: st, Sandbox: sb, Inspector: NewRepoInspector("/usr/bin/git", nil),
	}, &session.Service{Store: st, Sandbox: sb})
	m.width, m.height = 150, 40
	m.rows = rows
	m.proxyStatus = "proxy: 192.0.2.1:8787 · subscription"
	return m
}

func clickRows(n int) []SessionRow {
	rows := make([]SessionRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, SessionRow{
			ID: string(rune('a' + i)), Name: "session-" + string(rune('a'+i)),
			Runtime: "claude", State: "running", Activity: "idle", LastRun: "1m ago",
			RepoDetails: []RepoDetail{{Name: "repo", Branch: "main", Active: true}},
		})
	}
	return rows
}

// The click must land on the row the user is looking at. The hit test derives
// its geometry from the same renderers the view uses, and this asserts the two
// actually agree rather than trusting that they do.
func TestClickingARowSelectsTheRowUnderThePointer(t *testing.T) {
	t.Parallel()

	for _, style := range []string{store.ListStyleTable, store.ListStyleBlocks} {
		t.Run(style, func(t *testing.T) {
			t.Parallel()

			m := clickModel(t, style, clickRows(4))
			for want := 0; want < 4; want++ {
				y := rowLine(t, m, want)
				got := m.hitTest(2, y)
				if got.Zone != clickSessionRow {
					t.Fatalf("row %d at y=%d: zone = %v, want a session row", want, y, got.Zone)
				}
				if got.Index != want {
					t.Errorf("row %d at y=%d resolved to row %d", want, y, got.Index)
				}
			}
		})
	}
}

// rowLine finds the terminal line a session's name is actually drawn on, by
// reading the rendered frame rather than recomputing the layout.
func rowLine(t *testing.T, m Model, idx int) int {
	t.Helper()

	// Only the main column counts. The sidebar shows the selected session's name
	// on its own lines, so searching the whole row finds that instead and the
	// helper silently measures the wrong thing.
	name := m.filteredRows()[idx].Name
	layout := m.layout()
	for y, line := range strings.Split(stripEscapes(m.View()), "\n") {
		runes := []rune(line)
		from, to := layout.left, layout.left+layout.main
		if from > len(runes) {
			continue
		}
		if to > len(runes) {
			to = len(runes)
		}
		if strings.Contains(string(runes[from:to]), name) {
			return y
		}
	}
	t.Fatalf("session %q is not on screen", name)
	return -1
}

// A press selects; a second press on the same row opens it. Moving is free and
// entering is the deliberate act, which is the division the keyboard already
// uses.
func TestASecondClickOnTheSameRowAttaches(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	y := rowLine(t, m, 2)
	press := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: y}

	next, cmd := m.Update(press)
	after := next.(Model)
	if after.selected != 2 {
		t.Fatalf("selected = %d, want 2", after.selected)
	}
	if cmd != nil {
		t.Error("a single click did more than select")
	}

	next, cmd = after.Update(press)
	if cmd == nil {
		t.Error("a second click on the same row did nothing; it should do what enter does")
	}
	attached := next.(Model)
	attached.closePane()
}

// Two presses far apart in time are two decisions, not one gesture.
func TestASlowSecondClickIsNotADoubleClick(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	y := rowLine(t, m, 1)
	press := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: y}

	next, _ := m.Update(press)
	after := next.(Model)
	after.now = func() time.Time { return now.Add(doubleClickWithin + time.Second) }

	if _, cmd := after.Update(press); cmd != nil {
		t.Error("two clicks a second apart were treated as one gesture")
	}
}

// Clicking a different row is a new selection, never an open.
func TestClickingADifferentRowNeverOpensIt(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	first := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: rowLine(t, m, 0)}
	second := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: rowLine(t, m, 1)}

	next, _ := m.Update(first)
	if _, cmd := next.(Model).Update(second); cmd != nil {
		t.Error("clicking a second, different row opened it")
	}
}

// The chrome is not a row. Clicking the column header or the rule under it used
// to be an off-by-two away from selecting the wrong session.
func TestClickingTheTableChromeSelectsNothing(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	top := rowLine(t, m, 0)
	for _, y := range []int{top - 2, top - 1} {
		if got := m.hitTest(2, y); got.Zone != clickNone {
			t.Errorf("y=%d resolved to %v; the column header and rule are not rows", y, got.Zone)
		}
	}
}

// The sidebar is a readout. A click that appeared to do something there would
// teach a gesture that has no meaning.
func TestClickingTheSidebarDoesNothing(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	if !m.sidebarVisible() {
		t.Skip("sidebar not visible at this width")
	}
	layout := m.layout()
	if got := m.hitTest(layout.left+layout.main+2, rowLine(t, m, 0)); got.Zone != clickNone {
		t.Errorf("a click in the sidebar resolved to %v", got.Zone)
	}
}

// A dialog covers the list. Clicking through it to the rows behind is how a
// confirmation gets answered by accident.
func TestClicksDoNotReachTheListFromAnotherMode(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(3))
	y := rowLine(t, m, 1)
	for _, mode := range []uiMode{modeConfirmDelete, modeNewSession, modeAuth, modeProxy} {
		probe := m
		probe.mode = mode
		if got := probe.hitTest(2, y); got.Zone != clickNone {
			t.Errorf("mode %v: a click reached the list behind the dialog", mode)
		}
	}
}

// The rail does not emit one line per entry: a heading is preceded by a blank,
// and a provider sub-heading gets a line of its own. railLineOwners encodes
// those rules, and this renders the real rail and checks the label actually
// drawn on the line it claims - so the mapping and the renderer cannot drift
// apart silently.
func TestRailClickMappingMatchesTheRenderedRail(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(2))
	m.railOpen = true
	m.mode = modeRail
	if !m.railVisible() {
		t.Skip("rail not visible at this width")
	}

	lines := strings.Split(stripEscapes(m.View()), "\n")
	entries := m.railEntries()
	owners := railLineOwners(entries)

	checked := 0
	for line, idx := range owners {
		if idx < 0 {
			continue
		}
		y := railBodyTop(m) + line
		if y >= len(lines) {
			break
		}

		// The rail occupies the leftmost columns of the frame.
		runes := []rune(lines[y])
		width := min(m.layout().left, len(runes))
		drawn := string(runes[:width])

		want := entries[idx].Section
		if !entries[idx].Heading {
			want = entries[idx].Row.Label
		}
		if want == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(drawn), strings.ToLower(want)) {
			t.Errorf("line %d (y=%d) is mapped to entry %d (%q) but reads %q", line, y, idx, want, drawn)
		}
		checked++

		// And the hit test must agree with the mapping it is built from.
		if got := m.hitTest(1, y); got.Zone != clickRailRow || got.Index != idx {
			t.Errorf("hitTest at y=%d = %+v, want rail entry %d", y, got, idx)
		}
	}
	if checked == 0 {
		t.Fatal("no rail entries were checked; the mapping was not exercised")
	}
}

// railBodyTop is where the rail's entries start: its border, its title, and the
// filter input when one is showing.
func railBodyTop(m Model) int {
	top := 2
	if m.rail.filtering || strings.TrimSpace(m.rail.filter.Value()) != "" {
		top++
	}
	return top
}

// A second click on a rail row does what enter does - which for a collapsible
// heading is fold it, and for a row is go to what changes it.
func TestASecondClickOnARailHeadingFoldsIt(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, clickRows(2))
	m.railOpen = true
	m.mode = modeRail
	if !m.railVisible() {
		t.Skip("rail not visible at this width")
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	// Find a collapsible heading to click.
	target := -1
	for idx, entry := range m.railEntries() {
		if entry.Heading && m.railSectionIsCollapsible(entry.Section) {
			target = idx
			break
		}
	}
	if target < 0 {
		t.Skip("no collapsible heading in the rail")
	}

	line := -1
	for l, idx := range railLineOwners(m.railEntries()) {
		if idx == target {
			line = l
			break
		}
	}
	y := railBodyTop(m) + line
	press := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 1, Y: y}

	next, _ := m.Update(press)
	after := next.(Model)
	if after.rail.selected != target {
		t.Fatalf("rail selection = %d, want %d", after.rail.selected, target)
	}

	before := len(after.railEntries())
	next, _ = after.Update(press)
	if got := len(next.(Model).railEntries()); got == before {
		t.Errorf("a second click on a collapsible heading did not fold it (%d entries either way)", got)
	}
}
