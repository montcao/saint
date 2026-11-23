package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/montcao/saint/internal/toolprofile"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Only explicitly constructed variables reach the experimental runtime client.
// In particular, CONTAINER_*, DYLD_*, provider credentials and SSH_AUTH_SOCK do not.
func profileEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME"), "TERM=" + guestTerm, "LANG=" + guestLocale}
}

func (a *AppleContainer) runProfile(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if a.containerPath == "" {
		return nil, nil, ErrNotInstalled
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.containerPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = profileEnvironment()
	out := limitedProfileOutput{limit: 8 << 20}
	cmd.Stdout = &out
	// Inventory and error output can contain environment credentials. Do not
	// echo it into diagnostics or debug logs, even on a failed command.
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("experimental runtime command failed: %w", err)
	}
	return out.Bytes(), nil, nil
}

func (a *AppleContainer) profileAttachCmd(ctx context.Context, name string) (*exec.Cmd, error) {
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.checkProfileCLI(check); err != nil {
		return nil, err
	}
	handled, err := a.profileOperation(check, name, "check")
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, errors.New("experimental ownership record missing")
	}
	if a.containerPath == "" {
		return nil, ErrNotInstalled
	}
	cmd := exec.CommandContext(ctx, a.containerPath, "start", "--attach", "--interactive", name)
	cmd.Env = profileEnvironment()
	cmd.WaitDelay = 2 * time.Second
	return cmd, nil
}

func profileBootstrapScript(rt Runtime, workdir string, opts CreateOptions) (string, error) {
	if rt != RuntimeClaude || opts.AuthMode == AuthSubscription || opts.Network == "" || opts.ProxyToken == "" {
		return "", errors.New("experimental direct pane requires Claude, an isolated network and proxy key mode")
	}
	lines := []string{"set -eu", "export LANG=" + guestLocale, "export TERM=" + guestTerm, "export ENABLE_CLAUDEAI_MCP_SERVERS=false"}
	lines = append(lines, onboardingLines(rt, workdir)...)
	// Only the per-session placeholder is approved; never import host MCP or
	// credentials. Python is part of the complete, verified tool profile.
	lines = append(lines, `python3 - <<'SAINT_PY'
import json, os
p = os.path.join(os.environ['HOME'], '.claude.json')
with open(p) as f: d = json.load(f)
d['customApiKeyResponses'] = {'approved': [os.environ['ANTHROPIC_API_KEY'][-20:]], 'rejected': []}
with open(p, 'w') as f: json.dump(d, f)
SAINT_PY`,
		`_saint_i=0`,
		`while [ ! -e /saint/direct-proxy-ready ] && [ "$_saint_i" -lt 250 ]; do sleep 0.1; _saint_i=$((_saint_i+1)); done`,
		`if [ ! -e /saint/direct-proxy-ready ]; then echo 'saint: proxy did not become ready' >&2; exit 78; fi`)
	// Disable project/user MCP configuration as well as account-imported servers.
	// Other agent settings retain their existing project semantics.
	command, err := runtimeCommand(rt, workdir, opts)
	if err != nil {
		return "", err
	}
	command += " --strict-mcp-config --mcp-config '{\"mcpServers\":{}}'"
	if !opts.Resume && strings.TrimSpace(opts.Prompt) != "" {
		lines = append(lines, `if (set -C; : > "$HOME/.saint-prompt-used") 2>/dev/null; then`, "exec "+command, "fi")
		opts.Prompt = ""
		command, err = runtimeCommand(rt, workdir, opts)
		if err != nil {
			return "", err
		}
		command += " --strict-mcp-config --mcp-config '{\"mcpServers\":{}}'"
	}
	return strings.Join(append(lines, "exec "+command), "\n"), nil
}

func ProfileGateway(ctx context.Context, containerPath, network string) (string, error) {
	a := NewAppleContainer(containerPath, nil)
	data, _, err := a.runProfile(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return "", err
	}
	return parseNetworkGateway(data, network)
}

type limitedProfileOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedProfileOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("experimental runtime response exceeds limit")
	}
	return b.Buffer.Write(p)
}
func (a *AppleContainer) checkProfileCLI(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := a.runProfile(ctx, "--version")
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.Trim(field, "(),") == toolprofile.RuntimeCommit {
			return nil
		}
	}
	return errors.New("experimental profile requires the pinned Apple container CLI; runtime and settings were not changed")
}
