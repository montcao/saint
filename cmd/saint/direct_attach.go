package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/montcao/saint/internal/store"
)

const directStopTimeout = 8 * time.Second

func printDirectLifetime(out io.Writer) {
	fmt.Fprintln(out, "saint: closing this pane or quitting the agent stops the microVM. Reopen the session to start the agent again.")
}

func attachDirect(ctx context.Context, deps sessionDeps, session store.Session, _ *prompter, out io.Writer) error {
	lock, err := deps.svc.LockProfile(session.ID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := deps.svc.RequireProfileQuiescent(session.ID); err != nil {
		return err
	}

	fmt.Fprintln(out, "saint: starting the credential proxy")
	if err := deps.ensureProxy(ctx); err != nil {
		return err
	}
	if err := requireProfileProxy(ctx, deps); err != nil {
		return err
	}
	return withHerdrPane(deps, session.ID, func() error {
		deps.listen = func(ctx context.Context, id string) (string, error) { return listenProfile(ctx, deps, id) }
		return runDirectAgent(ctx, deps, session.ID)
	})
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
	if err := deps.svc.MarkProfilePending(id); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errors.Join(err, deps.svc.ClearProfilePending(id))
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
			err = markDirectProxyReady(deps.svc.Store, id, "direct-proxy-ready")
		}
		ready <- err
	}()

	completedOrdinarily := false
	select {
	case err = <-exited:
		waited = true
		completedOrdinarily = err == nil && ctx.Err() == nil
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	case err = <-ready:
		if err == nil {
			select {
			case err = <-exited:
				waited = true
				completedOrdinarily = err == nil && ctx.Err() == nil
				if ctx.Err() != nil {
					err = ctx.Err()
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
	if !waited {
		<-exited
		waited = true
	}
	shutdown, finish := context.WithTimeout(context.Background(), directStopTimeout)
	defer finish()
	if stopErr := deps.svc.Stop(shutdown, id); stopErr != nil {
		return errors.Join(err, fmt.Errorf("stop session %s: %w", id, stopErr))
	}
	var exitedError *exec.ExitError
	if errors.As(err, &exitedError) {
		err = &directExitError{exitedError}
	}
	if completedOrdinarily {
		if clearErr := deps.svc.ClearProfilePending(id); clearErr != nil {
			return errors.Join(err, clearErr)
		}
	} else {
		return errors.Join(err, errors.New("experimental attachment did not complete normally; owned resources retained and reuse/deletion blocked pending qualified recovery"))
	}
	return err
}

// Preserve the attached agent/CLI exit status without changing legacy commands.
type directExitError struct{ *exec.ExitError }

func (e *directExitError) Unwrap() error { return e.ExitError }
func commandExitStatus(err error) int {
	var direct *directExitError
	if errors.As(err, &direct) && direct.ExitCode() > 0 {
		return direct.ExitCode()
	}
	return 1
}
