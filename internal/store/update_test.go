package store

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestNormalizeHostRule(t *testing.T) {
	t.Parallel()

	good := map[string]string{
		"Example.COM":    "example.com",
		" example.com. ": "example.com",
		"*.example.com":  "*.example.com",
		".example.com":   "*.example.com",
		"api-2.example":  "api-2.example",
		"localhost":      "localhost",
	}
	for in, want := range good {
		got, err := NormalizeHostRule(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHostRule(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "*", "https://example.com", "example.com:443", "example.com/path",
		"user@example.com", "*.com", ".com", "exa mple.com", "a..b", "*.*.example.com",
	} {
		if got, err := NormalizeHostRule(bad); err == nil {
			t.Errorf("NormalizeHostRule(%q) = %q, want an error", bad, got)
		}
	}
}

// The proxy saving an answer and `saint allow` run in different processes and
// can write at the same moment. The lock makes every addition survive.
func TestConcurrentAllowsAllSurvive(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.AllowHost(fmt.Sprintf("host%d.example", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	cfg, err := st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProxyAllowHosts) != n {
		t.Fatalf("saved %d hosts, want %d: %v", len(cfg.ProxyAllowHosts), n, cfg.ProxyAllowHosts)
	}
	if !slices.IsSorted(cfg.ProxyAllowHosts) {
		t.Errorf("saved hosts are not sorted: %v", cfg.ProxyAllowHosts)
	}
}

func TestAllowAndDisallowReportWhatChanged(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if added, err := st.AllowHost("Example.com"); err != nil || !added {
		t.Fatalf("first AllowHost = %v, %v; want added", added, err)
	}
	if added, err := st.AllowHost("example.com"); err != nil || added {
		t.Fatalf("second AllowHost = %v, %v; want already there", added, err)
	}
	if removed, err := st.DisallowHost("EXAMPLE.com"); err != nil || !removed {
		t.Fatalf("DisallowHost = %v, %v; want removed", removed, err)
	}
	if removed, err := st.DisallowHost("example.com"); err != nil || removed {
		t.Fatalf("second DisallowHost = %v, %v; want nothing to remove", removed, err)
	}
	if _, err := st.AllowHost("https://example.com"); err == nil {
		t.Fatal("a URL was saved as a host")
	}
}

// Saving the allowlist must not disturb the rest of the config, which other
// commands own.
func TestAllowHostKeepsTheRestOfTheConfig(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	cfg, err := st.EnsureConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AllowHost("example.com"); err != nil {
		t.Fatal(err)
	}
	after, err := st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxyPort != cfg.ProxyPort || after.ProxyAuth != cfg.ProxyAuth {
		t.Errorf("config changed beyond the allowlist: before %+v, after %+v", cfg, after)
	}
}

func TestUpdateSession(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if err := st.AddSession(Session{ID: "abc123", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	pane := &HerdrPane{PaneID: "w1:p2", SocketPath: "/run/herdr.sock"}
	if err := st.UpdateSession("abc123", func(s *Session) { s.Herdr = pane }); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].Herdr == nil || *sessions[0].Herdr != *pane {
		t.Errorf("herdr pane = %+v, want %+v", sessions[0].Herdr, pane)
	}
	if err := st.UpdateSession("nope", func(*Session) {}); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("UpdateSession(unknown) error = %v, want ErrSessionNotFound", err)
	}

	removed, err := st.RemoveSession("abc123")
	if err != nil || removed.Name != "one" {
		t.Fatalf("RemoveSession = %+v, %v", removed, err)
	}
	if again, err := st.RemoveSession("abc123"); err != nil || again.ID != "" {
		t.Fatalf("second RemoveSession = %+v, %v; want nothing and no error", again, err)
	}
}

// The control socket and the lock live in the state directory, so a second
// copy of saint with its own SAINT_HOME has its own proxy and its own lock.
func TestControlSocketFollowsTheStateDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StateDirEnv, root)
	st, err := NewFromEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Paths().ControlSocket; got != root+"/proxy.sock" {
		t.Errorf("control socket = %q, want it under %s", got, root)
	}
	if got := st.Paths().LockPath; got != root+"/state.lock" {
		t.Errorf("lock = %q, want it under %s", got, root)
	}
}
