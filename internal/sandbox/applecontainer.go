package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type AppleContainer struct {
	containerPath string
	logf          func(string, ...any)
}

func NewAppleContainer(containerPath string, logf func(string, ...any)) *AppleContainer {
	return &AppleContainer{
		containerPath: containerPath,
		logf:          logf,
	}
}

func (a *AppleContainer) Create(ctx context.Context, name string, rt Runtime, repos []Repo, sessionDir, proxyURL string, opts CreateOptions) error {
	if err := ValidateContainerName(name); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if rt != RuntimeClaude && rt != RuntimeCodex {
		return fmt.Errorf("unsupported runtime %q", rt)
	}
	if len(repos) == 0 {
		return errors.New("at least one repo is required")
	}

	cpus := opts.CPUs
	if cpus <= 0 {
		cpus = 2
	}
	memoryGB := opts.MemoryGB
	if memoryGB <= 0 {
		memoryGB = 2
	}

	args := []string{
		"run",
		"--detach",

		// --init puts a real init process at PID 1, with saint's bootstrap
		// script as its child.
		//
		// Two things break without it, both measured (SPIKES.md). PID 1 in a
		// namespace ignores every signal whose disposition is the default, so
		// the shell saint ran as PID 1 never saw the SIGTERM from
		// `container stop` and was SIGKILLed after a timeout instead: 5.3s to
		// stop a container, and an agent killed outright rather than asked to
		// finish, so the transcript loses its tail. With --init it is 0.1s. A
		// `trap` in the script is not a substitute - measured at 4.2s, because
		// the shell only runs a trap between commands.
		//
		// The second is reaping. A shell never reaps children it did not wait
		// on, so orphans accumulate as zombies for the life of a session that
		// may run for days. init reaps them.
		"--init",

		"--name", name,
		"--label", "saint.runtime=" + string(rt),
		"--user", "agent",
		"--cpus", strconv.Itoa(cpus),
		"--memory", fmt.Sprintf("%dG", memoryGB),
		"--workdir", repos[0].Path,
	}
	// The runtime version is pinned in the image, so the agent must not replace
	// it at runtime. Set here as well as in the Dockerfile so a container built
	// from an image that predates the pin is fixed without a rebuild.
	args = append(args, "--env", "DISABLE_UPDATES=1")

	subscription := opts.AuthMode == AuthSubscription && opts.SubscriptionToken != ""
	if proxyURL != "" {
		args = append(args,
			"--env", "ANTHROPIC_BASE_URL="+proxyURL,
			"--env", "OPENAI_BASE_URL="+proxyURL,
		)
		// The guest authenticates to the local proxy with its own session token,
		// never with a real provider key. The proxy verifies the token, strips
		// it, and injects the upstream key on the way out.
		//
		// The environment is the only place the guest gets this value. The host
		// keeps the authoritative copy outside every mount, because a guest that
		// could rewrite it could claim another session's identity.
		if opts.ProxyToken != "" {
			if subscription {
				// Setting ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN here would
				// replace the subscription as the active credential and quietly
				// move this session onto per-token billing - the exact outcome
				// subscription mode exists to avoid. The session token travels
				// as a custom header instead, which Claude Code copies onto
				// every request.
				args = append(args,
					"--env", "ANTHROPIC_CUSTOM_HEADERS="+proxySessionHeaderLine(opts.ProxyToken),
				)
			} else {
				args = append(args,
					"--env", "ANTHROPIC_API_KEY="+opts.ProxyToken,
					"--env", "OPENAI_API_KEY="+opts.ProxyToken,
				)
			}
		}

		if opts.ProxyEgress {
			env, err := guestEgressEnv(proxyURL, opts.ProxyToken)
			if err != nil {
				// Loud, not silent. The user asked for this session's traffic
				// to go through saint; starting a container that quietly talks
				// to the internet directly would answer a question about
				// visibility with a container that has none.
				return err
			}
			for _, assignment := range env {
				args = append(args, "--env", assignment)
			}
		}
	}

	if subscription {
		// The subscription token is a real, year-long credential. --env would
		// put it in this process's argv, where ps(1) exposes it to every
		// same-user process for the lifetime of the run. An env file is 0600 and
		// short-lived instead.
		//
		// It is deliberately not written into sessionDir: that is mounted
		// read-write at /saint, so anything placed there is guest-rewritable.
		envFile, cleanup, err := writeEnvFile(map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": opts.SubscriptionToken,
		})
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--env-file", envFile)
	}
	for _, repo := range repos {
		mount := repo.Path + ":" + repo.Path
		if repo.ReadOnly {
			mount += ":ro"
		}
		args = append(args, "--volume", mount)
	}
	for _, skill := range opts.SkillMounts {
		if err := ValidateSkillName(skill.Name); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		args = append(args, "--volume", skill.Path+":/saint/skills/"+skill.Name+":ro")
	}
	args = append(args, "--volume", sessionDir+":/saint")

	script, err := runtimeBootstrapScript(rt, repos[0].Path, opts)
	if err != nil {
		return err
	}

	args = append(args, "saint/"+string(rt)+":latest", "sh", "-lc", script)
	_, _, err = a.run(ctx, args...)
	return err
}

func (a *AppleContainer) List(ctx context.Context) ([]Info, error) {
	stdout, _, err := a.run(ctx, "ls", "-a", "--format", "json")
	if err != nil {
		return nil, err
	}
	return ParseListJSON(stdout)
}

func (a *AppleContainer) Stop(ctx context.Context, name string) error {
	if err := ValidateContainerName(name); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	_, _, err := a.run(ctx, "stop", name)
	return err
}

func (a *AppleContainer) Start(ctx context.Context, name string) error {
	if err := ValidateContainerName(name); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	_, _, err := a.run(ctx, "start", name)
	return err
}

func (a *AppleContainer) Remove(ctx context.Context, name string) error {
	if err := ValidateContainerName(name); err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	_, _, err := a.run(ctx, "rm", "-f", name)
	return err
}

func (a *AppleContainer) AttachCmd(name string, opts AttachOptions) (*exec.Cmd, error) {
	if err := ValidateContainerName(name); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	// -d detaches any client already attached to the session.
	//
	// Detaching a pane kills the host side of the connection but never tells
	// tmux, so its client stays registered inside the guest. Those pile up, and
	// tmux sizes a session to its smallest client - so one stale client from an
	// old, smaller pane silently shrinks every later attach.
	// new-session -A attaches to the agent session if it exists and creates it
	// if it does not. That is what makes a container recoverable.
	//
	// A plain `tmux attach` stranded it permanently: the container's PID 1 is
	// `tail -f /dev/null`, so exiting the shell inside tmux kills the tmux
	// server while the container keeps running. saint went on reporting the
	// session as running, and every attach after that failed with "no sessions"
	// with no way back.
	args := []string{"exec", "-it", name, "tmux", "new-session", "-A", "-D", "-s", "agent"}
	if opts.Workdir != "" {
		args = append(args, "-c", opts.Workdir)
	}
	if opts.Command != "" {
		// Only used when the session had to be recreated; -A ignores it when
		// the session is already there.
		args = append(args, opts.Command)
	}
	if a.containerPath == "" {
		return nil, fmt.Errorf("attach: %w", ErrNotInstalled)
	}
	return exec.Command(a.containerPath, args...), nil
}

// AgentCommand is what attach runs when it has to rebuild a dead session.
func AgentCommand(rt Runtime, workdir string) (string, error) {
	return runtimeCommand(rt, workdir, CreateOptions{})
}

const (
	// bootstrapAgentFailed is the container's exit status when tmux could not
	// start the agent at all - a missing or broken binary in the image, rather
	// than a session that ran and ended.
	//
	// The status is for anyone reading `container` directly. saint cannot see
	// it: `container inspect` reports a stopped container's argv and not its
	// exit code, measured. So the script also writes the reason to a file,
	// below, which is the copy saint actually reads.
	bootstrapAgentFailed = 70

	// BootstrapErrorFile is where the bootstrap script leaves a one-line reason
	// when it gives up, relative to the session directory.
	//
	// It is inside the guest-writable mount, so its contents are untrusted text
	// that happens to usually be saint's own. It is a diagnostic and never a
	// decision: nothing branches on it, it is sanitised, and it is shown.
	BootstrapErrorFile = "bootstrap-error"
)

func runtimeBootstrapScript(rt Runtime, workdir string, opts CreateOptions) (string, error) {
	// Deliberately `set -u` and not `set -eu`.
	//
	// This script is the container's init: when it exits, the container stops.
	// Under `set -e` any failing line takes the whole session down before the
	// agent has started, and what the user sees is a container that was created
	// and is already gone - which looks exactly like saint failing to create
	// one. The lines below decide individually whether their own failure is
	// worth a session. Almost none of them are.
	lines := []string{"set -u"}
	lines = append(lines, onboardingLines(rt, workdir)...)
	command, err := runtimeCommand(rt, workdir, opts)
	if err != nil {
		return "", err
	}

	// The agent is the session's own command, not something typed into a shell.
	//
	// Sending it to a shell meant quitting the agent dropped the user at a
	// prompt inside tmux, and exiting that prompt killed the tmux server while
	// the container kept running - saint went on reporting a session that
	// could never be attached to again. As the session's command there is no
	// shell to fall into: the agent exits, the window closes, the session ends.
	//
	// The exit status of new-session is not the test, because it is not one.
	// tmux reports success as long as it could create the session: a command it
	// cannot run still produces a session, whose window then closes
	// immediately. Measured - a deliberately missing binary exits 0 here. So
	// what is checked is whether the session is still there a moment later,
	// which is also the only claim worth making. An agent that is gone within a
	// second did not start.
	//
	// Without this the watch loop below finds no session on its first pass and
	// returns 0, and a broken agent binary is a container that stopped cleanly
	// for no stated reason - indistinguishable from one the user quit.
	lines = append(lines,
		fmt.Sprintf("tmux new-session -d -s agent -x 220 -y 60 -c %s %s || true",
			shellQuote(workdir), shellQuote(command)),
		"sleep 1",
		"if ! tmux has-session -t agent 2>/dev/null; then",
		fmt.Sprintf("  printf '%%s' %s > /saint/%s 2>/dev/null || true",
			shellQuote("the "+string(rt)+" agent could not be started"), BootstrapErrorFile),
		fmt.Sprintf("  exit %d", bootstrapAgentFailed),
		"fi",
		// A previous failure must not outlive the run that fixed it.
		fmt.Sprintf("rm -f /saint/%s 2>/dev/null || true", BootstrapErrorFile),
		// The transcript is a convenience. Losing it costs the session list a
		// column, and is not worth refusing to start an agent over.
		`tmux pipe-pane -o -t agent:0.0 "cat >> /saint/transcript.log" || true`,
	)

	// PID 1 waits on the session rather than sleeping forever, so the container
	// stops when the agent does. Detaching does not end the session, so
	// ctrl+\ still leaves the agent running.
	lines = append(lines,
		"while tmux has-session -t agent 2>/dev/null; do sleep 2; done",
	)
	return strings.Join(lines, "\n"), nil
}

// onboardingLines pre-answer Claude Code's first-run questions.
//
// A fresh container has an empty home directory, so the agent opens on the
// theme picker and the trust-this-folder dialog instead of on a prompt. The
// user has already chosen a directory and a runtime in saint; being asked again
// inside the guest is saint's own setup leaking through.
//
// Written only when the file is absent, so a container that has accumulated
// real state keeps it. The version is read from the guest's own claude, because
// a newer release is entitled to ask its questions again.
func onboardingLines(rt Runtime, workdir string) []string {
	if rt != RuntimeClaude {
		return nil
	}
	return []string{
		`_saint_v=$(claude --version 2>/dev/null | awk '{print $1}')`,
		`if [ ! -f "$HOME/.claude.json" ]; then`,
		`  printf '{"hasCompletedOnboarding":true,"lastOnboardingVersion":"%s","projects":{%s:{"hasTrustDialogAccepted":true}}}' ` +
			`"$_saint_v" ` + shellQuote(jsonQuote(workdir)) + ` > "$HOME/.claude.json"`,
		`fi`,
	}
}

// jsonQuote renders a Go string as a JSON string literal.
func jsonQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func runtimeCommand(rt Runtime, workdir string, opts CreateOptions) (string, error) {
	var args []string
	switch rt {
	case RuntimeClaude:
		args = []string{"claude"}
		if opts.SkipPermissions {
			args = append(args, "--dangerously-skip-permissions")
		}
		if opts.Name != "" {
			args = append(args, "--name", opts.Name)
		}
		if opts.Resume {
			args = append(args, "--continue")
		} else if strings.TrimSpace(opts.Prompt) != "" {
			args = append(args, opts.Prompt)
		}
	case RuntimeCodex:
		approval := "untrusted"
		if opts.SkipPermissions {
			approval = "never"
		}
		if opts.Resume {
			args = []string{
				"codex", "resume", "--last",
				"--ask-for-approval", approval,
				"--sandbox", "workspace-write",
				"--cd", workdir,
			}
		} else {
			args = []string{
				"codex",
				"--ask-for-approval", approval,
				"--sandbox", "workspace-write",
				"--cd", workdir,
			}
			if strings.TrimSpace(opts.Prompt) != "" {
				args = append(args, opts.Prompt)
			}
		}
	default:
		return "", fmt.Errorf("unsupported runtime %q", rt)
	}
	return shellJoin(args), nil
}

func (a *AppleContainer) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	stdout, stderr, err := runContainer(ctx, a.containerPath, args...)
	a.debugf("exec %s exit=%d stderr=%q", redactArgv(append([]string{a.containerPath}, args...)), exitCode(err), string(stderr))
	return stdout, stderr, err
}

// runContainer execs the container CLI and returns its output. Errors carry the
// redacted argv and trimmed stderr, never a raw secret.
//
// An empty containerPath means the CLI was not found at launch. saint still
// starts so it can say so on screen, and every call reports ErrNotInstalled.
func runContainer(ctx context.Context, containerPath string, args ...string) ([]byte, []byte, error) {
	argv := strings.Join(redactArgv(args), " ")
	if containerPath == "" {
		return nil, nil, fmt.Errorf("%s: %w", argv, ErrNotInstalled)
	}
	cmd := exec.CommandContext(ctx, containerPath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if class := classifyRunError(err, stderr.Bytes()); class != nil {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w: %w: %s", argv, class, err, detail)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w: %s", argv, err, detail)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func (a *AppleContainer) debugf(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func redactArgv(args []string) []string {
	redacted := make([]string, 0, len(args))
	for _, arg := range args {
		lower := strings.ToLower(arg)
		if strings.Contains(lower, "key=") || strings.Contains(lower, "token=") || strings.Contains(lower, "secret=") || strings.Contains(lower, "password=") {
			parts := strings.SplitN(arg, "=", 2)
			redacted = append(redacted, parts[0]+"=<redacted>")
			continue
		}
		// The proxy variables carry the session token as userinfo, and none of
		// their names contain a word the check above looks for - so
		// HTTP_PROXY=http://saint:TOKEN@host:port would otherwise be written to
		// the debug log in full, where it outlives the container. The address
		// is the useful part of that line and is kept.
		redacted = append(redacted, strconv.Quote(redactURLUserinfo(arg)))
	}
	return redacted
}
