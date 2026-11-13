package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortSocketDir returns a directory short enough for a unix socket path.
// t.TempDir() can exceed the 103 bytes macOS allows once a long test name is
// part of it, so this uses the system temp directory directly.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startControl(t *testing.T, h controlHandlers) string {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "proxy.sock")
	ln, err := listenControl(path)
	if err != nil {
		t.Fatalf("listenControl() error = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serveControl(ln, h, t.Logf)
	return path
}

// Only this user may talk to the proxy: the socket is 0600.
func TestControlSocketIsPrivate(t *testing.T) {
	t.Parallel()

	path := startControl(t, controlHandlers{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestControlPingAndListen(t *testing.T) {
	t.Parallel()

	var asked string
	path := startControl(t, controlHandlers{
		listeners: func() []string { return []string{"192.0.2.1:8787"} },
		listen: func(_ context.Context, sessionID string) (string, error) {
			asked = sessionID
			if sessionID == "missing" {
				return "", errors.New("no session \"missing\"")
			}
			return "192.0.2.1:8787", nil
		},
	})

	if !pingProxy(path) {
		t.Fatal("pingProxy() = false for a running control socket")
	}
	resp, err := controlCall(context.Background(), path, controlRequest{Op: controlOpPing})
	if err != nil || resp.PID != os.Getpid() || len(resp.Listeners) != 1 {
		t.Fatalf("ping = %+v, %v", resp, err)
	}

	resp, err = controlCall(context.Background(), path, controlRequest{Op: controlOpListen, Session: "abc123"})
	if err != nil || resp.Addr != "192.0.2.1:8787" || asked != "abc123" {
		t.Fatalf("listen = %+v, %v (asked for %q)", resp, err, asked)
	}

	if _, err := controlCall(context.Background(), path, controlRequest{Op: controlOpListen, Session: "missing"}); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("listen for a missing session: err = %v, want the proxy's reason", err)
	}
	if _, err := controlCall(context.Background(), path, controlRequest{Op: "shutdown"}); err == nil {
		t.Fatal("an unknown operation was accepted")
	}
}

// A socket left by a proxy that died is replaced; one that still answers means
// a proxy runs, and a second must not take its place.
func TestListenControlReplacesOnlyAStaleSocket(t *testing.T) {
	t.Parallel()

	path := startControl(t, controlHandlers{})
	if _, err := listenControl(path); !errors.Is(err, errProxyRunning) {
		t.Fatalf("second listenControl() error = %v, want errProxyRunning", err)
	}

	stale := filepath.Join(shortSocketDir(t), "proxy.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	// Closing a unix listener removes its file; recreate the leftover a
	// killed process leaves behind.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}
	replaced, err := listenControl(stale)
	if err != nil {
		t.Fatalf("listenControl() over a stale socket error = %v", err)
	}
	_ = replaced.Close()
}

func TestListenControlRefusesAPathTooLongForASocket(t *testing.T) {
	t.Parallel()

	long := filepath.Join(shortSocketDir(t), strings.Repeat("d", 100), "proxy.sock")
	if _, err := listenControl(long); err == nil || !strings.Contains(err.Error(), "SAINT_HOME") {
		t.Fatalf("err = %v, want a pointer to a shorter SAINT_HOME", err)
	}
}

func TestPingWithNoProxy(t *testing.T) {
	t.Parallel()

	if pingProxy(filepath.Join(shortSocketDir(t), "proxy.sock")) {
		t.Fatal("pingProxy() = true with nothing listening")
	}
}
