package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The first-run surface: what saint shows before there is anything to show.
//
// This frame is worth more than its share of the codebase. A developer who
// types `saint` for the first time forms an opinion of the whole program in
// the first few seconds, and then repeats that opinion to other people. What
// they must not form is the other thing: an impression that they can press `n`
// now and sort the rest out later. They cannot. A session created before a
// credential is stored starts perfectly, runs for a few seconds, and then dies
// on the agent's first request - after the container is up, after the repo is
// mounted, at the point furthest from the cause. The empty state exists mostly
// to move that discovery forward by ninety seconds.
//
// So this surface is a readout, not a welcome. It says what saint is, it says
// what is and is not ready on *this* machine, and it names the one step that
// is standing in the way. A generic greeting that would render identically on
// a fully configured machine and a machine with no container runtime would be
// decoration, and decoration that restates is the thing this design system
// deletes on sight.

// firstRunReadiness is a three-valued answer about one precondition, and its
// zero value is "unknown".
//
// A bool would have been smaller and wrong. A caller that has not finished
// probing the Apple `container` CLI hands a bool `false`, and the surface then
// tells a new user their machine is missing a runtime that is sitting on it -
// a confident wrong statement, which this codebase treats as worse than a gap.
// "not checked" and "not there" are different facts and this type refuses to
// collapse them, for the same reason the context bar shows no percentage for a
// model whose window nobody has verified.
//
// The zero value being the honest one matters: a caller that forgets a field
// gets `unknown`, never a fabricated `ready`.
type firstRunReadiness int

const (
	// firstRunUnknown means saint has not established this one way or the
	// other. It is never rendered as a problem, because it is not known to be
	// one, and it is never rendered as ready either.
	firstRunUnknown firstRunReadiness = iota
	firstRunReady
	firstRunMissing
)

// firstRunState is everything the first-run surface is allowed to claim.
//
// Deliberately three flags and no strings: anything richer would tempt a caller
// into passing a message through, and then the words a new user reads would be
// assembled somewhere else, out of sight of this file's reasoning about which
// step has to come first.
type firstRunState struct {
	// Credential is whether saint holds a credential that a new session could
	// carry upstream. This is the one the user owns, and the one whose absence
	// fails late and confusingly, so it is checked before the others.
	Credential firstRunReadiness

	// Proxy is whether the local hop every request crosses is running. It is
	// both the credential boundary and the meter, so a session without it
	// cannot reach the API and nothing it did would be counted.
	Proxy firstRunReadiness

	// Runtime is whether the Apple `container` CLI was found on this host.
	// Discovered at runtime, never assumed from a version string: see the
	// project rule about nothing machine-local.
	Runtime firstRunReadiness
}

// Composition widths. The block is centred in the terminal but left-aligned
// inside itself: one straight left edge reads as composed, where centred prose
// over a centred table reads as a splash screen, which this is not.
const (
	// firstRunBlockWidth is the widest the whole block is allowed to get. Past
	// this the block stays put and the margins grow, because a line of prose
	// stretched across a 200-column terminal is measured in eye movements, not
	// in columns.
	firstRunBlockWidth = 77

	// firstRunReadoutWidth caps the label/value rows. They are narrower than
	// the block on purpose: the label/value convention puts the value hard
	// right, and hard right at 77 columns is a chasm with a word at each end.
	firstRunReadoutWidth = 42

	// firstRunProseWidth caps the explanation. Around sixty columns is where a
	// paragraph stops needing the reader to find the start of the next line.
	firstRunProseWidth = 64
)

// firstRunStep is one precondition, with the words for each of its three
// states already attached.
//
// The words live here rather than at the render site so that no state can be
// rendered as colour alone: every branch below has a string, which is what
// makes the surface survive a terminal theme that paints every ANSI index the
// same grey.
type firstRunStep struct {
	label   string
	ready   string
	missing string
	at      firstRunReadiness

	// headline is what the whole surface says when this step is the one in the
	// way, and why is the consequence of ignoring it.
	headline string
	why      string
}

// value is the step's state in words. Never a number, never a symbol.
func (s firstRunStep) value() string {
	switch s.at {
	case firstRunReady:
		return s.ready
	case firstRunMissing:
		return s.missing
	default:
		return "unknown"
	}
}

// steps lists the preconditions in the order a person has to deal with them.
//
// Credential is first, and that ordering is the entire point of this surface.
// The runtime and the proxy fail loudly and immediately; the credential fails
// quietly and late. Putting it first also matches the order of the two keys a
// new user is about to press - `a` then `n` - which is the ordering the old
// one-line empty state already encoded and which must not be lost here.
func (s firstRunState) steps() []firstRunStep {
	return []firstRunStep{
		{
			label:    "credential",
			ready:    "stored",
			missing:  "none stored",
			at:       s.Credential,
			headline: "no credential stored",
			why: "a stores one, once per install. Do that before n: a session " +
				"created without a credential starts, mounts the repo, and then " +
				"fails on the agent's first request.",
		},
		{
			label:    "container runtime",
			ready:    "found",
			missing:  "not found",
			at:       s.Runtime,
			headline: "Apple container runtime not found",
			why: "saint runs every session in its own microVM, so nothing can " +
				"start until the container CLI is installed. saint doctor reports " +
				"what it looked for.",
		},
		{
			label:    "proxy",
			ready:    "running",
			missing:  "not running",
			at:       s.Proxy,
			headline: "proxy not running",
			why: "Every request crosses the proxy: it is where the credential " +
				"stays and where the tokens are counted. A session cannot reach " +
				"the API without it.",
		},
	}
}

// blocker returns the one step standing between the user and a working session.
//
// One, not all of them. Marking every unready step would spend the interface's
// single saturated colour three times on a machine that has been installed but
// not set up, and a frame with three urgent things in it has none. `unknown`
// never blocks: saint has not looked, so it has nothing to assert.
func (s firstRunState) blocker() (firstRunStep, bool) {
	for _, step := range s.steps() {
		if step.at == firstRunMissing {
			return step, true
		}
	}
	return firstRunStep{}, false
}

// unchecked names the steps saint has not established either way.
func (s firstRunState) unchecked() []string {
	var names []string
	for _, step := range s.steps() {
		if step.at == firstRunUnknown {
			names = append(names, step.label)
		}
	}
	return names
}

// guidance is the sentence the surface exists to deliver, and whether it is the
// kind that needs a person.
//
// Three outcomes, and the third is the one that is easy to get wrong. When
// nothing is missing but something was never checked, the honest answer is
// neither "you are blocked" nor "you are ready" - it is that saint does not
// know, said plainly, with the credential ordering still stated because that
// advice is true regardless of what the probe would have returned.
func (s firstRunState) guidance() (headline, why string, needsPerson bool) {
	if step, blocked := s.blocker(); blocked {
		return step.headline, step.why, true
	}

	if names := s.unchecked(); len(names) > 0 {
		return "readiness unknown: " + strings.Join(names, ", "),
			"saint has not checked these yet, which is not the same as ready. " +
				"Store a credential before you spawn a session either way: a " +
				"session created without one fails on the agent's first request.",
			false
	}

	return "",
		"n spawns a session in its own microVM. It carries the credential saint " +
			"already holds, so it never asks you for one.",
		false
}

// firstRunTagline says what saint is, in one line, at whatever width there is.
//
// Three fixed variants rather than one sentence wrapped or truncated. A
// truncated tagline ("a control plane for AI coding agents, each in its o…")
// is a worse first impression than a shorter true one, and a tagline that
// wraps to two lines stops being a tagline. Every claim here is one the README
// already makes; none of them are performance, adoption, or safety claims.
func firstRunTagline(width int) string {
	switch {
	case width >= 77:
		return "a control plane for AI coding agents, each in its own Apple container microVM"
	case width >= 51:
		return "AI coding agents, each in its own container microVM"
	case width >= 27:
		return "AI coding agents, sandboxed"
	default:
		return ""
	}
}

// firstRunKeys are the keys this surface is about, most useful first.
//
// Keys, not intents: `n spawn`, never "create a new session". The reader is
// looking at a keyboard, and the gap between "create a session" and the key
// that does it is a gap they have to cross themselves.
var firstRunKeys = []struct{ key, word string }{
	{"a", "auth"},
	{"n", "spawn"},
	{"?", "keys"},
	{"q", "quit"},
}

// firstRunSpan is a run of text with the style it is drawn in.
//
// Styling happens last, after every measurement and cut, because truncating a
// string that already carries ANSI escapes slices the escape sequence and
// leaves the terminal painted in whatever colour the fragment ended on - which
// then bleeds down the rest of the frame.
type firstRunSpan struct {
	text  string
	style lipgloss.Style
}

// firstRunCompose joins spans, cutting the plain text to a total budget.
func firstRunCompose(width int, spans ...firstRunSpan) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	left := width
	for _, span := range spans {
		if left <= 0 {
			break
		}
		text := span.text
		if lipgloss.Width(text) > left {
			text = truncateString(text, left)
		}
		left -= lipgloss.Width(text)
		b.WriteString(span.style.Render(text))
	}
	return b.String()
}

// firstRunWrap breaks text into lines no wider than width.
//
// Deliberate layout, not terminal wrapping. The distinction matters: lines
// authored to a width the renderer chose are a paragraph, while lines the
// terminal folded because they overran are a corrupted frame, and in the
// alternate screen the two are told apart only by whether anyone checked.
func firstRunWrap(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	var (
		lines []string
		line  string
	)
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	// A single word longer than the measure still has to fit the frame.
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = truncateString(l, width)
		}
	}
	return lines
}

// firstRunSection is a run of lines with the order it gets sacrificed in.
//
// Short terminals are the common case rather than the edge - saint is usually
// in a split pane - so the surface has to have an opinion about what it loses
// first. It drops whole sections, never squeezes them, which is the same rule
// the columns follow: a panel that is gone is legible, and a panel that has
// been compressed to fit is not.
type firstRunSection struct {
	lines []string

	// drop is the sacrifice order, highest first. The key row is rank zero: a
	// frame that tells a new user nothing about how to proceed is worse than
	// any amount of missing explanation.
	drop int
}

const (
	firstRunDropKeys = iota
	firstRunDropHeadline
	firstRunDropWhy
	firstRunDropTagline
	firstRunDropReadout
	firstRunDropSpacer
)

// renderFirstRun draws the surface shown when there are no sessions yet.
//
// width and height are the box to fill: the result is centred in both axes and
// is exactly height lines tall when height is positive, so the caller can drop
// it into a fixed region without the frame appearing to come apart. Pass the
// height that is actually free, not the terminal's, or the surface will push
// the status bar off the bottom.
func renderFirstRun(width, height int, state firstRunState) string {
	if width <= 0 {
		return ""
	}

	block := min(width, firstRunBlockWidth)
	readout := min(block, firstRunReadoutWidth)
	prose := min(block, firstRunProseWidth)

	taglineStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkBright))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInk))
	proseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkBright))
	plain := lipgloss.NewStyle()

	var sections []firstRunSection

	if tagline := firstRunTagline(block); tagline != "" {
		sections = append(sections,
			firstRunSection{lines: []string{firstRunCompose(block, firstRunSpan{tagline, taglineStyle})}, drop: firstRunDropTagline},
			firstRunSection{lines: []string{""}, drop: firstRunDropSpacer},
		)
	}

	rows := make([]string, 0, len(state.steps()))
	for _, step := range state.steps() {
		value := step.value()
		label := truncateString(step.label, max(0, readout-lipgloss.Width(value)-1))
		gap := max(1, readout-lipgloss.Width(label)-lipgloss.Width(value))
		rows = append(rows, firstRunCompose(readout,
			firstRunSpan{label, labelStyle},
			firstRunSpan{strings.Repeat(" ", gap), plain},
			firstRunSpan{value, valueStyle},
		))
	}
	sections = append(sections,
		firstRunSection{lines: rows, drop: firstRunDropReadout},
		firstRunSection{lines: []string{""}, drop: firstRunDropSpacer},
	)

	headline, why, needsPerson := state.guidance()
	if headline != "" {
		// The interface has exactly one saturated colour and it means one
		// thing: this needs a person. It is spent here, on the single blocking
		// step, and nowhere else on this surface - so a machine that is ready
		// renders with no colour at all, which is readable across a room. The
		// headline is also a sentence, so a terminal that renders every index
		// as the same grey loses the emphasis and keeps the meaning.
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
		if needsPerson {
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colAttention))
		}
		sections = append(sections, firstRunSection{
			lines: firstRunWrapStyled(headline, prose, style),
			drop:  firstRunDropHeadline,
		})
	}
	if why != "" {
		sections = append(sections, firstRunSection{
			lines: firstRunWrapStyled(why, prose, proseStyle),
			drop:  firstRunDropWhy,
		})
	}

	sections = append(sections,
		firstRunSection{lines: []string{""}, drop: firstRunDropSpacer},
		firstRunSection{lines: []string{firstRunKeyRow(block, keyStyle, labelStyle, plain)}, drop: firstRunDropKeys},
	)

	lines := firstRunFit(sections, height)

	// Centre the block horizontally. Blank lines stay genuinely blank rather
	// than becoming a run of spaces, so that a copied frame does not carry
	// trailing whitespace into wherever it is pasted.
	margin := strings.Repeat(" ", max(0, (width-block)/2))
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = margin + line
	}

	if height > 0 {
		// Centre vertically, then pad to exactly height so the surrounding
		// frame keeps its shape.
		top := max(0, (height-len(lines))/2)
		padded := make([]string, 0, height)
		for i := 0; i < top; i++ {
			padded = append(padded, "")
		}
		padded = append(padded, lines...)
		for len(padded) < height {
			padded = append(padded, "")
		}
		lines = padded[:height]
	}

	// Belt and braces over the arithmetic above. Every construction path here
	// already fits, but this surface is the one a new user sees, and a single
	// over-wide line makes lipgloss pad every other line out to match it,
	// wrapping the whole frame.
	return clampWidth(strings.Join(lines, "\n"), width)
}

// firstRunWrapStyled wraps text and applies one style to each resulting line.
func firstRunWrapStyled(text string, width int, style lipgloss.Style) []string {
	wrapped := firstRunWrap(text, width)
	for i, line := range wrapped {
		wrapped[i] = firstRunCompose(width, firstRunSpan{line, style})
	}
	return wrapped
}

// firstRunKeyRow renders the keys, dropping from the right as room runs out.
//
// Right-to-left because the list is already in order of what a new user needs:
// losing `q quit` on a narrow terminal costs nothing, since quitting is bound
// in several places and documented everywhere, while losing `a auth` would
// leave the blocking step named with no way to act on it.
func firstRunKeyRow(width int, keyStyle, wordStyle, plain lipgloss.Style) string {
	const gap = "   "

	keys := firstRunKeys
	for len(keys) > 1 {
		total := 0
		for i, k := range keys {
			if i > 0 {
				total += len(gap)
			}
			total += lipgloss.Width(k.key) + 1 + lipgloss.Width(k.word)
		}
		if total <= width {
			break
		}
		keys = keys[:len(keys)-1]
	}

	spans := make([]firstRunSpan, 0, len(keys)*4)
	for i, k := range keys {
		if i > 0 {
			spans = append(spans, firstRunSpan{gap, plain})
		}
		spans = append(spans,
			firstRunSpan{k.key, keyStyle},
			firstRunSpan{" ", plain},
			firstRunSpan{k.word, wordStyle},
		)
	}
	return firstRunCompose(width, spans...)
}

// firstRunFit drops whole sections until the block fits the available height.
func firstRunFit(sections []firstRunSection, height int) []string {
	dropped := make([]bool, len(sections))

	count := func() int {
		n := 0
		for i, section := range sections {
			if !dropped[i] {
				n += len(section.lines)
			}
		}
		return n
	}

	for height > 0 && count() > height {
		// Find the surviving section with the highest drop rank. Ties go to the
		// last one, so a trailing spacer goes before a leading one and the block
		// loses its bottom before its top.
		worst, rank := -1, -1
		for i, section := range sections {
			if !dropped[i] && section.drop >= rank {
				worst, rank = i, section.drop
			}
		}
		if worst < 0 || rank <= firstRunDropKeys {
			break
		}
		dropped[worst] = true
	}

	var lines []string
	for i, section := range sections {
		if dropped[i] {
			continue
		}
		lines = append(lines, section.lines...)
	}

	// Dropping a section can strand a blank line at either end.
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return lines
}

// firstRun reads the model's readiness for the first-run screen.
//
// Every branch distinguishes "saint has not checked" from "saint checked and it
// is not there". They look identical to someone writing this code and are
// completely different to someone reading the screen: one says do nothing yet,
// the other says do something now.
func (m Model) firstRun() firstRunState {
	state := firstRunState{}

	switch {
	case m.auth.loadErr != nil:
		// saint tried and could not tell. That is not the same as absent, and
		// reporting it as absent would send the user to fix a credential that
		// may be perfectly fine.
		state.Credential = firstRunUnknown
	case len(m.auth.states) == 0:
		// The credential probe has not reported yet.
		state.Credential = firstRunUnknown
	default:
		state.Credential = firstRunMissing
		for _, candidate := range m.auth.states {
			if candidate.Present {
				state.Credential = firstRunReady
				break
			}
		}
	}

	// The proxy's own status line is the authority, because it already
	// distinguishes bound-and-reachable from bound-but-firewalled.
	switch {
	case strings.TrimSpace(m.proxyStatus) == "":
		state.Proxy = firstRunUnknown
	case strings.HasPrefix(m.proxyStatus, "proxy: "):
		state.Proxy = firstRunReady
	default:
		state.Proxy = firstRunMissing
	}

	// Listing the fleet is the only thing that exercises the container runtime,
	// so a successful snapshot is the evidence that it is there.
	switch {
	case m.sawSnapshot:
		state.Runtime = firstRunReady
	case m.lastError != nil:
		state.Runtime = firstRunMissing
	default:
		state.Runtime = firstRunUnknown
	}

	return state
}
