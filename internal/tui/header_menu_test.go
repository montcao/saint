package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The header is the one surface that is on screen in every mode, from launch to
// exit, so a permanent chip in the attention colour is the fastest way to spend
// that colour down to nothing. "d delete" and "ctrl+c quit" were amber in the
// key list of modes where nothing was being deleted and nothing was quitting -
// the same amber a credential about to lapse arrives in.
//
// So it is spent on one chip, in one mode, at the moment the destructive act is
// the next keystroke rather than a key that exists.
func TestOnlyThePendingDestructiveKeyGetsTheAttentionColour(t *testing.T) {
	t.Parallel()

	calm := []uiMode{modeList, modeFilter, modeNewSession, modeCommand, modeAuth, modeRail, modeProxy, modeAttached}
	for _, mode := range calm {
		m := Model{mode: mode}
		for _, chip := range m.headerCommandChips() {
			if chip.attention {
				t.Errorf("mode %v renders %q in the attention colour while nothing is pending", mode, chip.text)
			}
		}
	}

	m := Model{mode: modeConfirmDelete}
	marked := 0
	for _, chip := range m.headerCommandChips() {
		if !chip.attention {
			continue
		}
		marked++
		// Colour is never the only carrier: the chip names the key and what it
		// does, so a terminal that renders 214 as anything at all still reads.
		if !strings.Contains(chip.text, "confirm") {
			t.Errorf("the marked chip is %q, want the one that confirms the deletion", chip.text)
		}
	}
	if marked != 1 {
		t.Errorf("the delete confirmation marks %d chips, want exactly the one that destroys something", marked)
	}
}

// Asserting the meaning rather than the index, so a change to what the palette
// calls attention moves this test with it instead of breaking it.
func TestCommandChipsRenderFromThePalette(t *testing.T) {
	t.Parallel()

	attention := commandChipStyle(commandChip{text: "enter confirm", attention: true})
	if got := attention.GetBackground(); got != lipgloss.Color(colAttention) {
		t.Errorf("a pending destructive chip sits on %v, want the attention colour", got)
	}

	// The active key is inverted rather than coloured: "the key this mode is
	// for" and "the row the cursor is on" are the same idea, so they are the
	// same shape.
	active := commandChipStyle(commandChip{text: "enter attach", active: true})
	if got := active.GetBackground(); got != lipgloss.Color(colSelect) {
		t.Errorf("the active chip sits on %v, want the one selection ground", got)
	}

	ordinary := commandChipStyle(commandChip{text: "q quit"})
	if got := ordinary.GetBackground(); got != lipgloss.Color(colRule) {
		t.Errorf("an ordinary chip sits on %v, want the ambient ground", got)
	}
	if got := ordinary.GetForeground(); got == lipgloss.Color(colAttention) {
		t.Error("an ordinary key hint is drawn in the colour reserved for things needing a person")
	}
}

// The proxy's state is the one fact this header exists to carry, and it must
// survive every width.
//
// It used to be the last chip, so on any terminal under about 130 columns it
// was the first thing replaced by an ellipsis: three counts that were usually
// all zero survived, and "the credential boundary is not working" did not.
func TestProxyStateSurvivesEveryWidth(t *testing.T) {
	t.Parallel()

	for _, status := range []string{
		"proxy: 192.168.64.1:8787 · subscription",
		"proxy blocked: 192.168.64.1:8787 (firewall? run saint doctor)",
		"proxy off: no container network",
	} {
		for width := 60; width <= 200; width += 2 {
			m := proxyModel(t)
			m.width, m.height = width, 40
			m.proxyStatus = status

			first, _, _ := strings.Cut(stripEscapes(m.renderHeader()), "\n")
			if !strings.Contains(first, "proxy") {
				t.Fatalf("width %d: the header does not say what the proxy is doing: %q", width, first)
			}
		}
	}
}

// A blocked proxy is the worst state, not a calm one: it has bound its port and
// answers nothing, so the host sees a listener while no guest request ever
// arrives and the agents fall back to whatever credentials they can find. The
// colour test used to match only the "proxy off" prefix, so this state was
// styled as calmly as a working proxy.
func TestEveryUnhealthyProxyStateNeedsAPerson(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"proxy: 192.168.64.1:8787 · subscription": false,
		"proxy: external 192.168.64.1:8787":       false,
		"proxy blocked: 192.168.64.1:8787":        true,
		"proxy off: no container network":         true,
	}

	for status, want := range cases {
		m := proxyModel(t)
		m.proxyStatus = status
		full, short, needsPerson := m.proxyChip()
		if needsPerson != want {
			t.Errorf("%q: needsPerson = %v, want %v", status, needsPerson, want)
		}
		if full == "" || short == "" {
			t.Errorf("%q: produced an empty chip (%q / %q)", status, full, short)
		}
		// The short form still has to answer the question in words.
		if !strings.Contains(short, "proxy") {
			t.Errorf("%q: short form %q does not name the proxy", status, short)
		}
	}

	m := proxyModel(t)
	m.proxyStatus = ""
	if full, _, _ := m.proxyChip(); full != "" {
		t.Errorf("an unset proxy status produced a chip %q; unknown is not a state to render", full)
	}
}

// Amber on a count of zero spends the interface's one saturated colour on the
// absence of a problem.
func TestACountOfZeroIsNotAnAlarm(t *testing.T) {
	t.Parallel()

	calm := lipgloss.NewStyle().Foreground(lipgloss.Color(colInk))
	attention := lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))

	if got := countChip(calm, attention, "missing", 0); strings.Contains(got, "\x1b") &&
		!strings.Contains(got, colInk) {
		t.Errorf("missing 0 was not rendered calmly: %q", got)
	}
	if got, want := countChip(calm, attention, "missing", 0), "missing 0"; !strings.Contains(stripEscapes(got), want) {
		t.Errorf("countChip() = %q, want it to contain %q", got, want)
	}
	if got, want := countChip(calm, attention, "missing", 2), "missing 2"; !strings.Contains(stripEscapes(got), want) {
		t.Errorf("countChip() = %q, want it to contain %q", got, want)
	}
}
