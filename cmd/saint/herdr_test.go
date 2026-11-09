package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

func TestHerdrContextPrefersTheFocusedPaneFolder(t *testing.T) {
	hc, err := parseHerdrContext(`{"workspace_id":"w1","workspace_cwd":"/work","focused_pane_id":"w1:p1","focused_pane_cwd":"/work/repo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.repo(); got != "/work/repo" {
		t.Fatalf("repo = %q, want the focused pane's folder", got)
	}
	hc.FocusedPaneCwd = ""
	if got := hc.repo(); got != "/work" {
		t.Fatalf("repo = %q, want the workspace folder as fallback", got)
	}
}

func TestHerdrContextMissingIsAnError(t *testing.T) {
	if _, err := parseHerdrContext(""); err == nil {
		t.Fatal("empty context parsed; an action run outside herdr must say so")
	}
	if _, err := parseHerdrContext("{"); err == nil {
		t.Fatal("malformed context parsed")
	}
}

func TestPaneOpenArgsSplitsTheFocusedPane(t *testing.T) {
	args, err := paneOpenArgs(herdrContext{WorkspaceID: "w1", FocusedPaneID: "w1:p2", FocusedPaneCwd: "/work/repo"}, sandbox.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--plugin saint", "--entrypoint session", "--placement split",
		"--target-pane w1:p2", "--env SAINT_REPO=/work/repo",
		"--env SAINT_RUNTIME=claude", "--env HERDR_AGENT=claude",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	// herdr rejects a split that names both a target pane and a workspace.
	if slices.Contains(args, "--workspace") {
		t.Errorf("args name both --target-pane and --workspace: %s", joined)
	}
}

func TestPaneOpenArgsNeedsAFolder(t *testing.T) {
	if _, err := paneOpenArgs(herdrContext{FocusedPaneID: "w1:p1"}, sandbox.RuntimeClaude); err == nil {
		t.Fatal("no folder accepted; the session would mount nothing")
	}
}

func TestParseRuntime(t *testing.T) {
	for in, want := range map[string]sandbox.Runtime{"": sandbox.RuntimeClaude, "claude": sandbox.RuntimeClaude, "codex": sandbox.RuntimeCodex} {
		got, err := parseRuntime(in)
		if err != nil || got != want {
			t.Errorf("parseRuntime(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseRuntime("bash"); err == nil {
		t.Error("unknown runtime accepted")
	}
}

func testStarter(t *testing.T) (*proxyStarter, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	up := &atomic.Bool{}
	spawns := &atomic.Int32{}
	p := &proxyStarter{
		addr:    func(context.Context) (string, error) { return "gateway:8787", nil },
		probe:   func(string) bool { return up.Load() },
		timeout: 200 * time.Millisecond,
		poll:    5 * time.Millisecond,
		logPath: filepath.Join(t.TempDir(), "proxy.out"),
	}
	return p, up, spawns
}

func TestProxyStarterLeavesARunningProxyAlone(t *testing.T) {
	p, up, spawns := testStarter(t)
	up.Store(true)
	p.spawn = func() (<-chan error, error) { spawns.Add(1); return nil, nil }
	if err := p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spawns.Load() != 0 {
		t.Fatal("started a second proxy beside a running one")
	}
}

func TestProxyStarterStartsOneAndWaitsForIt(t *testing.T) {
	p, up, spawns := testStarter(t)
	p.spawn = func() (<-chan error, error) {
		spawns.Add(1)
		go func() { time.Sleep(20 * time.Millisecond); up.Store(true) }()
		return make(chan error), nil
	}
	if err := p.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spawns.Load() != 1 {
		t.Fatalf("spawned %d proxies, want 1", spawns.Load())
	}
}

func TestProxyStarterReportsWhyTheProxyExited(t *testing.T) {
	p, _, _ := testStarter(t)
	if err := os.WriteFile(p.logPath, []byte("starting\nanthropic credential: no key in keychain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.spawn = func() (<-chan error, error) {
		exited := make(chan error, 1)
		exited <- errors.New("exit status 1")
		return exited, nil
	}
	err := p.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no key in keychain") {
		t.Fatalf("err = %v, want the proxy's last line of output", err)
	}
}

func TestProxyStarterAcceptsAProxyThatWonTheRace(t *testing.T) {
	p, up, _ := testStarter(t)
	p.spawn = func() (<-chan error, error) {
		// Ours lost the bind to another pane's proxy, which is now serving.
		up.Store(true)
		exited := make(chan error, 1)
		exited <- errors.New("address already in use")
		return exited, nil
	}
	// The first probe sees nothing, so ensure spawns; the race is decided
	// before the exit is read.
	calls := 0
	p.probe = func(string) bool { calls++; return calls > 1 && up.Load() }
	if err := p.ensure(context.Background()); err != nil {
		t.Fatalf("err = %v, want success through the other proxy", err)
	}
}

func TestProxyStarterTimesOutWithTheFirewallHint(t *testing.T) {
	p, _, _ := testStarter(t)
	p.spawn = func() (<-chan error, error) { return make(chan error), nil }
	err := p.ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "saint doctor") {
		t.Fatalf("err = %v, want a pointer to saint doctor", err)
	}
}
