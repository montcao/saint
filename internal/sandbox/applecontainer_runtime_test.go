package sandbox

import (
	"strings"
	"testing"
)

func TestRuntimeCommandClaudeFresh(t *testing.T) {
	t.Parallel()

	got, err := runtimeCommand(RuntimeClaude, "/repos/app", CreateOptions{
		Name:            "demo",
		Prompt:          "fix the failing tests",
		SkipPermissions: true,
	})
	if err != nil {
		t.Fatalf("runtimeCommand() error = %v", err)
	}
	for _, fragment := range []string{
		"'claude'",
		"'--dangerously-skip-permissions'",
		"'--name'",
		"'demo'",
		"'fix the failing tests'",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("runtimeCommand() = %q, missing %q", got, fragment)
		}
	}
}

func TestRuntimeCommandCodexResume(t *testing.T) {
	t.Parallel()

	got, err := runtimeCommand(RuntimeCodex, "/repos/app", CreateOptions{
		Resume:          true,
		SkipPermissions: true,
	})
	if err != nil {
		t.Fatalf("runtimeCommand() error = %v", err)
	}
	for _, fragment := range []string{
		"'codex'",
		"'resume'",
		"'--last'",
		"'--ask-for-approval'",
		"'never'",
		"'--sandbox'",
		"'workspace-write'",
		"'/repos/app'",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("runtimeCommand() = %q, missing %q", got, fragment)
		}
	}
}

func TestRuntimeBootstrapScriptTargetsTranscriptPane(t *testing.T) {
	t.Parallel()

	got, err := runtimeBootstrapScript(RuntimeClaude, "/repos/app", CreateOptions{})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}
	for _, fragment := range []string{
		"tmux new-session -d -s agent -x 220 -y 60",
		`tmux pipe-pane -o -t agent:0.0 "cat >> /saint/transcript.log"`,
		// The agent is the session's command now, not typed into a shell, and
		// PID 1 waits on the session rather than sleeping forever.
		"has-session -t agent",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("runtimeBootstrapScript() = %q, missing %q", got, fragment)
		}
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	t.Parallel()

	got := shellQuote("it's fine")
	want := `'it'"'"'s fine'`
	if got != want {
		t.Fatalf("shellQuote() = %q, want %q", got, want)
	}
}
