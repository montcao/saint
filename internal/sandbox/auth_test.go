package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxySessionHeaderLine(t *testing.T) {
	t.Parallel()

	if got, want := proxySessionHeaderLine("tok"), "x-saint-session: tok"; got != want {
		t.Errorf("proxySessionHeaderLine() = %q, want %q", got, want)
	}
}

func TestWriteEnvFile(t *testing.T) {
	t.Parallel()

	path, cleanup, err := writeEnvFile(map[string]string{"B": "two", "A": "one"})
	if err != nil {
		t.Fatalf("writeEnvFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	// The file holds a real credential; anything readable by other users on the
	// machine defeats the point of not putting it in argv.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(data), "A=one\nB=two\n"; got != want {
		t.Errorf("contents = %q, want %q", got, want)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the env file survived cleanup: %v", err)
	}
}

// A newline in a value would define a second variable or truncate this one.
func TestWriteEnvFileRejectsNewlines(t *testing.T) {
	t.Parallel()

	if _, _, err := writeEnvFile(map[string]string{"TOKEN": "one\nTWO=two"}); err == nil {
		t.Fatal("writeEnvFile() error = nil, want an error for a value containing a newline")
	}
}

// fakeContainer stands in for the container CLI and records its argv.
func fakeContainer(t *testing.T) (binPath, argvPath string) {
	t.Helper()

	dir := t.TempDir()
	argvPath = filepath.Join(dir, "argv")
	binPath = filepath.Join(dir, "container")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvPath + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return binPath, argvPath
}

func createArgv(t *testing.T, opts CreateOptions) []string {
	t.Helper()

	binPath, argvPath := fakeContainer(t)
	sb := NewAppleContainer(binPath, nil)
	repos := []Repo{{Path: t.TempDir()}}

	if err := sb.Create(context.Background(), "saint-abc123", RuntimeClaude, repos, t.TempDir(), "http://192.0.2.1:8787", opts); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// This is the test that protects the whole feature. Setting either credential
// variable replaces the subscription as Claude Code's active credential, which
// silently moves the session onto per-token billing - the exact outcome
// subscription mode exists to prevent.
func TestSubscriptionCreateSetsNoCredentialVariable(t *testing.T) {
	t.Parallel()

	const oauth = "oauth-token-value-do-not-leak"
	argv := createArgv(t, CreateOptions{
		ProxyToken:        "session-token",
		AuthMode:          AuthSubscription,
		SubscriptionToken: oauth,
	})
	joined := strings.Join(argv, "\n")

	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("argv sets %s in subscription mode, which reverts the session to API billing", forbidden)
		}
	}
	// The real credential must never reach argv, where ps(1) exposes it.
	if strings.Contains(joined, oauth) {
		t.Error("the subscription token appears in argv; it must travel by env file")
	}
	if !strings.Contains(joined, "--env-file") {
		t.Error("argv has no --env-file; the subscription token has nowhere safe to travel")
	}
	if !strings.Contains(joined, "ANTHROPIC_CUSTOM_HEADERS=x-saint-session: session-token") {
		t.Errorf("argv does not carry the saint session header:\n%s", joined)
	}
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://192.0.2.1:8787") {
		t.Error("argv does not point the guest at the proxy")
	}
}

// The env file exists only for the duration of the run.
func TestSubscriptionEnvFileIsRemovedAfterCreate(t *testing.T) {
	t.Parallel()

	argv := createArgv(t, CreateOptions{
		ProxyToken:        "session-token",
		AuthMode:          AuthSubscription,
		SubscriptionToken: "oauth-token",
	})

	var envFile string
	for i, arg := range argv {
		if arg == "--env-file" && i+1 < len(argv) {
			envFile = argv[i+1]
		}
	}
	if envFile == "" {
		t.Fatal("no --env-file in argv")
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Errorf("the env file outlived Create(): %v", err)
	}
}

func TestKeyModeCreateIsUnchanged(t *testing.T) {
	t.Parallel()

	argv := createArgv(t, CreateOptions{ProxyToken: "session-token"})
	joined := strings.Join(argv, "\n")

	if !strings.Contains(joined, "ANTHROPIC_API_KEY=session-token") {
		t.Error("key mode no longer gives the guest its session token")
	}
	if !strings.Contains(joined, "OPENAI_API_KEY=session-token") {
		t.Error("key mode no longer gives the guest its OpenAI session token")
	}
	if strings.Contains(joined, "--env-file") {
		t.Error("key mode should not need an env file")
	}
	if strings.Contains(joined, "ANTHROPIC_CUSTOM_HEADERS") {
		t.Error("key mode should not set custom headers")
	}
}

// Subscription mode without a token must not half-apply: no custom header, and
// the guest still gets a working key-mode credential.
func TestSubscriptionWithoutATokenFallsBackToKeyMode(t *testing.T) {
	t.Parallel()

	argv := createArgv(t, CreateOptions{ProxyToken: "session-token", AuthMode: AuthSubscription})
	joined := strings.Join(argv, "\n")

	if !strings.Contains(joined, "ANTHROPIC_API_KEY=session-token") {
		t.Error("a session with no subscription token was left with no credential at all")
	}
	if strings.Contains(joined, "--env-file") {
		t.Error("an env file was written with no token to put in it")
	}
}

// Detaching a pane kills the host side of the connection but never tells tmux,
// so its client stays registered in the guest. tmux sizes a session to its
// smallest client, so one stale client from an older, smaller pane silently
// shrinks every attach after it.
func TestAttachCmdDetachesStaleClients(t *testing.T) {
	t.Parallel()

	sb := NewAppleContainer("/usr/local/bin/container", nil)
	cmd, err := sb.AttachCmd("saint-abc123", AttachOptions{})
	if err != nil {
		t.Fatalf("AttachCmd() error = %v", err)
	}

	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "tmux new-session -A -D -s agent") {
		t.Errorf("attach command = %q, want attach-or-create with other clients detached", joined)
	}
}

// The container's PID 1 is `tail -f /dev/null`, so exiting the shell inside
// tmux kills the tmux server while the container keeps running. A plain
// `tmux attach` then failed forever and the session was unreachable from saint
// with no way back.
func TestAttachCmdRebuildsADeadSession(t *testing.T) {
	t.Parallel()

	sb := NewAppleContainer("/usr/local/bin/container", nil)
	cmd, err := sb.AttachCmd("saint-abc123", AttachOptions{
		Workdir: "/work/project",
		Command: "claude --dangerously-skip-permissions",
	})
	if err != nil {
		t.Fatalf("AttachCmd() error = %v", err)
	}

	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-A") {
		t.Error("attach does not create the session when it is missing")
	}
	if !strings.Contains(joined, "-c /work/project") {
		t.Errorf("a rebuilt session does not start in the session's directory: %q", joined)
	}
	if !strings.Contains(joined, "claude --dangerously-skip-permissions") {
		t.Errorf("a rebuilt session does not relaunch the agent: %q", joined)
	}
}

// Without a session record saint still attaches; it just comes back as a plain
// shell rather than refusing.
func TestAttachCmdWithoutOptionsStillWorks(t *testing.T) {
	t.Parallel()

	sb := NewAppleContainer("/usr/local/bin/container", nil)
	cmd, err := sb.AttachCmd("saint-abc123", AttachOptions{})
	if err != nil {
		t.Fatalf("AttachCmd() error = %v", err)
	}
	if got := strings.Join(cmd.Args, " "); !strings.HasSuffix(got, "new-session -A -D -s agent") {
		t.Errorf("attach command = %q, want a bare attach-or-create", got)
	}
}

// A fresh container has an empty home, so Claude Code opens on the theme picker
// and the trust dialog instead of a prompt.
func TestBootstrapPreAnswersOnboarding(t *testing.T) {
	t.Parallel()

	script, err := runtimeBootstrapScript(RuntimeClaude, "/work/project", CreateOptions{})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}
	for _, want := range []string{"hasCompletedOnboarding", "hasTrustDialogAccepted", "/work/project", "lastOnboardingVersion"} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap does not pre-answer onboarding, missing %q:\n%s", want, script)
		}
	}
	// Never clobber a container that has accumulated real state.
	if !strings.Contains(script, `if [ ! -f "$HOME/.claude.json" ]`) {
		t.Error("bootstrap overwrites an existing Claude Code config")
	}

	// Codex is a different program and has none of these questions.
	codex, err := runtimeBootstrapScript(RuntimeCodex, "/work/project", CreateOptions{})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}
	if strings.Contains(codex, "hasCompletedOnboarding") {
		t.Error("codex bootstrap writes Claude Code onboarding state")
	}
}

// The runtime version is pinned in the image. An agent that can fetch and run
// new code inside its own sandbox is a supply-chain route into the guest, and
// it silently undoes the pin.
func TestCreateBlocksRuntimeSelfUpdate(t *testing.T) {
	t.Parallel()

	for _, opts := range []CreateOptions{
		{ProxyToken: "session-token"},
		{ProxyToken: "session-token", AuthMode: AuthSubscription, SubscriptionToken: "oauth"},
	} {
		argv := createArgv(t, opts)
		if !strings.Contains(strings.Join(argv, "\n"), "DISABLE_UPDATES=1") {
			t.Errorf("argv does not pin the runtime against self-update:\n%v", argv)
		}
	}
}

// Quitting the agent used to drop the user at a shell inside tmux, and exiting
// that shell killed the tmux server while the container kept running - saint
// reported a session that could never be attached to again.
func TestBootstrapRunsTheAgentAsTheSessionCommand(t *testing.T) {
	t.Parallel()

	script, err := runtimeBootstrapScript(RuntimeClaude, "/work/project", CreateOptions{SkipPermissions: true})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}

	if strings.Contains(script, "send-keys") {
		t.Errorf("the agent is still typed into a shell, so quitting it leaves one behind:\n%s", script)
	}
	if !strings.Contains(script, "new-session -d -s agent") || !strings.Contains(script, "claude") {
		t.Errorf("the agent is not the session's own command:\n%s", script)
	}
	if strings.Contains(script, "tail -f /dev/null") {
		t.Errorf("PID 1 still outlives the agent:\n%s", script)
	}
	// PID 1 waits on the session, so the container stops when the agent does.
	if !strings.Contains(script, "has-session -t agent") {
		t.Errorf("PID 1 does not wait on the agent session:\n%s", script)
	}
}

// The microVM is the boundary, so skipping prompts is the usual choice - but it
// has to be a choice, because the session's directory is mounted read-write.
func TestPermissionModeIsAChoice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		runtime Runtime
		skip    bool
		want    string
		absent  string
	}{
		{name: "claude skipping", runtime: RuntimeClaude, skip: true, want: "'--dangerously-skip-permissions'"},
		{name: "claude asking", runtime: RuntimeClaude, skip: false, absent: "--dangerously-skip-permissions"},
		{name: "codex skipping", runtime: RuntimeCodex, skip: true, want: "'--ask-for-approval' 'never'"},
		{name: "codex asking", runtime: RuntimeCodex, skip: false, want: "'--ask-for-approval' 'untrusted'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			command, err := runtimeCommand(tc.runtime, "/work/project", CreateOptions{SkipPermissions: tc.skip})
			if err != nil {
				t.Fatalf("runtimeCommand() error = %v", err)
			}
			if tc.want != "" && !strings.Contains(command, tc.want) {
				t.Errorf("command = %q, want it to contain %q", command, tc.want)
			}
			if tc.absent != "" && strings.Contains(command, tc.absent) {
				t.Errorf("command = %q, want it to omit %q", command, tc.absent)
			}
		})
	}
}
