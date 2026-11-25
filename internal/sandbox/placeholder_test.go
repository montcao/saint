package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPlaceholderApprovalFreshStateOnly(t *testing.T) {
	t.Parallel()
	for _, approve := range []bool{false, true} {
		home := t.TempDir()
		token := "fixture-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		if err := os.WriteFile(filepath.Join(home, "claude"), []byte("#!/bin/sh\necho '2.1.247 (Claude Code)'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		script := strings.Join(onboardingLines(RuntimeClaude, home, approve), "\n")
		run := func() {
			cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+home+":"+os.Getenv("PATH"), "ANTHROPIC_API_KEY="+token)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		}
		run()
		path := filepath.Join(home, ".claude.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Custom struct {
				Approved []string `json:"approved"`
			} `json:"customApiKeyResponses"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		var want []string
		if approve {
			want = []string{token[len(token)-20:]}
		}
		if !reflect.DeepEqual(state.Custom.Approved, want) || strings.Contains(string(data), token) {
			t.Fatalf("unexpected approval: %s", data)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("state permissions: %v %v", info, err)
		}
		existing := []byte(`{"userSetting":true}`)
		if err := os.WriteFile(path, existing, 0600); err != nil {
			t.Fatal(err)
		}
		run()
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(existing) {
			t.Fatalf("existing state overwritten: %s %v", after, err)
		}
	}
}

func TestPlaceholderApprovalRequiresProxyKeyMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		opts  CreateOptions
		proxy string
		want  bool
	}{
		{CreateOptions{ProxyToken: "placeholder"}, "http://localhost:8080", true},
		{CreateOptions{ProxyToken: "placeholder", AuthMode: AuthSubscription, SubscriptionToken: "fixture-oauth"}, "http://localhost:8080", false},
		{CreateOptions{}, "http://localhost:8080", false},
		{CreateOptions{ProxyToken: "placeholder"}, "", false},
	} {
		script := strings.Join(createArgvWithProxy(t, tc.proxy, tc.opts), "\n")
		if got := strings.Contains(script, "customApiKeyResponses"); got != tc.want {
			t.Fatalf("approval=%t want %t", got, tc.want)
		}
	}
}

// Execute the initialization emitted by Create with both features enabled:
// approving a placeholder must not drop the host configuration snapshot.
func TestPlaceholderApprovalWithConfigSnapshot(t *testing.T) {
	t.Parallel()
	home, snapshot := t.TempDir(), t.TempDir()
	settings := []byte(`{"outputStyle":"concise"}`)
	if err := os.WriteFile(filepath.Join(snapshot, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	instructions := []byte("synthetic instructions\n")
	if err := os.WriteFile(filepath.Join(snapshot, "CLAUDE.md"), instructions, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "claude"), []byte("#!/bin/sh\necho '2.1.247 (Claude Code)'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	token := "fixture-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	argv := createArgvWithProxy(t, "http://localhost:8080", CreateOptions{ProxyToken: token, ClaudeConfigDir: snapshot})
	script, _, found := strings.Cut(bootstrapOf(argv), "\ntmux new-session")
	if !found {
		t.Fatal("cannot isolate initialization before agent launch")
	}
	// Substitute the guest mount with its local fixture; execute the generated
	// onboarding/config commands rather than a separately assembled script.
	script = strings.ReplaceAll(script, guestClaudeConfigMount, snapshot)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+home+":"+os.Getenv("PATH"), "ANTHROPIC_API_KEY="+token)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var state struct {
		Custom struct {
			Approved []string `json:"approved"`
		} `json:"customApiKeyResponses"`
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Custom.Approved, []string{token[len(token)-20:]}) {
		t.Fatalf("approval missing: %+v", state)
	}
	for name, want := range map[string][]byte{"settings.json": settings, "CLAUDE.md": instructions} {
		got, err := os.ReadFile(filepath.Join(home, ".claude", name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("snapshot %s not installed: %q %v", name, got, err)
		}
	}
}
