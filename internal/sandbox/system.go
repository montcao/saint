package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
)

// The two ways the container runtime can be absent, which ask different things
// of the person reading about them.
//
// They used to arrive as one opaque exec error, and the first-run screen
// rendered both as "Apple container runtime not found". On a machine where the
// CLI was installed and only its service was stopped, that sent the user off to
// install something they already had. Callers tell them apart with errors.Is.
var (
	// ErrNotInstalled means there is no container CLI to run.
	ErrNotInstalled = errors.New("container CLI not found on PATH")

	// ErrServiceStopped means the CLI ran and could not reach its own API
	// server: `container system start` has not been run since boot, or the
	// service was stopped.
	ErrServiceStopped = errors.New("container system service is not running")
)

// System controls the container runtime's own services.
//
// It is separate from Sandbox because its blast radius is not one session.
// The service is shared by every container on the host, saint's and anyone
// else's, so stopping it stops all of them.
type System interface {
	StartSystem(ctx context.Context) error
	StopSystem(ctx context.Context) error
}

// StartSystem starts the container services.
//
// --enable-kernel-install answers the prompt `container system start` shows
// when no default kernel is installed. saint runs the CLI with no terminal on
// stdin, so an unanswered prompt would read EOF rather than a choice; this is
// the answer `saint doctor` already recommends. It downloads a kernel only on a
// host that has none.
func (a *AppleContainer) StartSystem(ctx context.Context) error {
	_, _, err := a.run(ctx, "system", "start", "--enable-kernel-install")
	return err
}

// StopSystem stops the container services, and with them every container on
// the host.
func (a *AppleContainer) StopSystem(ctx context.Context) error {
	_, _, err := a.run(ctx, "system", "stop")
	return err
}

// classifyRunError names which kind of absence a failed CLI call was, or
// returns nil when it was neither.
//
// The service check reads stderr because the CLI exits 1 for everything. Both
// phrases are what container 1.3.0 printed with its service stopped (see
// SPIKES.md); a later CLI that rewords them falls through to an unclassified
// error, which the UI reports as unknown rather than as a wrong answer.
func classifyRunError(err error, stderr []byte) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return ErrNotInstalled
	}
	text := string(stderr)
	if strings.Contains(text, "container system start") || strings.Contains(text, "XPC connection error") {
		return ErrServiceStopped
	}
	return nil
}
