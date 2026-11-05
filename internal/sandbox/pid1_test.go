package sandbox

import (
	"strings"
	"testing"
)

// The bootstrap script is the container's init: when it exits, the container
// stops. Under `set -e` any failing line takes the session down before the
// agent has started, and what the user sees is a container that was created and
// is already gone - which is what saint failing to create one also looks like.
func TestBootstrapDoesNotDieOnTheFirstFailedLine(t *testing.T) {
	t.Parallel()

	script, err := runtimeBootstrapScript(RuntimeClaude, "/work/project", CreateOptions{})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}

	first, _, _ := strings.Cut(script, "\n")
	if strings.Contains(first, "-e") {
		t.Errorf("bootstrap runs under errexit (%q): one unwritable onboarding file kills the session", first)
	}
	if !strings.Contains(first, "set -u") {
		t.Errorf("bootstrap does not set -u, so a typo in a variable name expands to empty: %q", first)
	}

	// The transcript is a convenience. Losing it costs the session list a
	// column and is not worth refusing to start an agent over.
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "pipe-pane") && !strings.Contains(line, "|| true") {
			t.Errorf("a failed transcript pipe is fatal: %q", line)
		}
	}
}

// A broken or missing agent binary used to produce a container that stopped
// with status 0 and no explanation: the session never existed, so the watch
// loop found nothing on its first pass and returned cleanly. That is
// indistinguishable from an agent that ran and was quit.
func TestBootstrapReportsAnAgentThatNeverStarted(t *testing.T) {
	t.Parallel()

	script, err := runtimeBootstrapScript(RuntimeClaude, "/work/project", CreateOptions{})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}

	// tmux reports success whenever it could create the session, even when the
	// command it was handed cannot be run - measured, a missing binary exits 0.
	// So the script must check that the session survived, not that tmux agreed
	// to make one.
	if !strings.Contains(script, "if ! tmux has-session -t agent") {
		t.Errorf("bootstrap trusts new-session's exit status, which is 0 for a broken agent:\n%s", script)
	}
	if !strings.Contains(script, "exit 70") {
		t.Errorf("bootstrap does not exit with a distinct status when the agent will not start:\n%s", script)
	}
	// saint cannot read the exit status - `container inspect` reports a stopped
	// container's argv and not its exit code - so the reason has to be written
	// where the host can read it.
	if !strings.Contains(script, "/saint/"+BootstrapErrorFile) {
		t.Errorf("bootstrap leaves no reason the host can read:\n%s", script)
	}
	// And a failure from a previous run must not outlive the run that fixed it.
	if !strings.Contains(script, "rm -f /saint/"+BootstrapErrorFile) {
		t.Errorf("bootstrap never clears a stale failure:\n%s", script)
	}

	// The watch loop still has to be what keeps the container alive: quitting
	// the agent must still stop the container.
	if !strings.Contains(script, "while tmux has-session -t agent") {
		t.Errorf("bootstrap no longer dies with the agent:\n%s", script)
	}
}

// --init puts a real init at PID 1 with saint's script as its child.
//
// Without it, PID 1 is the shell, and PID 1 in a namespace ignores every signal
// whose disposition is the default - so `container stop` was answered by
// nothing and the runtime SIGKILLed the container after a timeout. Measured at
// 5.3s to stop, against 0.1s with --init, and a SIGKILLed agent never flushes.
// A trap in the script is not a substitute: measured at 4.2s, because a shell
// only runs a trap between commands.
func TestCreatePutsARealInitAtPidOne(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "", CreateOptions{})

	var found bool
	for _, arg := range argv {
		if arg == "--init" {
			found = true
		}
	}
	if !found {
		t.Errorf("container run has no --init, so stopping a session SIGKILLs the agent:\n%s",
			strings.Join(argv, " "))
	}
}
