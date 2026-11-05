package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// screen renders lines the way a terminal would hand them to the detector.
func screen(lines ...string) []string { return lines }

// A permission prompt is two signals, never one. Every individual phrase is
// something an agent also prints while explaining itself - which is not a
// hypothetical: the only occurrence of "to confirm" in a real 472 KB transcript
// was inside documentation the agent had written about something else.
func TestBlockedNeedsTheShapeOfAControlNotAPhrase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		screen []string
		want   bool
	}{
		{
			name:   "a confirm footer",
			screen: screen("Edit file src/main.go?", "Enter to confirm · Esc to cancel"),
			want:   true,
		},
		{
			name:   "a selection footer",
			screen: screen("Choose a branch", "Enter to select · Esc to cancel"),
			want:   true,
		},
		{
			name: "a permission question with a cursor",
			screen: screen(
				"Do you want to proceed?",
				"❯ 1. Yes",
				"  2. No, and tell Claude what to do differently",
			),
			want: true,
		},
		{
			// The agent explaining a prompt is not stopped at one.
			name:   "prose about confirming",
			screen: screen("The dialog shows Enter to confirm when it wants a decision."),
			want:   false,
		},
		{
			// A rhetorical question with no control under it.
			name:   "a question with no options",
			screen: screen("Do you want me to continue with the refactor?"),
			want:   false,
		},
		{
			name:   "an ordinary prompt",
			screen: screen("> ", "? for shortcuts"),
			want:   false,
		},
		{
			name:   "nothing drawn",
			screen: screen("", "   ", ""),
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := blockedReason(tc.screen) != ""
			if got != tc.want {
				t.Errorf("blockedReason(%q) reported blocked=%v, want %v", tc.screen, got, tc.want)
			}
		})
	}
}

// An agent that is generating is not waiting, whatever else is on screen. The
// interrupt hint shares a footer with the controls a prompt draws, so without
// this a screen carrying both reads as blocked.
func TestAGeneratingAgentIsNeverBlocked(t *testing.T) {
	t.Parallel()

	got := blockedReason(screen(
		"✳ Thinking… (12s · esc to interrupt)",
		"Enter to confirm · Esc to cancel",
	))
	if got != "" {
		t.Errorf("blockedReason() = %q while the agent was generating", got)
	}
}

// Only the bottom of the screen counts. A prompt lives in the footer; prose
// about prompts can be anywhere, and in the transcript this was built from, was.
func TestOnlyTheFooterIsRead(t *testing.T) {
	t.Parallel()

	lines := []string{"Enter to confirm · Esc to cancel"}
	for i := 0; i < blockedFooterLines+6; i++ {
		lines = append(lines, "some later output line")
	}
	if got := blockedReason(lines); got != "" {
		t.Errorf("blockedReason() = %q from a phrase scrolled far above the footer", got)
	}
}

// The transcript is a raw byte stream painted at absolute cursor positions, so
// it has to go through an emulator before any rule can look at it. This asserts
// the reconstruction, not the rules.
func TestTranscriptIsReconstructedThroughTheEmulator(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.log")
	// Write a line, then move the cursor home and overwrite it. A reader that
	// searched the bytes would find both; only the second is on screen.
	raw := "stale content that is no longer visible\r\n" +
		"\x1b[H\x1b[2J" +
		"Enter to confirm \xc2\xb7 Esc to cancel\r\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	lines, ok := transcriptScreen(path, 80, 24)
	if !ok {
		t.Fatal("transcriptScreen() could not rebuild the screen")
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "stale content") {
		t.Error("the reconstruction kept content the agent had cleared")
	}
	if blockedReason(lines) == "" {
		t.Errorf("the rebuilt screen did not read as blocked:\n%s", joined)
	}
}

// A missing or empty transcript is not evidence of anything.
func TestAnAbsentTranscriptClaimsNothing(t *testing.T) {
	t.Parallel()

	if _, ok := transcriptScreen(filepath.Join(t.TempDir(), "absent.log"), 80, 24); ok {
		t.Error("transcriptScreen() claimed a screen for a file that is not there")
	}

	empty := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, ok := transcriptScreen(empty, 80, 24); ok {
		t.Error("transcriptScreen() claimed a screen for an empty transcript")
	}
}

// A stopped container's transcript still ends on whatever was last drawn, which
// would leave a dead session reporting a prompt nobody can answer.
func TestOnlyRunningSessionsAreInspected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.log")
	if err := os.WriteFile(path, []byte("Enter to confirm \xc2\xb7 Esc to cancel\r\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if got := sessionBlocked("running", path); got == "" {
		t.Error("a running session at a prompt was not reported as blocked")
	}
	for _, state := range []string{"stopped", "missing", "orphan (running)"} {
		if got := sessionBlocked(state, path); got != "" {
			t.Errorf("state %q reported %q; only a running session can be waiting", state, got)
		}
	}
}

// Measured beats inferred. A request in flight is a fact; a screen that looks
// like a prompt is a pattern match, and the two must not be presented with the
// same confidence.
func TestMeasuredWorkingOutranksAnInferredPrompt(t *testing.T) {
	t.Parallel()

	if got := blockedLabel("working", "waiting to confirm"); got != "working" {
		t.Errorf("blockedLabel() = %q; a measured request in flight outranks a screen pattern", got)
	}
	if got := blockedLabel("idle", "waiting to confirm"); got != "blocked" {
		t.Errorf("blockedLabel() = %q, want blocked", got)
	}
	if got := blockedLabel("idle", ""); got != "idle" {
		t.Errorf("blockedLabel() = %q, want idle", got)
	}
	// "-" means nothing is measuring. Inference must not quietly upgrade that
	// into a claim.
	if got := blockedLabel("-", ""); got != "-" {
		t.Errorf("blockedLabel() = %q, want the unmeasured state preserved", got)
	}
}

// A device query in the replayed bytes makes the emulator answer into a
// synchronous pipe, and with nobody reading it the write never returns. That is
// a frozen interface, not a slow one, so the queries are removed before replay.
func TestDeviceQueriesAreStrippedBeforeReplay(t *testing.T) {
	t.Parallel()

	queries := []string{
		"\x1b[6n",           // cursor position report
		"\x1b[?2026$p",      // DECRQM, private
		"\x1b[c",            // primary device attributes
		"\x1b[>c",           // secondary device attributes
		"\x1b[?1;2n",        // DSR, DEC private
		"\x1b]11;?\x07",     // background colour query
		"\x1bP+q544e\x1b\\", // XTGETTCAP
	}
	for _, query := range queries {
		if got := stripDeviceQueries([]byte("before" + query + "after")); string(got) != "beforeafter" {
			t.Errorf("stripDeviceQueries(%q) = %q, want the query removed", query, got)
		}
	}

	// And it must not eat ordinary output on the way past.
	keep := "\x1b[1;32mgreen\x1b[m text \x1b[?1049h\x1b[?1002h"
	if got := string(stripDeviceQueries([]byte(keep))); got != keep {
		t.Errorf("stripDeviceQueries() altered ordinary output:\n got %q\nwant %q", got, keep)
	}
}

// The whole point of the stripping: a transcript carrying a query must still
// rebuild, and must do so promptly.
func TestATranscriptFullOfQueriesStillRebuilds(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.log")
	var raw strings.Builder
	for i := 0; i < 200; i++ {
		raw.WriteString("\x1b[6n\x1b[c\x1b[?2026$p")
	}
	raw.WriteString("\x1b[H\x1b[2JEnter to confirm \xc2\xb7 Esc to cancel\r\n")
	if err := os.WriteFile(path, []byte(raw.String()), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	done := make(chan []string, 1)
	go func() {
		lines, _ := transcriptScreen(path, 80, 24)
		done <- lines
	}()

	select {
	case lines := <-done:
		if blockedReason(lines) == "" {
			t.Errorf("a transcript full of queries did not rebuild into a blocked screen:\n%s",
				strings.Join(lines, "\n"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rebuilding a transcript containing device queries hung")
	}
}
