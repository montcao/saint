package tui

import (
	"strings"
	"testing"
)

func TestSanitizeInlineStripsEscapeSequences(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "hello world", want: "hello world"},
		{name: "csi colour", in: "\x1b[31mred\x1b[0m", want: "red"},
		{
			// The one that reached the session list: tmux's DCS passthrough
			// wrapping a background-colour query. Stripping control characters
			// first turned this into the text "Ptmux;]11;?\]11;?".
			name: "tmux passthrough of an osc query",
			in:   "\x1bPtmux;\x1b\x1b]11;?\x07\x1b\\",
			want: "",
		},
		{name: "osc terminated by bel", in: "before\x1b]0;a title\x07after", want: "beforeafter"},
		{name: "osc terminated by st", in: "before\x1b]11;?\x1b\\after", want: "beforeafter"},
		{name: "bare osc query", in: "\x1b]11;?\x07", want: ""},
		{name: "charset designation", in: "\x1b(Btext", want: "text"},
		{name: "string terminator alone", in: "a\x1b\\b", want: "ab"},
		{
			// A log cut mid-sequence has no terminator. Everything after the
			// introducer is sequence, not text.
			name: "unterminated osc",
			in:   "output\x1b]11;rgb:0000/000",
			want: "output",
		},
		{name: "carriage returns and tabs", in: "a\rb\tc", want: "abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sanitizeInline(tc.in); got != tc.want {
				t.Errorf("sanitizeInline(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever the input, no escape introducer may survive into the UI.
func TestSanitizeInlineLeavesNoEscapeBytes(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"\x1bPtmux;\x1b\x1b]11;?\x07\x1b\\",
		"\x1b]11;?\x1b\\\x1b]10;?\x07",
		"\x1b[?2004h\x1b[?25l\x1b[1;1H\x1b[K",
		"\x1b_apc payload\x1b\\",
		"\x1b^pm payload\x1b\\",
		"\x1bXsos\x1b\\",
	}

	for _, in := range inputs {
		got := sanitizeInline(in)
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("sanitizeInline(%q) = %q, still contains ESC", in, got)
		}
		for _, marker := range []string{"tmux;", "11;?", "2004h", "payload"} {
			if strings.Contains(got, marker) {
				t.Errorf("sanitizeInline(%q) = %q, leaked sequence payload %q", in, got, marker)
			}
		}
	}
}
