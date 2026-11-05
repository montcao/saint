package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type commandChip struct {
	text string
	// active is the key that does the obvious thing in this mode - attach, run,
	// spawn. One per mode, and it is inverted rather than coloured.
	active bool

	// attention is a key whose effect cannot be undone, and it is set only while
	// that act is the pending one.
	//
	// It used to be set on every destructive key in every mode - "d delete" and
	// "ctrl+c quit" sat in the header permanently, in the same colour the
	// interface uses for a credential about to expire. A warning that is on
	// screen from launch to exit is not a warning, and worse, it taught the
	// reader to filter out the exact colour that a lapsed credential arrives in.
	// So the confirmation prompt keeps it and the key list does not: by then the
	// destructive thing is one keystroke away rather than hypothetical.
	attention bool
}

func (m Model) headerModeLabel() string {
	switch m.mode {
	case modeFilter:
		return "FILTER"
	case modeNewSession:
		return "SPAWN"
	case modeConfirmDelete:
		return "DELETE"
	case modeCommand:
		return "COMMAND"
	case modeAuth:
		return "AUTH"
	case modeProxy:
		return "PROXY"
	case modeAttached:
		return "ATTACHED"
	default:
		return "SESSIONS"
	}
}

func (m Model) headerCommandChips() []commandChip {
	switch m.mode {
	case modeAttached:
		return []commandChip{
			{text: "ctrl+\\ detach", active: true},
			{text: "ctrl+q quit"},
			{text: "pgup/pgdn · shift+↑↓ scroll"},
		}
	case modeCommand:
		return []commandChip{
			{text: "enter run", active: true},
			{text: "esc cancel"},
			{text: ":q quit"},
			{text: ":d detach"},
			{text: ":refresh sync"},
			{text: ":new spawn"},
			{text: ":auth auth"},
			{text: ":help help"},
		}
	case modeFilter:
		return []commandChip{
			{text: "type query", active: true},
			{text: "enter apply"},
			{text: "esc close"},
			{text: "ctrl+c quit"},
		}
	case modeNewSession:
		return []commandChip{
			{text: "enter spawn", active: true},
			{text: "esc cancel"},
			{text: "ctrl+c quit"},
		}
	case modeConfirmDelete:
		return []commandChip{
			// The one chip in the interface that is amber: a container and its
			// work are about to be destroyed by the next keystroke.
			{text: "enter confirm", attention: true},
			{text: "n cancel", active: true},
			{text: "esc cancel"},
		}
	case modeAuth:
		return []commandChip{
			{text: "up/down select", active: true},
			{text: "enter edit/open"},
			{text: "esc back"},
		}
	default:
		return []commandChip{
			{text: "enter attach", active: true},
			{text: "n spawn"},
			{text: ": cmd"},
			{text: "/ filter"},
			{text: "R refresh"},
			{text: "tab details"},
			{text: "v style"},
			{text: "c renew cred"},
			{text: "x stop"},
			{text: "d delete"},
			{text: "a auth"},
			{text: "? help"},
			{text: "q quit"},
		}
	}
}

func (m Model) renderCommandMenu() string {
	// The header is a ramp of grounds rather than a set of hues: ordinary chips
	// sit on the darkest, the active key is inverted, and the two labels are
	// inverted a step brighter still. Rank by brightness leaves the one colour
	// for the one meaning.
	labelStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(colOnDark)).
		Background(lipgloss.Color(colInkSoft)).
		Padding(0, 1)
	overflowStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(colMuted)).
		Background(lipgloss.Color(colSunken)).
		Padding(0, 1)

	parts := []string{labelStyle.Render("COMMANDS")}
	for _, chip := range m.headerCommandChips() {
		parts = append(parts, renderCommandChip(chip))
	}

	return fitHeaderSegments(m.contentWidth(), overflowStyle.Render("…"), parts...)
}

// renderModeBadge says which mode has the keys. It is the brightest ground in
// the header because "which keys am I holding" is the first question the header
// answers, and getting it wrong is how a keystroke lands somewhere unintended.
func renderModeBadge(text string) string {
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(colOnDark)).
		Background(lipgloss.Color(colInk)).
		Padding(0, 1).
		Render(text)
}

func renderCommandChip(chip commandChip) string {
	return commandChipStyle(chip).Render(chip.text)
}

// commandChipStyle is split out from the rendering so what a chip means is
// assertable without a terminal: a test binary has no colour profile, so a
// rendered chip comes back as bare text and a wrong ground would go unnoticed.
func commandChipStyle(chip commandChip) lipgloss.Style {
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color(colInk)).
		Background(lipgloss.Color(colRule)).
		Padding(0, 1)
	if chip.active {
		// The same inversion the selected row uses. "The thing the cursor is on"
		// and "the key this mode is for" are the same idea, and one shape for
		// both is one thing to learn.
		style = selectionStyle().Padding(0, 1)
	}
	if chip.attention {
		style = style.Foreground(lipgloss.Color(colOnDark)).Background(lipgloss.Color(colAttention))
	}
	return style
}

// fitHeaderSegments keeps a styled header line inside the main column without
// slicing through ANSI escape sequences mid-chip.
func fitHeaderSegments(width int, overflow string, parts ...string) string {
	if width <= 0 || len(parts) == 0 {
		return ""
	}

	out := make([]string, 0, len(parts))
	for i, part := range parts {
		candidate := append(append([]string(nil), out...), part)
		if lipgloss.Width(strings.Join(candidate, " ")) <= width {
			out = candidate
			continue
		}
		if overflow == "" || i == 0 {
			break
		}

		withOverflow := append(append([]string(nil), out...), overflow)
		for len(out) > 0 && lipgloss.Width(strings.Join(withOverflow, " ")) > width {
			out = out[:len(out)-1]
			withOverflow = append(append([]string(nil), out...), overflow)
		}
		if lipgloss.Width(strings.Join(withOverflow, " ")) <= width {
			out = withOverflow
		}
		break
	}

	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ")
}
