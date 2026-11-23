package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileDirectCLIExcludesHostAuthority(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "SSH_AUTH_SOCK", "CONTAINER_HOST"} {
		t.Setenv(key, "synthetic-private-sentinel")
	}
	a, ref, _ := profileFixture(t)
	if err := a.ReserveProfile(t.Context(), "saint-direct", ref); err != nil {
		t.Fatal(err)
	}
	cmd, err := a.AttachCmd("saint-direct", AttachOptions{DirectContext: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cmd.Args[1:], " ") != "start --attach --interactive saint-direct" {
		t.Fatal("not an attached start", cmd.Args)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "synthetic-private-sentinel") {
		t.Fatal("inherited host authority")
	}
	if _, err = a.AttachCmd("saint-direct", AttachOptions{}); err == nil {
		t.Fatal("profile fell back to tmux")
	}
	if err = a.Start(context.Background(), "saint-direct"); err == nil {
		t.Fatal("profile started detached")
	}
}

func TestProfileRejectsUnsupportedCLIWithoutCallingAdapter(t *testing.T) {
	a, ref, dir := profileFixture(t)
	if err := os.WriteFile(a.containerPath, []byte("#!/bin/sh\necho unsupported\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.PreflightProfile(t.Context(), ref); err == nil {
		t.Fatal("unsupported CLI accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "protected")); !os.IsNotExist(err) {
		t.Fatal("preflight allocated state")
	}
}

func TestProfileBootstrapHasNoDetachedAgentAndDisablesMCP(t *testing.T) {
	opts := CreateOptions{ExperimentalProfile: "synthetic", Network: "saint-test", ProxyToken: "synthetic-placeholder", Prompt: "initial 'quoted' prompt"}
	script, err := runtimeBootstrapScript(RuntimeClaude, "/work", opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exec 'claude'", "--strict-mcp-config", "direct-proxy-ready", ".saint-prompt-used", "ENABLE_CLAUDEAI_MCP_SERVERS=false"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(script, "tmux") {
		t.Fatal("detached tmux agent remains")
	}
	if strings.Count(script, "initial") != 1 {
		t.Fatal("initial prompt may be replayed")
	}
}
