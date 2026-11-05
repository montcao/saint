package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
)

// Clicking.
//
// saint has had mouse reporting on since the wheel was wired up, so every click
// already arrives - they were simply dropped. What was missing is knowing what
// is under the pointer, and the honest difficulty is that nothing tracks its
// own position: View stacks strings with lipgloss and the y-axis is implicit.
//
// The rule followed here is that the hit test must never restate the layout.
// Anywhere it needs a height it renders the same function the view renders and
// measures the result, because a second copy of the arithmetic would agree with
// the first exactly until someone changed one of them, and then clicks would
// land on the wrong row in a way nobody would think to test. Re-rendering a
// header to answer a click is free at the rate humans click.

// clickZone names what kind of thing is under the pointer.
type clickZone int

const (
	clickNone clickZone = iota
	clickSessionRow
	clickRailRow
)

// clickTarget is what a click resolved to.
type clickTarget struct {
	Zone clickZone
	// Index is into filteredRows for a session, or into railEntries for a rail
	// row. It is only meaningful when Zone says so.
	Index int
}

// hitTest resolves a terminal coordinate to something saint can act on.
//
// Coordinates are zero-based from the top-left of the terminal, which is what
// Bubble Tea reports.
func (m Model) hitTest(x, y int) clickTarget {
	if x < 0 || y < 0 || m.width <= 0 {
		return clickTarget{}
	}

	layout := m.layout()
	switch {
	case layout.left > 0 && x < layout.left:
		return m.hitRail(y)
	case x < layout.left+layout.main:
		return m.hitMain(y)
	default:
		// The sidebar is a readout. A click that appeared to do something there
		// would be teaching a gesture that has no meaning.
		return clickTarget{}
	}
}

// hitMain resolves a click in the session list.
func (m Model) hitMain(y int) clickTarget {
	// The list is only a list in the modes that show one. Clicking through a
	// dialog to the rows behind it is how a confirmation gets answered by
	// accident.
	if m.mode != modeList && m.mode != modeFilter {
		return clickTarget{}
	}

	rows := m.filteredRows()
	if len(rows) == 0 {
		return clickTarget{}
	}

	// Measured, not assumed: the header's height changes with the terminal's
	// width as chips wrap out, and the filter row exists only while it is doing
	// something.
	top := lipgloss.Height(m.renderHeader())
	if m.mode == modeFilter || strings.TrimSpace(m.filterInput.Value()) != "" {
		top += lipgloss.Height(m.renderFilters())
	}

	offset := y - top
	if offset < 0 {
		return clickTarget{}
	}

	start, end := m.visibleRowRange(len(rows))
	if m.config().ListStyleOrDefault() == store.ListStyleBlocks {
		return blockAt(m, rows, start, end, offset)
	}

	// The table opens with its column header and a rule, neither of which is a
	// row.
	const tableChrome = 2
	idx := start + offset - tableChrome
	if offset < tableChrome || idx >= end {
		return clickTarget{}
	}
	return clickTarget{Zone: clickSessionRow, Index: idx}
}

// blockAt walks the visible blocks counting the lines each one occupies.
//
// A block's height is not fixed - a session that wants attention is three lines
// and a quiet one is a single line - so the only way to know which session a
// line belongs to is to lay them out the same way the renderer does, from the
// same predicate.
func blockAt(m Model, rows []SessionRow, start, end, offset int) clickTarget {
	line := 0
	for idx := start; idx < end; idx++ {
		height := 1
		if m.blockExpanded(rows[idx], idx == m.selected) {
			height = blockRows
		}
		if offset < line+height {
			// The blank separator under an expanded block belongs to no
			// session: a click there should do nothing rather than pick its
			// neighbour.
			if height == blockRows && offset == line+blockRows-1 {
				return clickTarget{}
			}
			return clickTarget{Zone: clickSessionRow, Index: idx}
		}
		line += height
	}
	return clickTarget{}
}

// visibleRowRange is whichever range function the current list style uses, so
// a click resolves against the rows that are actually on screen.
func (m Model) visibleRowRange(total int) (int, int) {
	if m.config().ListStyleOrDefault() == store.ListStyleBlocks {
		return m.visibleBlockRange(total)
	}
	return m.visibleRange(total)
}

// hitRail resolves a click in the control rail.
func (m Model) hitRail(y int) clickTarget {
	if !m.railVisible() {
		return clickTarget{}
	}

	// The rail's box draws a border, then the title, then an optional filter
	// input, and the entries follow.
	top := 1 + 1
	if m.rail.filtering || strings.TrimSpace(m.rail.filter.Value()) != "" {
		top++
	}

	offset := y - top
	if offset < 0 {
		return clickTarget{}
	}

	for line, idx := range railLineOwners(m.railEntries()) {
		if line == offset {
			if idx < 0 {
				return clickTarget{}
			}
			return clickTarget{Zone: clickRailRow, Index: idx}
		}
	}
	return clickTarget{}
}

// railLineOwners reports, for each line the rail's body emits, which entry owns
// it - or -1 for a line that belongs to none.
//
// The rail does not emit one line per entry: a heading is preceded by a blank,
// and a provider sub-heading gets a line of its own that labels the rows under
// it rather than being one. A click has to account for all of that, and a
// regression test renders the real rail and checks the label on the line this
// says is a given entry's, so the two cannot drift apart silently.
func railLineOwners(entries []railEntry) []int {
	owners := make([]int, 0, len(entries)*2)
	group := ""
	for idx, entry := range entries {
		if entry.Heading {
			group = ""
			owners = append(owners, -1) // the blank line above a heading
			owners = append(owners, idx)
			continue
		}
		if entry.Row.Group != "" && entry.Row.Group != group {
			group = entry.Row.Group
			owners = append(owners, -1) // the sub-heading labelling the group
		}
		owners = append(owners, idx)
	}
	return owners
}

// doubleClickWithin is how close together two presses on the same row have to
// be to read as one gesture.
//
// Generous rather than tight: the cost of missing a double click is that the
// user clicks again, and the cost of inventing one is attaching to a session
// they were only selecting.
const doubleClickWithin = 500 * time.Millisecond

// updateMouse handles every mouse event.
//
// A press selects and a second press on the same row opens - the same division
// the rest of the interface already uses, where moving is free and entering is
// the deliberate act.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Scrolling an attached pane is the older behaviour and stays exactly as it
	// was.
	if m.mode == modeAttached {
		if m.pane == nil {
			return m, nil
		}
		up := msg.Button == tea.MouseButtonWheelUp
		if !up && msg.Button != tea.MouseButtonWheelDown {
			return m, nil
		}

		// The guest gets first refusal. A full-screen agent keeps its own
		// history and saint has none for it, so scrolling saint's empty
		// scrollback is the one thing that cannot work - which is precisely
		// what it did for as long as this feature existed.
		x, y := m.paneRelative(msg.X, msg.Y)
		if m.pane.ForwardWheel(up, x, y) {
			return m, nil
		}
		if up {
			m.pane.ScrollUp(paneWheelLines)
		} else {
			m.pane.ScrollDown(paneWheelLines)
		}
		return m, nil
	}

	// The wheel moves the selection in a list, which is what a wheel does
	// everywhere else.
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.moveSelection(-1)
		return m, nil
	case tea.MouseButtonWheelDown:
		m.moveSelection(1)
		return m, nil
	}

	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}

	target := m.hitTest(msg.X, msg.Y)
	switch target.Zone {
	case clickSessionRow:
		// A click into the list is also a decision to be in the list: clicking a
		// row while the filter input has focus should not leave the next
		// keystroke going into the filter.
		if m.mode == modeFilter {
			m.mode = modeList
			m.filterInput.Blur()
		}
		repeat := m.isDoubleClick(target, msg)
		m.selected = target.Index
		m.clampSelection()
		m.noteClick(target, msg)
		if repeat {
			return m.updateList(tea.KeyMsg{Type: tea.KeyEnter})
		}
		return m, nil

	case clickRailRow:
		repeat := m.isDoubleClick(target, msg)
		m.mode = modeRail
		m.rail.selected = target.Index
		m.noteClick(target, msg)
		if repeat {
			return m.updateRail(tea.KeyMsg{Type: tea.KeyEnter})
		}
		return m, nil
	}
	return m, nil
}

// isDoubleClick reports whether this press repeats the previous one on the same
// thing, closely enough in time to be one gesture.
func (m Model) isDoubleClick(target clickTarget, msg tea.MouseMsg) bool {
	if m.lastClick.Zone != target.Zone || m.lastClick.Index != target.Index {
		return false
	}
	if m.lastClickAt.IsZero() {
		return false
	}
	return m.clock().Sub(m.lastClickAt) <= doubleClickWithin
}

func (m *Model) noteClick(target clickTarget, msg tea.MouseMsg) {
	m.lastClick = target
	m.lastClickAt = m.clock()
}

// paneRelative converts a terminal coordinate into a one-based coordinate
// inside the attached pane, which is the frame of reference the wire format and
// the program inside the pane both use.
//
// The origin is measured from the same renderers the view uses, for the reason
// every other measurement here is: a second copy of the arithmetic would drift.
func (m Model) paneRelative(x, y int) (int, int) {
	layout := m.layout()
	top := lipgloss.Height(m.renderHeader()) + lipgloss.Height(m.renderTable())

	cols, rows := m.paneSize()
	// The pane draws inside a border, so its first text cell is one in from
	// each edge.
	px := x - layout.left - 1
	py := y - top - 1

	return clampCoord(px, cols), clampCoord(py, rows)
}

// clampCoord keeps a coordinate inside the pane and one-based. A report for a
// cell the program does not have is worse than a report for the nearest one it
// does.
func clampCoord(v, size int) int {
	if v < 0 {
		v = 0
	}
	if size > 0 && v >= size {
		v = size - 1
	}
	return v + 1
}
