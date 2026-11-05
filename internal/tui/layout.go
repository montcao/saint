package tui

import "github.com/charmbracelet/lipgloss"

const (
	// sidebarWidth is fixed. A proportional sidebar would reflow the token
	// columns on every resize, and these numbers are read by glancing at a
	// position, not by reading a label.
	sidebarWidth = 34

	// minMainWidth is the narrowest the session list stays usable at. Side panes
	// appear only when the main column can keep at least this much space.
	minMainWidth = 64
)

// layoutPlan is how the terminal's width is divided.
//
// Two side columns, and they answer different questions. The right sidebar is
// the selected session; the left rail is the machine. An earlier left pane was
// removed for blurring that line - it restated the header chips, the sidebar and
// the key list, charging the session list twenty-two columns to say things that
// were already on screen. Nothing may appear in both.
type layoutPlan struct {
	total int
	left  int
	main  int
	right int
}

func planLayout(total int, railOpen bool) layoutPlan {
	if total < 0 {
		total = 0
	}

	layout := layoutPlan{
		total: total,
		main:  total,
	}

	// The rail is taken first. It was asked for explicitly; the sidebar is
	// ambient, so when both cannot fit the sidebar is the one that yields.
	if railOpen && total >= railWidth+minMainWidth {
		layout.left = railWidth
		layout.main = total - railWidth
	}

	if layout.main >= minMainWidth+sidebarWidth {
		layout.right = sidebarWidth
		layout.main -= sidebarWidth
	}

	return layout
}

func (m Model) layout() layoutPlan {
	return planLayout(m.width, m.railVisible())
}

// sidebarVisible reports whether the terminal is wide enough for the usage sidebar.
func (m Model) sidebarVisible() bool {
	return m.layout().right > 0
}

// contentWidth is the width the main column gets. Everything that sizes itself
// against the terminal must use this instead of m.width, including the pane:
// its value becomes the PTY window size, and an agent told it has more columns
// than it does renders as garbage.
func (m Model) contentWidth() int {
	return m.layout().main
}

// withPanels composes the control rail and the usage sidebar around the main
// column.
func (m Model) withPanels(main string) string {
	layout := m.layout()
	main = clampWidth(main, layout.main)
	if layout.left == 0 && layout.right == 0 {
		return main
	}

	height := lipgloss.Height(main)
	columns := make([]string, 0, 3)
	if layout.left > 0 {
		columns = append(columns, m.renderRail(height))
	}
	columns = append(columns, main)
	if layout.right > 0 {
		columns = append(columns, m.renderSidebar(height))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, columns...)
}
