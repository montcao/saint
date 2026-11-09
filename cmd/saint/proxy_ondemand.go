package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			// A listener that is running but unreachable is almost always the
			// macOS application firewall dropping connections to a binary it
			// has not been told to allow.
			return fmt.Errorf("proxy started but %s is not reachable after %s; run `saint doctor` (the macOS firewall may be blocking saint)", addr, p.timeout)
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

func newProxyStarter(st *store.Store, containerPath string) proxyStarter {
	logPath := filepath.Join(st.Paths().Root, "proxy.out")
	return proxyStarter{
		addr: func(ctx context.Context) (string, error) {
			cfg, err := st.EnsureConfig()
			if err != nil {
				return "", err
			}
			host, err := resolveProxyBindHost(ctx, cfg, containerPath)
			if err != nil {
				return "", err
			}
			return net.JoinHostPort(host, strconv.Itoa(cfg.ProxyPort)), nil
		},
		probe:   probeProxy,
		spawn:   func() (<-chan error, error) { return spawnDetachedProxy(logPath) },
		timeout: proxyStartTimeout,
		poll:    200 * time.Millisecond,
		logPath: logPath,
	}
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
