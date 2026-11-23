package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

func setupDirectSession(t *testing.T) *store.Store {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(dir)
	if err := os.MkdirAll(filepath.Join(st.Paths().Root, "tool-profiles", "saint-abc"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureSessionDir("abc"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSession(store.Session{ID: "abc", ExperimentalProfile: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureProxyToken("abc"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDirectAttachLockDoesNotReplaceItsInode(t *testing.T) {
	st := setupDirectSession(t)
	after := func() []byte {
		t.Helper()
		token, err := os.ReadFile(st.ProxyTokenPath("abc"))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	token := after()
	first, err := (&session.Service{Store: st}).LockProfile("abc")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := (&session.Service{Store: st}).LockProfile("abc"); err == nil {
		_ = duplicate.Close()
		t.Fatal("two panes locked one session")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	last, err := (&session.Service{Store: st}).LockProfile("abc")
	if err != nil {
		t.Fatal(err)
	}
	defer last.Close()
	if string(after()) != string(token) {
		t.Fatal("attaching modified the proxy token")
	}
}

func TestProxyReadyReplacesGuestSymlinkWithoutWritingItsTarget(t *testing.T) {
	t.Parallel()
	st := setupDirectSession(t)
	target := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(st.Paths().SessionsDir, "abc", "direct-proxy-ready")
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	if err := markDirectProxyReady(st, "abc", "direct-proxy-ready"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("host file contents = %q (%v)", got, err)
	}
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("readiness marker = %v (%v)", info, err)
	}
}

type directTestSandbox struct {
	sandbox.Sandbox
	command        string
	cmd            *exec.Cmd
	stopped        bool
	args           []string
	env            []string
	stopBeforeReap bool
}

func (s *directTestSandbox) AttachCmd(_ string, opts sandbox.AttachOptions) (*exec.Cmd, error) {
	if s.args != nil {
		s.cmd = exec.CommandContext(opts.DirectContext, s.args[0], s.args[1:]...)
		s.cmd.Env = s.env
	} else {
		s.cmd = exec.CommandContext(opts.DirectContext, "sh", "-c", s.command)
	}
	return s.cmd, nil
}

func (s *directTestSandbox) Stop(ctx context.Context, _ string) error {
	s.stopped = true
	s.stopBeforeReap = s.cmd != nil && s.cmd.ProcessState == nil
	return ctx.Err()
}

func TestDirectAgentJoinsStartupAndReapsOnNetworkFailure(t *testing.T) {
	st := setupDirectSession(t)
	for _, tc := range []struct {
		name  string
		cmd   string
		stop  bool
		cause error
	}{
		{name: "agent exit cancels startup", cmd: "exit 0", stop: true},
		{name: "network failure stops VM", cmd: "exec sleep 60", stop: true, cause: errors.New("network unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &directTestSandbox{command: tc.cmd}
			finished := make(chan struct{})
			deps := sessionDeps{
				svc: &session.Service{Store: st, Sandbox: sb},
				listen: func(ctx context.Context, _ string) (string, error) {
					defer close(finished)
					if tc.cause == nil {
						<-ctx.Done()
						return "", ctx.Err()
					}
					return "", tc.cause
				},
				reachTimeout: time.Second,
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := runDirectAgent(ctx, deps, "abc")
			if !errors.Is(err, tc.cause) {
				t.Fatalf("runDirectAgent: %v, want %v", err, tc.cause)
			}
			if sb.stopped != tc.stop || sb.cmd.ProcessState == nil {
				t.Fatalf("stopped=%v, state=%v", sb.stopped, sb.cmd.ProcessState)
			}
			select {
			case <-finished:
			default:
				t.Fatal("returned before its listener startup ended")
			}
		})
	}
}

func (s *directTestSandbox) PreflightProfile(context.Context, string) error       { return nil }
func (s *directTestSandbox) RequireProfile(string, string) error                  { return nil }
func (s *directTestSandbox) ReserveProfile(context.Context, string, string) error { return nil }

func TestProfileChild(t *testing.T) {
	if os.Getenv("SAINT_SYNTHETIC_CHILD") != "1" {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("SAINT_TEST_MARKER")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(91)
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := http.Post(os.Getenv("SAINT_TEST_MODEL"), "application/json", strings.NewReader(`{"model":"synthetic","messages":[]}`))
	if err != nil {
		os.Exit(92)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		os.Exit(93)
	}
	os.Exit(7)
}

func TestDirectAgentUsesFakeModelAndPreservesExitStatus(t *testing.T) {
	st := setupDirectSession(t)
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Error("synthetic child sent credentials")
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"synthetic"}]}`))
	}))
	defer model.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sb := &directTestSandbox{args: []string{exe, "-test.run=^TestProfileChild$"}, env: []string{"SAINT_SYNTHETIC_CHILD=1", "SAINT_TEST_MODEL=" + model.URL, "SAINT_TEST_MARKER=" + filepath.Join(st.Paths().SessionsDir, "abc", "direct-proxy-ready")}}
	deps := sessionDeps{svc: &session.Service{Store: st, Sandbox: sb}, listen: func(context.Context, string) (string, error) { return model.URL, nil }, probe: func(string) bool { return true }, reachTimeout: time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	err = runDirectAgent(ctx, deps, "abc")
	if commandExitStatus(err) != 7 || requests.Load() != 1 || !sb.stopped || sb.stopBeforeReap {
		t.Fatalf("status=%d requests=%d stopped=%v stop-before-reap=%v", commandExitStatus(err), requests.Load(), sb.stopped, sb.stopBeforeReap)
	}
	if err := deps.svc.RequireProfileQuiescent("abc"); err == nil {
		t.Fatal("nonzero CLI completion lost uncertainty marker")
	}
}

func TestProfileDeleteCannotRaceAttachedPane(t *testing.T) {
	st := setupDirectSession(t)
	svc := &session.Service{Store: st, Sandbox: &directTestSandbox{}}
	lease, err := svc.LockProfile("abc")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = svc.Delete(t.Context(), "abc"); err == nil {
		t.Fatal("delete bypassed active pane lease")
	}
	if _, err = svc.Find("abc"); err != nil {
		t.Fatal("failed cleanup lost metadata")
	}
}

func TestProfileCanceledPaneReapsBeforeStopping(t *testing.T) {
	st := setupDirectSession(t)
	sb := &directTestSandbox{command: "exec sleep 60"}
	ctx, cancel := context.WithCancel(t.Context())
	deps := sessionDeps{svc: &session.Service{Store: st, Sandbox: sb}, listen: func(ctx context.Context, _ string) (string, error) { cancel(); return "", ctx.Err() }, reachTimeout: time.Second}
	err := runDirectAgent(ctx, deps, "abc")
	if !errors.Is(err, context.Canceled) || !sb.stopped || sb.stopBeforeReap {
		t.Fatalf("cancel err=%v stopped=%v stop-before-reap=%v", err, sb.stopped, sb.stopBeforeReap)
	}
}
