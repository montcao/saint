package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The stderr container 1.3.0 printed for `container ls` with its service
// stopped, recorded in docs/research/spikes.md.
const stoppedServiceStderr = `Error: internalError: "failed to list containers" (cause: "interrupted: "XPC connection error: Connection invalid"")
Ensure container system service has been started with ` + "`container system start`."

func TestRunContainerEmptyPathIsNotInstalled(t *testing.T) {
	t.Parallel()

	_, _, err := runContainer(context.Background(), "", "ls", "-a")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("runContainer(\"\") error = %v, want ErrNotInstalled", err)
	}
	if errors.Is(err, ErrServiceStopped) {
		t.Fatalf("runContainer(\"\") error = %v, also claims the service is stopped", err)
	}
}

func TestRunContainerMissingBinaryIsNotInstalled(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "container")
	_, _, err := runContainer(context.Background(), missing, "ls")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("runContainer(missing) error = %v, want ErrNotInstalled", err)
	}
}

// A fake CLI that prints what the real one prints with its service stopped.
// The point is that the classification survives the trip through exec, not
// only through classifyRunError.
func TestRunContainerStoppedServiceIsClassified(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stderrFile := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(stderrFile, []byte(stoppedServiceStderr), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "container")
	script := "#!/bin/sh\ncat '" + stderrFile + "' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	_, _, err := runContainer(context.Background(), fake, "ls", "-a", "--format", "json")
	if !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("runContainer() error = %v, want ErrServiceStopped", err)
	}
	if errors.Is(err, ErrNotInstalled) {
		t.Fatalf("runContainer() error = %v, a stopped service is not a missing CLI", err)
	}
}

func TestClassifyRunError(t *testing.T) {
	t.Parallel()

	exitErr := errors.New("exit status 1")
	tests := []struct {
		name   string
		err    error
		stderr string
		want   error
	}{
		{name: "stopped service", err: exitErr, stderr: stoppedServiceStderr, want: ErrServiceStopped},
		{name: "not on path", err: &exec.Error{Name: "container", Err: exec.ErrNotFound}, want: ErrNotInstalled},
		// Any other failure is not evidence about the runtime either way.
		{name: "unrelated failure", err: exitErr, stderr: `Error: notFound: "container saint-x not found"`, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyRunError(tt.err, []byte(tt.stderr))
			if got != tt.want {
				t.Fatalf("classifyRunError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAttachCmdWithoutCLI(t *testing.T) {
	t.Parallel()

	_, err := NewAppleContainer("", nil).AttachCmd("saint-demo", AttachOptions{})
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("AttachCmd() error = %v, want ErrNotInstalled", err)
	}
}
