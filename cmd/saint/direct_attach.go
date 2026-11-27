package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

const directStopTimeout = 8 * time.Second

func printDirectLifetime(out io.Writer) {
	fmt.Fprintln(out, "saint: closing this pane or restarting herdr may end the agent. Guest files and history are kept; reattach continues the last conversation. If herdr restores a shell, run saint attach <session>.")
}

func attachDirect(ctx context.Context, deps sessionDeps, session store.Session, in *prompter, out io.Writer) error {
	lock, err := lockDirectSession(deps.svc.Store, session.ID)
	if err != nil {
		return err
	}
	defer lock.Close()

	// Apple's attached start cannot attach to an already running VM. Do not
	// try to stop one that was started outside this pane.
	check, cancel := context.WithTimeout(ctx, directStopTimeout)
	infos, err := deps.svc.Sandbox.List(check)
	cancel()
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.Name == deps.svc.ContainerName(session.ID) && info.State == sandbox.StateRunning {
			return fmt.Errorf("session %s is already running; stop it before reopening it", session.Name)
		}
	}

	fmt.Fprintln(out, "saint: starting the credential proxy")
	if err := deps.ensureProxy(ctx); err != nil {
		return err
	}
	reportResume(ctx, deps, session, out)
	fmt.Fprintf(out, "saint: reattach with saint attach %s\n", session.ID)
	return attachHere(deps, session.ID, func() error {
		err := runDirectAgent(ctx, deps, session.ID)
		if errors.Is(err, errProxyUnreachable) && ctx.Err() == nil && in != nil &&
			deps.firstRun.fixFirewall != nil &&
			confirm(in, out, "the macOS firewall is blocking saint's proxy. Allow it now? macOS shows a password dialog, once.") {
			if fixErr := deps.firstRun.fixFirewall(ctx, out); fixErr != nil {
				return fmt.Errorf("allow saint through the firewall: %w", fixErr)
			}
			return runDirectAgent(ctx, deps, session.ID)
		}
		return err
	})
}

func lockDirectSession(st *store.Store, id string) (*os.File, error) {
	if !store.ValidSessionID(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	// The proxy token is stored outside every guest mount, lives as long as
	// the session, and is not replaced. Lock its inode without reading or
	// modifying it so deletion can remove the lock with the rest of the row.
	f, err := os.OpenFile(st.ProxyTokenPath(id), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("session %s already has an attached pane", id)
		}
		return nil, err
	}
	return f, nil
}

func runDirectAgent(ctx context.Context, deps sessionDeps, id string) error {
	// Only intercept signals while a VM may need cleanup. Once terminal input
	// returns to saint, Ctrl-C should work normally on prompts.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer stop()
	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()
	cmd, err := deps.svc.DirectAttachCmd(cmdCtx, id)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The Apple CLI makes stdin raw. Restore it even if cancellation kills
	// the CLI before it gets a chance to run its own terminal cleanup.
	fd := os.Stdin.Fd()
	if state, err := term.GetState(int(fd)); err == nil {
		defer term.Restore(int(fd), state)
	}
	if flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0); err == nil {
		// The CLI also puts its stdin in nonblocking mode. The parent's copy
		// shares those flags and must be usable for any questions after exit.
		defer unix.FcntlInt(fd, unix.F_SETFL, flags)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	waited := false
	defer func() {
		cancelCmd()
		if !waited {
			<-exited
		}
	}()

	startup, cancelStartup := context.WithTimeout(ctx, controlTimeout+deps.reachTimeout)
	ready := make(chan error, 1)
	startupDone := make(chan struct{})
	defer func() {
		cancelStartup()
		<-startupDone
	}()
	go func() {
		defer close(startupDone)
		// attachDirect started the daemon before the attached CLI took stdin.
		// From here only the per-network listener and probe need to finish.
		deps.ensureProxy = func(context.Context) error { return nil }
		err := serveSession(startup, deps, id)
		if err == nil {
			err = markProxyReady(deps.svc.Store, id, sandbox.DirectProxyReadyFile)
		}
		ready <- err
	}()

	select {
	case err = <-exited:
		waited = true
		if ctx.Err() == nil {
			return err
		}
	case err = <-ready:
		if err == nil {
			select {
			case err = <-exited:
				waited = true
				if ctx.Err() == nil {
					return err
				}
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
	case <-ctx.Done():
		err = ctx.Err()
	}

	// An interrupted host or a network startup failure can outlive the CLI.
	// Stop the VM while we still hold the attach lock. Use a fresh bounded
	// context so cancellation of the pane does not cancel cleanup as well.
	cancelStartup()
	cancelCmd()
	shutdown, finish := context.WithTimeout(context.Background(), directStopTimeout)
	defer finish()
	if stopErr := deps.svc.Stop(shutdown, id); stopErr != nil {
		return errors.Join(err, fmt.Errorf("stop session %s: %w", id, stopErr))
	}
	return err
}
