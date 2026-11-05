package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

func blockModel(t *testing.T, rows []SessionRow) Model {
	t.Helper()

	st := store.New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg, _ := st.EnsureConfig()
	cfg.ListStyle = store.ListStyleBlocks
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	m := NewModel(&SnapshotLoader{
		Store: st, Sandbox: &fakeSandbox{}, Inspector: NewRepoInspector("/usr/bin/git", nil),
	}, nil)
	m.width, m.height = 170, 44
	m.rows = rows
	return m
}

func workingRow() SessionRow {
	return SessionRow{
		ID: "alpha", Name: "alpha", Repos: "saint", Runtime: "claude",
		State: "running", Activity: "working", LastRun: "2m ago", UsageKnown: true,
		Usage: usage.Session{
			Model: "claude-opus-5", Requests: 412, InputTokens: 180_000,
			OutputTokens: 24_000, Context: 128_000, Reported: true,
		},
		RepoDetails: []RepoDetail{{Name: "saint", Branch: "main", DirtyCount: 4, Active: true}},
	}
}

func plain(s string) string {
	return stripStyle(s)
}

// The block list exists so a fleet can be read without selecting anything. Its
// whole claim is that one session answers four questions at a glance.
func TestBlockAnswersWithoutSelecting(t *testing.T) {
	t.Parallel()

	got := plain(blockModel(t, []SessionRow{workingRow()}).renderTable())
	for _, want := range []string{"alpha", "claude", "working", "saint", "main", "4 dirty", "2m ago", "412 req"} {
		if !strings.Contains(got, want) {
			t.Errorf("block omits %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "█") {
		t.Errorf("block shows no context bar:\n%s", got)
	}
}

// Both lines of a block share one column grid, and the gutter glyph differs per
// state. padCell pads by len(), a byte count, and "·" is two bytes where "●" is
// three - so padding by that measure indents them differently.
//
// The column is measured in display cells, not bytes, because cells are what
// the terminal draws and what the reader sees.
func TestBlockGutterAlignsAcrossStates(t *testing.T) {
	t.Parallel()

	rows := []SessionRow{
		workingRow(),
		{ID: "b", Name: "beta", Runtime: "codex", State: "running", Activity: "idle", LastRun: "1m",
			RepoDetails: []RepoDetail{{Name: "webapp", Branch: "main", Active: true}}},
		{ID: "c", Name: "gamma", Runtime: "claude", State: "stopped", Activity: "-", LastRun: "3h",
			RepoDetails: []RepoDetail{{Name: "infra", Branch: "main", Active: true}}},
	}

	columns := map[string]int{}
	for _, line := range strings.Split(plain(blockModel(t, rows).renderTable()), "\n") {
		for _, name := range []string{"alpha", "beta", "gamma"} {
			idx := strings.Index(line, name)
			if idx < 0 {
				continue
			}
			// Display column, not byte offset.
			columns[name] = lipgloss.Width(line[:idx])
		}
	}
	if len(columns) != 3 {
		t.Fatalf("found %d session names, want 3: %v", len(columns), columns)
	}
	var first int
	for _, got := range columns {
		if first == 0 {
			first = got
		}
		if got != first {
			t.Errorf("names start at differing display columns %v; the gutter does not align", columns)
			break
		}
	}
}

// Every unmeasured value must stay unmeasured. A block that printed 0% for a
// session the proxy never saw would be the exact failure the context bar was
// built to avoid.
func TestBlockNeverInventsANumber(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  SessionRow
		deny []string
	}{
		{
			name: "never metered",
			row: SessionRow{
				ID: "a", Name: "a", Runtime: "claude", State: "running", Activity: "idle",
				RepoDetails: []RepoDetail{{Name: "r", Branch: "main", Active: true}},
			},
			deny: []string{"0%", "█"},
		},
		{
			name: "provider reports no counters",
			row: SessionRow{
				ID: "b", Name: "b", Runtime: "codex", State: "running", Activity: "idle",
				UsageKnown: true, Usage: usage.Session{Model: "gpt-5-codex", Requests: 12},
				RepoDetails: []RepoDetail{{Name: "r", Branch: "main", Active: true}},
			},
			deny: []string{"0%", "█"},
		},
		{
			// Counts are real; the window is not verified, so there is no
			// percentage to show.
			name: "model has no verified context window",
			row: SessionRow{
				ID: "c", Name: "c", Runtime: "codex", State: "running", Activity: "idle",
				UsageKnown: true,
				Usage: usage.Session{
					Model: "gpt-5-codex", Requests: 9, InputTokens: 1000,
					Context: 1000, Reported: true,
				},
				RepoDetails: []RepoDetail{{Name: "r", Branch: "main", Active: true}},
			},
			deny: []string{"%", "█"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := plain(blockModel(t, []SessionRow{tc.row}).renderTable())
			for _, deny := range tc.deny {
				if strings.Contains(got, deny) {
					t.Errorf("block shows %q for a value nothing measured:\n%s", deny, got)
				}
			}
		})
	}
}

// SelectActiveRepo returns -1 when no repository qualifies, and a block that
// indexed with it panicked and took the whole TUI down.
func TestBlockSurvivesRowsWithNoActiveRepo(t *testing.T) {
	t.Parallel()

	rows := []SessionRow{
		{ID: "a", Name: "no-repos", Runtime: "claude", State: "running", Activity: "idle", Repos: "-"},
		{ID: "b", Name: "none-active", Runtime: "claude", State: "running", Activity: "idle",
			RepoDetails: []RepoDetail{{Name: "one", Branch: "main"}, {Name: "two", Branch: "dev"}}},
		{ID: "c", Name: "orphan", State: "orphan (running)", Activity: "-", Orphan: true},
	}

	got := plain(blockModel(t, rows).renderTable())
	for _, want := range []string{"no-repos", "none-active", "orphan"} {
		if !strings.Contains(got, want) {
			t.Errorf("block list dropped %q:\n%s", want, got)
		}
	}
	// A row with several repositories says so rather than silently showing one.
	if !strings.Contains(got, "+1") {
		t.Errorf("a session with two repositories does not say so:\n%s", got)
	}
}

// Every line must fit the column, or a wrapped block turns one session into
// three rows and throws off the selection highlight.
func TestBlockLinesFitTheColumn(t *testing.T) {
	t.Parallel()

	long := workingRow()
	long.Name = strings.Repeat("session-", 12)
	long.RepoDetails = []RepoDetail{{
		Name: strings.Repeat("repo-", 20), Branch: strings.Repeat("branch-", 10),
		DirtyCount: 9999, Active: true,
	}}

	for _, width := range []int{62, 80, 120, 170} {
		m := blockModel(t, []SessionRow{long})
		m.width = width
		inner := m.listInnerWidth()
		for _, line := range strings.Split(plain(m.renderTable()), "\n") {
			if got := lipgloss.Width(strings.TrimRight(line, " ")); got > inner+2 {
				t.Errorf("width %d: line of %d cells exceeds the %d-cell column: %q",
					width, got, inner, line)
			}
		}
	}
}

// The toggle is the point of the flag: comparing the two styles has to be one
// key, not an edit to config.json, and the choice has to survive a restart.
func TestToggleListStyleFlipsAndPersists(t *testing.T) {
	t.Parallel()

	m := blockModel(t, []SessionRow{workingRow()})
	if got := m.config().ListStyleOrDefault(); got != store.ListStyleBlocks {
		t.Fatalf("setup: style = %q", got)
	}

	next, _ := m.toggleListStyle()
	m = next.(Model)
	if got := m.config().ListStyleOrDefault(); got != store.ListStyleTable {
		t.Errorf("after toggle, style = %q, want the table", got)
	}
	if !strings.Contains(m.renderTable(), "NAME") {
		t.Error("toggling to the table did not render the table")
	}

	next, _ = m.toggleListStyle()
	m = next.(Model)
	if got := m.config().ListStyleOrDefault(); got != store.ListStyleBlocks {
		t.Errorf("toggling back gave %q, want blocks", got)
	}
}

// An unrecognised value in a hand-edited config must not produce a blank list.
func TestUnknownListStyleFallsBackToTheTable(t *testing.T) {
	t.Parallel()

	if got := (store.Config{ListStyle: "carousel"}).ListStyleOrDefault(); got != store.ListStyleTable {
		t.Errorf("ListStyleOrDefault() = %q, want %q", got, store.ListStyleTable)
	}
	if got := (store.Config{}).ListStyleOrDefault(); got != store.ListStyleTable {
		t.Errorf("an empty config gave %q, want the table it already had", got)
	}
}

// A block's token figure excludes cache reads and writes, so it must not be
// labelled as a total.
//
// A Claude Code conversation re-sends its history every turn, so cache reads
// routinely dwarf fresh input by an order of magnitude. Calling input+output
// "tok" invites a comparison against the agent's own accounting that can only
// ever disagree.
func TestBlockDoesNotLabelAPartialCountAsATotal(t *testing.T) {
	t.Parallel()

	row := workingRow()
	row.Usage.InputTokens = 10_000
	row.Usage.OutputTokens = 2_000
	row.Usage.CacheReadTokens = 1_400_000
	row.Usage.CacheWriteTokens = 30_000

	got := plain(blockModel(t, []SessionRow{row}).renderTable())
	if strings.Contains(got, " tok") {
		t.Errorf("a figure excluding cache tokens is labelled as a total:\n%s", got)
	}
	if !strings.Contains(got, "in+out") {
		t.Errorf("the token figure does not say what it counts:\n%s", got)
	}
	// And it must be the partial figure, not a silently corrected total.
	if !strings.Contains(got, "12.0k") {
		t.Errorf("expected the in+out figure of 12.0k:\n%s", got)
	}
}

// The three states a person actually acts on, each with its own colour, and one
// function behind both list styles so the table and the block list cannot
// disagree about what a session is doing.
func TestTheStatusDotMapsStateToColour(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		row   SessionRow
		glyph string
		want  string
	}{
		{"working is green", SessionRow{State: "running", Activity: "working"}, "●", colGood},
		{"idle is yellow", SessionRow{State: "running", Activity: "idle"}, "●", colAttention},
		{"blocked is red", SessionRow{State: "running", Activity: "blocked", BlockedReason: "waiting to confirm"}, "●", colBad},
		// Not one of the three, so no colour is spent on it.
		{"stopped is dim", SessionRow{State: "stopped", Activity: "-"}, "·", colDim},
		{"missing is dim", SessionRow{State: "missing", Activity: "-"}, "·", colDim},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Phase 0 so the working breath is at a known point in its cycle.
			glyph, colour := sessionDot(tc.row, 0)
			if glyph != tc.glyph {
				t.Errorf("glyph = %q, want %q", glyph, tc.glyph)
			}
			if tc.name == "working is green" {
				// The breath moves through the greens, so this asserts the
				// cycle stays inside them rather than pinning one frame.
				for phase := 0; phase < breathFrames; phase++ {
					_, c := sessionDot(tc.row, phase)
					if string(c) != colGood && string(c) != colGoodDim && string(c) != colInkBright {
						t.Errorf("phase %d: working left the greens with %q", phase, c)
					}
				}
				return
			}
			if string(colour) != tc.want {
				t.Errorf("colour = %q, want %q", colour, tc.want)
			}
		})
	}
}

// Blocked outranks everything: a session stopped at a prompt is the one that
// needs a person, whatever its measured activity last said.
func TestBlockedOutranksTheMeasuredActivityOnTheDot(t *testing.T) {
	t.Parallel()

	row := SessionRow{State: "running", Activity: "idle", BlockedReason: "waiting to confirm"}
	if _, colour := sessionDot(row, 0); string(colour) != colBad {
		t.Errorf("a blocked session drew %q, want the blocked colour", colour)
	}
}

// The table is the default list style, so it carries the same marker. It had
// none, which meant the state was only readable by reading.
func TestTheTableCarriesTheStatusDot(t *testing.T) {
	t.Parallel()

	m := clickModel(t, store.ListStyleTable, []SessionRow{
		{ID: "a", Name: "alpha", Runtime: "claude", State: "running", Activity: "working"},
	})
	m.selected = -1

	got := plain(m.renderTable())
	if !strings.Contains(got, "● alpha") {
		t.Errorf("the table row carries no status dot beside the name:\n%s", got)
	}
	// The dot's column has no heading: a two-cell title would be a label
	// competing with the thing it labels.
	header, _, _ := strings.Cut(got, "\n")
	if strings.HasPrefix(strings.TrimSpace(header), "ST") {
		t.Errorf("the marker column grew a heading: %q", header)
	}
}
