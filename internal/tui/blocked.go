package tui

import (
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/montcao/saint/internal/sandbox"
)

// Detecting that an agent is waiting for a person.
//
// saint measures "working" from traffic crossing the proxy, which is fact: a
// request is in flight or it is not. The state that measurement cannot see is
// the one worth the most. An agent stopped at a permission prompt makes no
// requests, so it is indistinguishable from an agent that finished an hour ago,
// and the difference is whether somebody needs to walk over to the keyboard.
//
// The only evidence of it is what the agent has drawn, so this reads the
// screen. That is inference and it is labelled as such everywhere it surfaces:
// a pattern match is an observation with a known failure mode, not a
// measurement, and it must not be presented with the same confidence as the
// in-flight count.
//
// Two things about the source are worth knowing before changing any of this.
//
// The transcript cannot be pattern-matched directly. tmux's pipe-pane writes
// the raw byte stream, and a full-screen agent paints at absolute cursor
// positions, so the file is not lines of text in any order a reader would
// recognise. Searching it for a phrase finds the phrase wherever it last
// happened to be written, not what is on screen. Measured on a real 472 KB
// transcript: the only occurrence of "to confirm" in the whole file was inside
// documentation the agent had printed about something else. So the bytes go
// through an emulator first and the rules run against the reconstructed screen.
//
// That same false positive is why the rules only look at the bottom of the
// screen. A prompt lives in the footer; prose about prompts can be anywhere.

const (
	// blockedTailBytes is how much of a transcript is replayed to rebuild the
	// current screen.
	//
	// A full-screen agent repaints everything it wants visible, so the last
	// screenful of output is enough and older bytes only cost time. This is
	// generous against a wide terminal redrawing with colour, and it is a bound
	// rather than a guess: a transcript grows without limit and reading all of
	// it on every refresh would make the cost of looking at saint depend on how
	// long its sessions had been running.
	blockedTailBytes = 128 << 10

	// blockedFooterLines is how far up from the last drawn line a prompt may
	// be. A permission dialog sits at the bottom; prose about permissions can
	// be anywhere on screen, and was, in the transcript this was built from.
	blockedFooterLines = 14
)

// transcriptScreen replays the tail of a session's transcript and returns the
// screen the agent currently has drawn, as lines.
//
// cols and rows must match what the agent believes it has, or the reconstructed
// screen wraps differently from the real one and the footer this looks at is
// not the footer the user sees.
func transcriptScreen(path string, cols, rows int) ([]string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return nil, false
	}
	if offset := info.Size() - blockedTailBytes; offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, false
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, blockedTailBytes))
	if err != nil || len(data) == 0 {
		return nil, false
	}

	// Replies are removed rather than drained.
	//
	// The emulator answers device queries into a synchronous pipe, so a single
	// query in the replayed bytes blocks the write forever when nobody is
	// reading - a hang, not a slow path, which is why the live pane runs
	// pumpReplies. A reader here cannot be shut down cleanly: SafeEmulator
	// deliberately does not lock Read so that a blocked read cannot deadlock a
	// write, and Close then races that read on a plain bool inside the library.
	// The race detector finds it immediately.
	//
	// So the queries are stripped before the bytes are handed over, and nothing
	// ever writes to the pipe. Nothing is listening for an answer anyway: the
	// agent that asked is not on the other end of this emulator, the real
	// terminal answered it at the time, and the answer is already in the
	// stream.
	emulator := vt.NewEmulator(cols, rows)

	// The write is bounded even so. Stripping is a list, and a list can be
	// missed; a missed query costs one abandoned goroutine on a path that
	// should never run, where the alternative is a frozen interface.
	type result struct {
		rendered string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, err := emulator.Write(stripDeviceQueries(data))
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{rendered: emulator.Render()}
	}()

	var out result
	select {
	case out = <-done:
	case <-time.After(transcriptReplayBudget):
		return nil, false
	}
	if out.err != nil {
		return nil, false
	}

	// Render emits styled output, so the words of a phrase arrive separated by
	// SGR sequences: "Enter to confirm" is drawn as Enter<sgr> to<sgr>
	// confirm<sgr>. Matching before stripping finds nothing, which is a false
	// negative rather than a crash and so stays invisible until someone looks
	// at a session that really is waiting. Measured against two real
	// transcripts that were sitting at a permission prompt.
	lines := strings.Split(out.rendered, "\n")
	for i, line := range lines {
		lines[i] = stripStyle(line)
	}
	return lines, true
}

// blockedReason names why saint believes a session is waiting for a person, or
// is empty when it does not believe that.
//
// The rules are deliberately strict and deliberately paired: a single familiar
// phrase is never enough, because every one of them is something an agent might
// also print while explaining itself. Each rule needs a footer that only an
// interactive control draws.
func blockedReason(lines []string) string {
	footer := lastNonEmpty(lines, blockedFooterLines)
	if len(footer) == 0 {
		return ""
	}
	haystack := strings.ToLower(strings.Join(footer, "\n"))

	// An agent that is generating is not waiting, whatever else is on screen.
	// This outranks every rule below: the interrupt hint is drawn by the same
	// footer a prompt would use, so without it a screen carrying both reads as
	// blocked.
	//
	if strings.Contains(haystack, "esc to interrupt") {
		return ""
	}

	switch {
	case strings.Contains(haystack, "esc to cancel") && strings.Contains(haystack, "enter to confirm"):
		return "waiting to confirm"
	case strings.Contains(haystack, "esc to cancel") && strings.Contains(haystack, "enter to select"):
		return "waiting for a choice"
	case containsChoicePrompt(footer) && hasSelectableOptions(footer):
		return "waiting for permission"
	}
	return ""
}

// containsChoicePrompt reports whether the footer asks a question of the kind
// an agent stops on.
func containsChoicePrompt(footer []string) bool {
	for _, line := range footer {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "do you want") && strings.HasSuffix(lower, "?") {
			return true
		}
	}
	return false
}

// hasSelectableOptions reports whether the footer draws a list the user moves a
// cursor through.
//
// A question alone is not enough. An agent writing "Do you want me to continue?"
// into its own output is asking rhetorically and is not blocked on anything; the
// selection marker or a numbered option list is what only a real control draws.
func hasSelectableOptions(footer []string) bool {
	for _, line := range footer {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// The cursor Claude Code draws beside the active choice.
		if strings.HasPrefix(trimmed, "❯") {
			return true
		}
		// Or a numbered option: "1. Yes", "2. No, and tell ...".
		if len(trimmed) > 2 && trimmed[0] >= '1' && trimmed[0] <= '9' && trimmed[1] == '.' {
			return true
		}
	}
	return false
}

// lastNonEmpty returns up to n of the final lines that have content, oldest
// first.
//
// Trailing blank rows are what a full-screen agent leaves below its footer, so
// counting raw lines from the bottom would spend the whole window on padding.
func lastNonEmpty(lines []string, n int) []string {
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		out = append(out, lines[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Agent screen geometry, matching what the bootstrap gives tmux in
// applecontainer.go. A reconstruction at a different size wraps differently
// from the screen the user is looking at, and the footer stops being the
// footer.
const (
	agentScreenCols = 220
	agentScreenRows = 60
)

// sessionBlocked reports why a session appears to be waiting for a person.
//
// It answers only for running sessions: a stopped container's transcript still
// ends on whatever was last drawn, which would leave a dead session reporting a
// prompt nobody can answer.
func sessionBlocked(state, transcriptPath string) string {
	if state != stateLabel(sandbox.StateRunning) {
		return ""
	}
	lines, ok := transcriptScreen(transcriptPath, agentScreenCols, agentScreenRows)
	if !ok {
		return ""
	}
	return blockedReason(lines)
}

// transcriptReplayBudget bounds rebuilding one screen. Measured at 1-4ms for
// transcripts up to 750 KB, so this is a backstop against a hang rather than a
// deadline anything normal approaches.
const transcriptReplayBudget = 2 * time.Second

// deviceQueries are the sequences that make the emulator answer.
//
// Each is a question a program asks its terminal: report the cursor position,
// identify yourself, is this mode set. The real terminal answered them when the
// session was live, so replaying them here would only produce replies with
// nobody to receive them - and a synchronous pipe with no reader is a hang.
var deviceQueries = []*regexp.Regexp{
	// DSR - cursor position and the DEC private variants.
	regexp.MustCompile(`\x1b\[6n`),
	regexp.MustCompile(`\x1b\[\?[0-9;]*n`),
	// DA1 and DA2 - primary and secondary device attributes.
	regexp.MustCompile(`\x1b\[>?[0-9;]*c`),
	// DECRQM - request mode, both private and ANSI forms.
	regexp.MustCompile(`\x1b\[\??[0-9;]*\$p`),
	// XTGETTCAP and colour queries, which arrive as OSC with a query payload.
	regexp.MustCompile(`\x1b\][0-9]+;\?(\x07|\x1b\\)`),
	regexp.MustCompile(`\x1bP\+q[0-9a-fA-F;]*\x1b\\`),
}

// stripDeviceQueries removes every sequence that would make the emulator reply.
func stripDeviceQueries(data []byte) []byte {
	for _, query := range deviceQueries {
		data = query.ReplaceAll(data, nil)
	}
	return data
}
