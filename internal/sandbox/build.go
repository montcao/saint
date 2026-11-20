package sandbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

func BuildRuntimeImage(ctx context.Context, containerPath, repoRoot, runtime string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	buildDir := filepath.Join(repoRoot, "images", runtime)
	cmd := exec.CommandContext(ctx, containerPath, "build", "-t", "saint/"+runtime+":latest", buildDir)
	cmd.Stdout, cmd.Stderr = out, out
	// Apple container uses one global builder that external builds can begin
	// using at any moment, even if it was idle before our build started.
	defer fmt.Fprintln(out, "saint: Apple container uses a shared build VM. Once every build is finished, run `container builder stop` to stop it.")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s: %w", buildDir, err)
	}
	return nil
}
