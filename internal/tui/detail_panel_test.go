package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/usage"
)

// hostileRow is a session whose every guest-influenced string is as long and as
// hostile as the filesystem and git allow: a checkout nested past any real
// depth, a branch carrying escape sequences, and review paths from a merge that
// touched everything.
func hostileRow() SessionRow {
	return SessionRow{
		ID:      "abc123",
		Name:    strings.Repeat("session-", 200),
		Repos:   strings.Repeat("repo", 100),
		Runtime: strings.Repeat("claude", 100),
		State:   "running",
		LastRun: "2m ago",
		RepoDetails: []RepoDetail{{
			Path:        "/" + strings.Repeat("deep/", 1000),
			Name:        strings.Repeat("name", 100),
			Branch:      sanitizeInline(strings.Repeat("\x1b[31mfeature/\x1b]0;title\x07", 200)),
			DirtyCount:  4,
			LastTouched: "1m ago",
			ReviewPaths: []string{strings.Repeat("a/b/c.go ", 300)},
			Active:      true,
		}},
		UsageKnown: true,
		Usage: usage.Session{
			Model:   strings.Repeat("claude-opus-5-", 100),
			Context: 128_400, Requests: 37, Reported: true,
		},
	}
}

// benignRow is hostileRow's structural twin - same session, same one repository,
// same states - with values of an ordinary length.
func benignRow() SessionRow {
	row := hostileRow()
	row.Name = "demo"
	row.Repos = "repo"
	row.Runtime = "claude"
	row.RepoDetails[0].Path = "/src/repo"
	row.RepoDetails[0].Name = "repo"
	row.RepoDetails[0].Branch = "main"
	row.RepoDetails[0].ReviewPaths = []string{"main.go"}
	row.Usage.Model = "claude-opus-5"
	return row
}

func detailPanelModel(width int, row SessionRow) Model {
	return Model{
		width:  width,
		height: 40,
		rows:   []SessionRow{row},
		orphans: []sandbox.Info{
			{Name: "saint-" + strings.Repeat("orphan", 200), Runtime: sandbox.RuntimeClaude},
		},
	}
}

// The detail panel prints strings saint did not author: repository paths and
// branch names off the user's filesystem, review paths out of git. An over-long
// one used to be written whole, and a line past the panel's width does not
// render narrow - lipgloss wraps it, the panel gains a row, and the frame comes
// apart.
func TestDetailPanelFitsTheTerminalWithHostileContent(t *testing.T) {
	t.Parallel()

	for _, width := range []int{62, 80, 100, 120, 200} {
		m := detailPanelModel(width, hostileRow())
		for i, line := range strings.Split(m.renderDetailPanel(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: detail line %d is %d columns:\n%q", width, i, got, line)
			}
		}
	}
}

// Height is the half that a width check alone would miss: the panel sits above
// the footer, so one wrapped line pushes the status bar off the screen.
func TestDetailPanelHeightDoesNotDependOnContentLength(t *testing.T) {
	t.Parallel()

	for _, width := range []int{62, 80, 100, 120, 200} {
		benign := lipgloss.Height(detailPanelModel(width, benignRow()).renderDetailPanel())
		hostile := lipgloss.Height(detailPanelModel(width, hostileRow()).renderDetailPanel())
		if benign != hostile {
			t.Errorf("width %d: panel is %d rows for ordinary values and %d for long ones",
				width, benign, hostile)
		}
	}
}

// The whole view is what the user actually sees, and the detail panel is only
// bounded there if it is bounded here first.
func TestViewFitsTheTerminalWithAHostileSession(t *testing.T) {
	t.Parallel()

	for _, width := range []int{80, 100, 120, 200} {
		m := newRenderModel(t, width, 40)
		m.rows = []SessionRow{hostileRow()}
		for i, line := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: view line %d is %d columns:\n%q", width, i, got, line)
			}
		}
	}
}

// Truncating must not cost the panel the two things in a repository line that
// need a person: whether it is dirty and whether it has something to review.
func TestDetailPanelKeepsRepoStateWhenThePathIsLong(t *testing.T) {
	t.Parallel()

	m := detailPanelModel(120, hostileRow())
	panel := m.renderDetailPanel()
	for _, want := range []string{"4 dirty", "review:"} {
		if !strings.Contains(panel, want) {
			t.Errorf("a long path pushed %q off the panel:\n%s", want, panel)
		}
	}
}

// The transcript line is gone from the interface entirely. It was the one value
// with no bound of its own, and what it showed was a raw terminal dump's last
// line - routinely a cursor move rather than output, and never worth the widest
// column in the table.
func TestSessionTableHasNoTranscriptColumn(t *testing.T) {
	t.Parallel()

	m := detailPanelModel(200, benignRow())
	for _, surface := range []string{m.renderDetailPanel(), m.renderTable()} {
		if strings.Contains(strings.ToLower(surface), "last output") {
			t.Errorf("a surface still carries the transcript line:\n%s", surface)
		}
	}
}

// Columns leave from the right, whole. The alternative - letting every column
// shrink together - reaches a width where each one holds two characters and an
// ellipsis, which is a table that is present but tells the user nothing.
func TestSessionTableDropsColumnsFromTheRight(t *testing.T) {
	t.Parallel()

	wide := detailPanelModel(200, benignRow())
	if got := len(wide.visibleListColumns()); got != len(listColumns()) {
		t.Errorf("a wide terminal shows %d columns, want all %d", got, len(listColumns()))
	}

	narrow := detailPanelModel(62, benignRow())
	got := narrow.visibleListColumns()
	if len(got) == 0 {
		t.Fatal("a narrow terminal dropped every column: a table with no columns is a blank pane, not a narrow one")
	}
	if len(got) >= len(listColumns()) {
		t.Errorf("a narrow terminal kept all %d columns; nothing was dropped", len(got))
	}
	if got[0].Title != listColumns()[0].Title {
		t.Errorf("first column = %q, want the leftmost column kept", got[0].Title)
	}

	// What survives must actually fit, or dropping bought nothing.
	width := 0
	for _, column := range got {
		width += column.Width
	}
	if width > narrow.listInnerWidth() {
		t.Errorf("kept columns need %d cells, only %d available", width, narrow.listInnerWidth())
	}

	// The header and the body are generated from one list, so they cannot
	// disagree about which columns are present.
	if a, b := len(narrow.formatListHeader()), len(narrow.formatRow(benignRow())); a > narrow.listInnerWidth() || b > narrow.listInnerWidth() {
		t.Errorf("header/row widths = %d/%d, both must fit %d", a, b, narrow.listInnerWidth())
	}
}

// The activity column reports one thing: whether a request from this session is
// crossing the proxy. Everything saint cannot see from outside the guest - a
// build running, tests running, an agent waiting at its own prompt - is "idle"
// rather than a state saint would be guessing at.
func TestActivityLabelClaimsOnlyWhatWasMeasured(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		state    string
		inFlight int
		watched  bool
		want     string
	}{
		{"a request in flight is the only thing that reads as work", "running", 1, true, "working"},
		{"several requests are still one working session", "running", 4, true, "working"},
		{"running with nothing in flight", "running", 0, true, "idle"},
		// "idle" is a claim about something that is running.
		{"stopped", "stopped", 0, true, "-"},
		{"missing", "missing", 0, true, "-"},
		{"an orphan has no session to measure", "orphan (running)", 0, true, "-"},
		// The one that matters: with no meter every count is zero, so "idle"
		// would be printed for every session on every run and would be wrong
		// every time.
		{"running but nothing is watching", "running", 0, false, "-"},
	} {
		if got := activityLabel(tc.state, tc.inFlight, tc.watched); got != tc.want {
			t.Errorf("%s: activityLabel(%q, %d, %v) = %q, want %q", tc.name, tc.state, tc.inFlight, tc.watched, got, tc.want)
		}
	}
}

// A session that stopped before its agent ever started leaves a reason in the
// mounted session directory, because `container inspect` reports a stopped
// container's argv and not its exit code. The file is guest-writable, so it is
// read to be shown and never to be acted on.
func TestBootstrapErrorIsReadDefensively(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, contents string) string {
		t.Helper()
		path := t.TempDir() + "/" + sandbox.BootstrapErrorFile
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		return path
	}

	if got := bootstrapError(write(t, "the agent could not be started: claude\n")); got != "the agent could not be started: claude" {
		t.Errorf("bootstrapError() = %q, want the reason", got)
	}
	if got := bootstrapError(t.TempDir() + "/absent"); got != "" {
		t.Errorf("a missing file produced %q, want empty", got)
	}
	// Only the first line: the rest of a file the guest wrote is not a reason.
	if got := bootstrapError(write(t, "first\nsecond\nthird")); got != "first" {
		t.Errorf("bootstrapError() = %q, want only the first line", got)
	}
	// Escape sequences from a guest must not reach the terminal.
	if got := bootstrapError(write(t, "\x1b]0;pwned\x07boom")); strings.Contains(got, "\x1b") {
		t.Errorf("bootstrapError() = %q, want the escape payload stripped", got)
	}
	// A very large file must not be read whole.
	if got := bootstrapError(write(t, strings.Repeat("x", 1<<20))); len(got) > 8192 {
		t.Errorf("bootstrapError() returned %d bytes; the read is not capped", len(got))
	}
}
