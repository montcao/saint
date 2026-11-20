package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDirectPaneUsesTheRuntimeTerminal(t *testing.T) {
	t.Parallel()
	argv := createArgv(t, CreateOptions{Direct: true, Network: "saint-abc123"})
	if argv[0] != "create" || slices.Contains(argv, "--detach") || slices.Contains(argv, "-lc") {
		t.Fatalf("direct create arguments: %v", argv)
	}
	for _, want := range []string{"--init", "--tty", "--interactive", "TERM=xterm-256color", "LANG=C.UTF-8", "HOME=/home/agent", "-c"} {
		if !slices.Contains(argv, want) {
			t.Errorf("direct create missing %s", want)
		}
	}
	script := strings.Join(argv[slices.Index(argv, "-c")+1:], "\n")
	if strings.Contains(script, "tmux") || !strings.Contains(script, "/saint/"+DirectProxyReadyFile) || !strings.Contains(script, "exec 'claude'") {
		t.Fatalf("unexpected direct bootstrap: %s", script)
	}
	bin, _ := fakeContainer(t)
	sb := NewAppleContainer(bin, nil)
	cmd, err := sb.AttachCmd("saint-abc123", AttachOptions{DirectContext: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{bin, "start", "--attach", "--interactive", "saint-abc123"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("direct attach = %v, want %v", cmd.Args, want)
	}
}

func TestClaudeBootstrapApprovesProxyKeyAndSendsPromptOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const token = "fixture-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const prompt = "first prompt with 'quotes'"
	bin := filepath.Join(home, "claude")
	const fake = "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.247 (Claude Code)'; exit; fi\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := bootstrapScript(RuntimeClaude, home, CreateOptions{Direct: true, Prompt: prompt, Name: "example", SkipPermissions: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	run := func() string {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+home+":"+os.Getenv("PATH"), "ANTHROPIC_API_KEY="+token)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bootstrap failed: %v: %s", err, out)
		}
		return string(out)
	}
	if got := run(); !strings.Contains(got, prompt+"\n") || !strings.Contains(got, "--dangerously-skip-permissions\n--name\nexample\n") {
		t.Fatalf("first output = %q", got)
	}
	path := filepath.Join(home, ".claude.json")
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Custom struct {
			Approved []string `json:"approved"`
		} `json:"customApiKeyResponses"`
	}
	if err := json.Unmarshal(config, &state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Custom.Approved, []string{token[len(token)-20:]}) || strings.Contains(string(config), token) || strings.Contains(script, token) {
		t.Fatalf("unexpected approval configuration %s", config)
	}
	userConfig := []byte(`{"userSetting":true}`)
	if err := os.WriteFile(path, userConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run(); strings.Contains(got, prompt) {
		t.Fatalf("reopening replayed the prompt: %q", got)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(userConfig) {
		t.Fatalf("reopening changed existing config: %s (%v)", after, err)
	}
}
