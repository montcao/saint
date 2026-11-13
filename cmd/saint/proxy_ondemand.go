package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/montcao/saint/internal/store"
)

// proxyStartTimeout bounds how long a pane waits for a proxy it started.
// Starting resolves credentials, which can mean a 1Password unlock prompt.
const proxyStartTimeout = 30 * time.Second

// proxyStarter starts the proxy on demand: the first pane that needs it
// starts it, and every later pane finds it already listening.
//
// The proxy runs detached in its own session rather than as the pane's
// child. Closing a pane ends only that pane's `container exec`, and the
// session's agent keeps running in its microVM; a proxy tied to the pane
// would cut that agent's API access the moment its pane closed.
type proxyStarter struct {
	// addr is where the proxy answers when it is running: its control
	// socket, which exists before any session network does.
	addr    func(context.Context) (string, error)
	probe   func(string) bool
	spawn   func() (exited <-chan error, err error)
	timeout time.Duration
	poll    time.Duration
	logPath string
}

func (p proxyStarter) ensure(ctx context.Context) error {
	addr, err := p.addr(ctx)
	if err != nil {
		return fmt.Errorf("proxy address: %w", err)
	}
	if p.probe(addr) {
		return nil
	}

	exited, err := p.spawn()
	if err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}

	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	tick := time.NewTicker(p.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			// Another pane may have won the race for the port; its proxy
			// serves this pane just as well.
			if p.probe(addr) {
				return nil
			}
			return fmt.Errorf("proxy exited (%v): %s", err, p.lastLog())
		case <-tick.C:
			if p.probe(addr) {
				return nil
			}
		case <-deadline.C:
			return fmt.Errorf("proxy started but did not answer on %s after %s: %s; run `saint doctor`", addr, p.timeout, p.lastLog())
		}
	}
}

// lastLog is the end of the proxy's output, so a failure says why.
func (p proxyStarter) lastLog() string {
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		return "no output"
	}
	text := strings.TrimSpace(string(data))
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		text = text[i+1:]
	}
	if text == "" {
		return "no output"
	}
	return text
}

func newProxyStarter(st *store.Store) proxyStarter {
	logPath := filepath.Join(st.Paths().Root, "proxy.out")
	return proxyStarter{
		addr: func(context.Context) (string, error) {
			// The port is chosen on first use of a state directory; doing it
			// here means the proxy and the pane cannot pick different ones.
			if _, err := st.EnsureConfig(); err != nil {
				return "", err
			}
			return st.Paths().ControlSocket, nil
		},
		probe:   pingProxy,
		spawn:   func() (<-chan error, error) { return spawnDetachedProxy(logPath) },
		timeout: proxyStartTimeout,
		poll:    200 * time.Millisecond,
		logPath: logPath,
	}
}

// proxyReachTimeout bounds how long a pane waits for a session's listener to
// accept a connection once the proxy says it is listening.
const proxyReachTimeout = 5 * time.Second

// serveSession makes sure the proxy runs and listens on a session's network,
// and that the listener can actually be reached.
//
// A successful bind proves nothing on macOS: the application firewall lets an
// unlisted binary bind the bridge address and then drops every connection to
// it (docs/research/spikes.md). So the listener is probed before the pane
// attaches, and a listener that does not answer is reported as the firewall.
func serveSession(ctx context.Context, deps sessionDeps, sessionID string) error {
	if err := deps.ensureProxy(ctx); err != nil {
		return err
	}
	addr, err := deps.listen(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	deadline := time.Now().Add(deps.reachTimeout)
	for !deps.probe(addr) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the proxy listens on %s but it is not reachable; run `saint doctor` (the macOS firewall may be blocking saint)", addr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// spawnDetachedProxy runs `saint proxy` in a new session, writing to logPath.
// The returned channel reports when it exits, for as long as this process
// lives to hear it.
func spawnDetachedProxy(logPath string) (<-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "proxy")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = errors.New("exit status 0")
		}
		exited <- err
	}()
	return exited, nil
}
