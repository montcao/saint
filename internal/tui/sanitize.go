package tui

import (
	"regexp"
	"strings"
)

// escapeSequences strips terminal escape sequences from text that is about to
// be shown as a plain string.
//
// Order matters. The multi-byte forms have to be removed before the two-byte
// catch-all, which would otherwise match only their introducer and leave the
// payload behind as text. That is exactly how a background-colour query from an
// agent - tmux's DCS passthrough wrapping an OSC, `ESC P tmux; ESC ] 11 ; ? ESC \` -
// used to reach the session list as the string "Ptmux;]11;?\]11;?".
var escapeSequences = []*regexp.Regexp{
	// CSI: ESC [ params intermediates final
	regexp.MustCompile(`\x1b\[[0-9;:<=>?]*[ -/]*[@-~]`),
	// DCS, SOS, PM, APC: introducer, payload, string terminator. Non-greedy,
	// because the payload can itself contain escapes - tmux passthrough works by
	// wrapping them.
	regexp.MustCompile(`\x1b[P_^X][\s\S]*?(?:\x1b\\|\x07)`),
	// OSC: ESC ] payload, terminated by BEL or ST.
	regexp.MustCompile(`\x1b\][\s\S]*?(?:\x1b\\|\x07)`),
	// The same introducers with no terminator, which is what a truncated log or
	// a half-written line looks like. Everything after them is sequence, not
	// text.
	regexp.MustCompile(`\x1b[P_^X\]][\s\S]*$`),
	// Charset designation: ESC ( B and friends.
	regexp.MustCompile(`\x1b[()*+][ -/]*[0-~]`),
	// Anything else introduced by ESC, including the bare string terminator.
	regexp.MustCompile(`\x1b[ -/]*[0-~]`),
}

// stripEscapes removes escape sequences, leaving only text.
func stripEscapes(value string) string {
	for _, pattern := range escapeSequences {
		value = pattern.ReplaceAllString(value, "")
	}
	return value
}

func sanitizeInline(value string) string {
	// Escapes first, control characters second. Doing it the other way round
	// deletes the ESC that marks a sequence and promotes the rest of it to
	// text, which is worse than leaving it alone.
	value = stripEscapes(value)

	var b strings.Builder
	for _, r := range value {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// statusLine renders an arbitrary error or message as a single bounded line fit
// for the status bar: control characters and ANSI escapes stripped, whitespace
// collapsed, and length capped so a long CLI error cannot flood the view.
func statusLine(value string) string {
	const maxLen = 140

	value = strings.Join(strings.Fields(sanitizeInline(value)), " ")
	if len(value) > maxLen {
		value = strings.TrimSpace(value[:maxLen-3]) + "..."
	}
	return value
}
