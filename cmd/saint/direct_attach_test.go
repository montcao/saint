package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

func setupDirectSession(t *testing.T) *store.Store {
	t.Helper()
	st := store.New(t.TempDir())
	if err := st.AddSession(store.Session{ID: "abc", Direct: true}); err != nil {
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
	first, err := lockDirectSession(st, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := lockDirectSession(st, "abc"); err == nil {
		_ = duplicate.Close()
		t.Fatal("two panes locked one session")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	last, err := lockDirectSession(st, "abc")
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
	marker := filepath.Join(st.Paths().SessionsDir, "abc", sandbox.DirectProxyReadyFile)
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	if err := markProxyReady(st, "abc", sandbox.DirectProxyReadyFile); err != nil {
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
	command string
	cmd     *exec.Cmd
	stopped bool
}

func (s *directTestSandbox) AttachCmd(_ string, opts sandbox.AttachOptions) (*exec.Cmd, error) {
	s.cmd = exec.CommandContext(opts.DirectContext, "sh", "-c", s.command)
	return s.cmd, nil
}

func (s *directTestSandbox) Stop(ctx context.Context, _ string) error {
	s.stopped = true
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
		{name: "agent exit cancels startup", cmd: "exit 0"},
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
