package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

// The block list is the alternative to the session table.
//
// The table was sized for a fleet saint rarely has. Rendered at 170 columns
// with three sessions, three of forty-four rows carried content and the rest
// was void: NAME held twenty-two cells to say "alpha", and the context bar -
// the one value a person actually watches - was in the sidebar, reachable only
// by selecting a row.
//
// A block spends two lines per session instead of one and answers, without
// selecting anything: is it working, what is it working on, how full is its
// context, and what has it spent. Three sessions fill twelve meaningful rows
// rather than three sparse ones.
//
// This deliberately bends the layout rule of one home per fact. That rule
// exists to stop a column being charged for repeating what another column
// already says. Here the row takes the sidebar's job, and the sidebar keeps
// what does not fit on two lines: full repository paths, review paths,
// credential expiry.

const (
	// blockRows is the height of one session, including the blank line that
	// separates it from the next. The separator is part of the unit: without it
	// two sessions read as one four-line paragraph.
	blockRows = 3

	// blockMarkWidth is the change-mark gutter, left of everything. It is always
	// present, even when empty: a column that appears and disappears shifts the
	// whole list sideways, which is a change the eye reports and the fleet did
	// not make.
	blockMarkWidth = 2

	// blockGlyphWidth is the status marker and its gutter.
	blockGlyphWidth = 4
	// blockNameWidth is the name column on the first line, and the repository
	// summary's column on the second. The two align, so the eye reads a block
	// as one shape rather than two unrelated lines.
	blockNameWidth = 26
)

// activityGlyph marks a session's state in the gutter.
//
// It is never the only carrier: the same line spells the state in words a few
// cells later. The glyph exists so a fleet can be read by shape at a glance,
// which words in a column cannot do.
func (m Model) activityGlyph(row SessionRow) (string, lipgloss.Color) {
	return sessionDot(row, m.breath)
}

// sessionDot is the status marker beside a session's name, in both list styles.
//
// One function so the table and the block list cannot disagree about what a
// session is doing - they are the same screen to the person reading them, and a
// green dot in one view beside a yellow dot in the other is a bug that looks
// like a rendering glitch.
//
// The three that matter:
//
//	green   working - a request from this session is crossing the proxy
//	yellow  idle    - running, and not currently talking to a model
//	red     blocked - stopped at a prompt, waiting for a person
//
// Everything else is not one of those three and is drawn dim: a stopped or
// missing session has no activity to report, and giving it a colour would put a
// third meaning on a channel that is carrying two already. The exception is an
// orphan, which is debris the user has to clear.
func sessionDot(row SessionRow, breath int) (string, lipgloss.Color) {
	switch {
	case row.BlockedReason != "":
		return "●", lipgloss.Color(colBad)
	case row.Activity == "working":
		// The one cell in the interface that moves. Peripheral vision is
		// motion-sensitive and nearly colour-blind, so the movement is what
		// reaches someone looking at their editor and the colour is what tells
		// them which state it is once they look. The breath stays within the
		// greens for exactly that reason.
		return "●", lipgloss.Color(breathBrightness(breath))
	case row.Activity == "idle":
		return "●", lipgloss.Color(colAttention)
	case row.Orphan:
		return "◆", lipgloss.Color(colAttention)
	default:
		// Stopped, missing, or nothing measuring: no activity to report.
		return "·", lipgloss.Color(colDim)
	}
}

// blockStatus is the middle of a block's first line: what the session is and
// what it is doing.
func blockStatus(row SessionRow) string {
	parts := []string{}
	if row.Runtime != "" {
		parts = append(parts, row.Runtime)
	}
	// Precedence is by how the claim was arrived at. "working" is measured -
	// a request is crossing the proxy or it is not - so it outranks a screen
	// pattern. "blocked" is inferred, and it outranks "idle" because idle is
	// only ever the absence of the other two.
	switch {
	case row.Activity == "working":
		parts = append(parts, row.Activity)
	case row.BlockedReason != "":
		parts = append(parts, "blocked · "+row.BlockedReason)
	case row.Activity == "idle":
		parts = append(parts, row.Activity)
	case row.State != "":
		parts = append(parts, row.State)
	}
	return strings.Join(parts, " · ")
}

// blockRepos is the second line's left column: where the work is happening.
//
// The active repository is the one the loader already marked, not a fresh
// selection: SelectActiveRepo returns -1 when nothing qualifies, and a block
// that indexed with that crashed the whole TUI. Reading the flag makes the
// "none of them" case an ordinary fallback instead of a panic.
func blockRepos(row SessionRow) string {
	if len(row.RepoDetails) == 0 {
		return row.Repos
	}
	detail := row.RepoDetails[0]
	for _, candidate := range row.RepoDetails {
		if candidate.Active {
			detail = candidate
			break
		}
	}

	parts := []string{detail.Name}
	if detail.Branch != "" && detail.Branch != "-" {
		parts = append(parts, detail.Branch)
	}
	switch {
	case detail.Missing:
		parts = append(parts, "missing")
	case detail.NoVCS:
		// "clean" is a claim saint cannot make about a directory it has no
		// history for.
		parts = append(parts, "no git")
	case detail.DirtyCount > 0:
		parts = append(parts, fmt.Sprintf("%d dirty", detail.DirtyCount))
	default:
		parts = append(parts, "clean")
	}
	if len(row.RepoDetails) > 1 {
		parts = append(parts, fmt.Sprintf("+%d", len(row.RepoDetails)-1))
	}
	return strings.Join(parts, " · ")
}

// blockSpend is the second line's right half: the context bar and what the
// session has spent.
//
// Every branch here is a different kind of "not measured", and none of them is
// allowed to read as zero. A session the proxy never saw has no bar and no
// counts; one whose provider reports no counters says so in words; one whose
// model has no verified context window gets the counts without a bar, because
// a percentage of an unknown window is an invented number.
func (m Model) blockSpend(row SessionRow, width int) string {
	if !row.UsageKnown || !row.Usage.Reported {
		if row.State != "running" {
			return ""
		}
		return sidebarMuted("tokens unreported")
	}

	// Named "in+out", not "tok", because it is not the total and the difference
	// is enormous. A Claude Code conversation re-sends its whole history every
	// turn, so cache reads routinely dwarf fresh input by an order of
	// magnitude; a bare "tok" next to a number that excludes them is a wrong
	// answer dressed as a total. The sidebar carries all four counters
	// separately, which is where the full accounting lives.
	spent := fmt.Sprintf("%s req · %s in+out",
		formatTokens(int64(row.Usage.Requests)),
		formatTokens(row.Usage.InputTokens+row.Usage.OutputTokens))

	limit, ok := usage.ContextLimit(row.Usage.Model)
	if !ok || limit <= 0 {
		return sidebarMuted(spent)
	}

	fraction := float64(row.Usage.Context) / float64(limit)
	barWidth := max(8, min(24, width-len(spent)-10))
	return fmt.Sprintf("%s %3.0f%%   %s",
		renderBar(fraction, barWidth), fraction*100, sidebarMuted(spent))
}

// expanded reports whether a session gets its full two lines.
//
// Calm technology: information should move between the centre and the
// periphery rather than sitting permanently in the centre. A fleet where every
// session shouts equally forever is a fleet nobody can read at twelve sessions,
// and most of them are doing nothing most of the time.
//
// Three reasons to be in the centre, and no others: it is working, it changed
// and nobody has looked yet, or the user has it selected. Everything else
// collapses to one dim line and stays legible.
func (m Model) blockExpanded(row SessionRow, selected bool) bool {
	if selected || row.Activity == "working" {
		return true
	}
	_, changed := m.markFor(row.ID)
	return changed
}

// collapsedLine is a quiet session in one line.
//
// "idle" carries a duration only when one was measured. LastRun is when the
// session last started, not when its agent last did anything, so it cannot
// answer "how long has this been quiet" - and answering it from the wrong
// field is exactly the invented number this codebase refuses.
func (m Model) collapsedLine(row SessionRow) string {
	parts := []string{row.Name}
	if repo := blockRepos(row); repo != "" {
		parts = append(parts, repo)
	}

	state := row.Activity
	if state == "" || state == "-" {
		state = row.State
	}
	if state == "idle" && row.UsageKnown && row.Usage.Reported && !row.Usage.LastAt.IsZero() {
		state = "idle " + humanDuration(m.clock().Sub(row.Usage.LastAt))
	}
	parts = append(parts, state)
	return strings.Join(parts, " · ")
}

// renderBlocks draws the session list as two-line blocks.
func (m Model) renderBlocks() string {
	panel := lipgloss.NewStyle().
		Padding(0, 1).
		Width(max(60, m.contentWidth()-2))

	inner := m.listInnerWidth()

	var b strings.Builder
	rows := m.filteredRows()
	if len(rows) == 0 {
		b.WriteString(emptyStateHint())
		return panel.Render(b.String())
	}

	start, end := m.visibleBlockRange(len(rows))
	for idx := start; idx < end; idx++ {
		b.WriteString(m.renderBlock(rows[idx], idx == m.selected, inner))
	}
	if end < len(rows) {
		b.WriteString(sidebarMuted(fmt.Sprintf("… %d more", len(rows)-end)))
		b.WriteString("\n")
	}
	return panel.Render(b.String())
}

// padVisual pads to a display width, measured in cells.
//
// padCell, which the table uses, pads by len() - a byte count. Every name in
// the table is ASCII today so it has never mattered there, but the block list
// puts a glyph in the gutter: "●" is three bytes and "·" is two, so padCell
// gave them different amounts of padding and the markers did not line up.
// Using lipgloss.Width measures what the terminal actually draws.
// It truncates to width, not width-1. padCell reserves that last cell to keep
// a gap between columns, which is right for a column and wrong for a line: a
// line already exactly the column's width came back one cell shorter with an
// ellipsis on the end, which is how "2m ago" became "2m a…".
func padVisual(value string, width int) string {
	value = truncateString(value, width)
	if pad := width - lipgloss.Width(value); pad > 0 {
		return value + strings.Repeat(" ", pad)
	}
	return value
}

// markGutter is the change column: two cells, always present.
//
// The mark is a character before it is a colour, because a change the reader
// cannot name is just a flicker.
func markGutter(mark changeMark, ok bool) string {
	if !ok {
		return strings.Repeat(" ", blockMarkWidth)
	}
	return padVisual("▸", blockMarkWidth)
}

// renderBlock draws one session: two lines when it wants attention, one when it
// does not.
func (m Model) renderBlock(row SessionRow, selected bool, inner int) string {
	glyph, glyphColor := m.activityGlyph(row)
	mark, marked := m.markFor(row.ID)

	gutter := markGutter(mark, marked) + padVisual(glyph, blockGlyphWidth)
	lead := blockMarkWidth + blockGlyphWidth
	blank := strings.Repeat(" ", lead)

	selectStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(colOnDark)).
		Background(lipgloss.Color(colSelect))

	// The right edge: what changed if something did, otherwise how old this is.
	// A mark that does not say what it means is decoration.
	right := row.LastRun
	if marked {
		right = string(mark.Kind)
	}

	if !m.blockExpanded(row, selected) {
		line := gutter + m.collapsedLine(row)
		gap := max(1, inner-lipgloss.Width(line)-lipgloss.Width(right))
		line += strings.Repeat(" ", gap) + right
		if selected {
			return selectStyle.Render(padVisual(line, inner)) + "\n"
		}
		// A collapsed row is dim by default - it is not asking for anything -
		// but a stopped or blocked one keeps its colour, because the reason it
		// collapsed is that nothing is happening, not that nothing is wrong.
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(colDim))
		if row.BlockedReason != "" || row.State == "stopped" || row.Orphan {
			style = lipgloss.NewStyle().Foreground(glyphColor)
		}
		return style.Render(truncateString(line, inner)) + "\n"
	}

	// The cells keep one reserved space so a long name cannot touch the next
	// column; the lines below do not, because they are not columns.
	nameCell := padVisual(truncateString(row.Name, blockNameWidth-1), blockNameWidth)
	repoCell := padVisual(truncateString(blockRepos(row), blockNameWidth-1), blockNameWidth)

	status := blockStatus(row)
	spend := m.blockSpend(row, inner-lead-blockNameWidth)

	// The age is pushed hard right, so the ages form a column the eye scans
	// without reading across.
	used := lead + blockNameWidth + lipgloss.Width(status)
	gap := max(1, inner-used-lipgloss.Width(right))

	if selected {
		// The whole block inverts, not one of its lines: a half-highlighted
		// block reads as two rows again.
		first := gutter + nameCell + status + strings.Repeat(" ", gap) + right
		second := blank + repoCell + stripStyle(spend)
		return selectStyle.Render(padVisual(first, inner)) + "\n" +
			selectStyle.Render(padVisual(second, inner)) + "\n\n"
	}

	markStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))
	rightStyle := sidebarMuted
	if marked {
		rightStyle = func(text string) string { return markStyle.Render(text) }
	}

	return markStyle.Render(markGutter(mark, marked)) +
		lipgloss.NewStyle().Foreground(glyphColor).Render(padVisual(glyph, blockGlyphWidth)) +
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInk)).Render(nameCell) +
		sidebarMuted(status) +
		strings.Repeat(" ", gap) +
		rightStyle(right) + "\n" +
		blank + sidebarMuted(repoCell) + spend + "\n\n"
}

// visibleBlockRange walks the fleet counting the lines each session will
// actually occupy, because a collapsed session is one line and an expanded one
// is three. Dividing by a fixed block height would either overflow the column
// or leave a third of it blank.
func (m Model) visibleBlockRange(total int) (int, int) {
	height := max(1, m.listHeight())
	rows := m.filteredRows()

	start := m.scroll
	if start > total {
		start = 0
	}

	used := 0
	end := start
	for end < total {
		cost := 1
		if end < len(rows) && m.blockExpanded(rows[end], end == m.selected) {
			cost = blockRows
		}
		if used+cost > height && end > start {
			break
		}
		used += cost
		end++
	}
	return start, end
}

// stripStyle removes SGR sequences so a styled fragment can be re-styled or
// measured as plain text. lipgloss composes styles by wrapping, and a selected
// block that kept its inner colours would paint them over the selection.
func stripStyle(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b {
			j := i
			for j < len(value) && value[j] != 'm' {
				j++
			}
			if j < len(value) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}

// toggleListStyle flips between the table and the block list and remembers the
// choice, so comparing them is one key rather than an edit to config.json.
func (m Model) toggleListStyle() (tea.Model, tea.Cmd) {
	if m.loader == nil || m.loader.Store == nil {
		return m, nil
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil {
		m.lastError = err
		m.status = statusLine(err.Error())
		return m, nil
	}

	if cfg.ListStyleOrDefault() == store.ListStyleBlocks {
		cfg.ListStyle = store.ListStyleTable
	} else {
		cfg.ListStyle = store.ListStyleBlocks
	}
	if err := m.loader.Store.SaveConfig(cfg); err != nil {
		m.lastError = err
		m.status = statusLine(err.Error())
		return m, nil
	}

	// Selection is an index into the same rows either way, but the number of
	// them on screen is not: a scroll offset valid for single-line rows can sit
	// past the end in blocks, which renders as an empty list rather than a
	// shorter one.
	m.scroll = 0
	m.status = "list style: " + cfg.ListStyleOrDefault()
	return m, nil
}
