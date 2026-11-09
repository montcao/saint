package inventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

func testGitEnv(home, xdg string) []string {
	env := []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
	if xdg != "" {
		env = append(env, "XDG_CONFIG_HOME="+xdg)
	}
	return env
}

func TestHostExcludesFile(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()

	t.Run("core.excludesFile wins", func(t *testing.T) {
		home := t.TempDir()
		ignore := filepath.Join(home, "my-ignore")
		writeFile(t, ignore, "*.log\n")
		writeFile(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/my-ignore\n")
		if got := hostExcludesFile(ctx, gitPath, testGitEnv(home, "")); got != ignore {
			t.Fatalf("hostExcludesFile() = %q, want %q", got, ignore)
		}
	})

	t.Run("XDG default", func(t *testing.T) {
		home, xdg := t.TempDir(), t.TempDir()
		ignore := filepath.Join(xdg, "git", "ignore")
		writeFile(t, ignore, "*.log\n")
		if got := hostExcludesFile(ctx, gitPath, testGitEnv(home, xdg)); got != ignore {
			t.Fatalf("hostExcludesFile() = %q, want %q", got, ignore)
		}
	})

	t.Run("home default", func(t *testing.T) {
		home := t.TempDir()
		ignore := filepath.Join(home, ".config", "git", "ignore")
		writeFile(t, ignore, "*.log\n")
		if got := hostExcludesFile(ctx, gitPath, testGitEnv(home, "")); got != ignore {
			t.Fatalf("hostExcludesFile() = %q, want %q", got, ignore)
		}
	})

	t.Run("none", func(t *testing.T) {
		if got := hostExcludesFile(ctx, gitPath, testGitEnv(t.TempDir(), "")); got != "" {
			t.Fatalf("hostExcludesFile() = %q, want none", got)
		}
	})
}

// A file ignored only by the user's global ignore file is not a change.
func TestInspectAppliesGlobalExcludes(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	repo := t.TempDir()
	if out, err := exec.Command(gitPath, "-C", repo, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, filepath.Join(repo, "build.log"), "noise\n")
	ignore := filepath.Join(t.TempDir(), "ignore")
	writeFile(t, ignore, "*.log\n")

	inspect := func(excludes string) RepoStatus {
		inspector := NewRepoInspector(gitPath, nil)
		inspector.excludesOnce.Do(func() { inspector.excludesFile = excludes })
		statuses, errs := inspector.Inspect(context.Background(), []sandbox.Repo{{Path: repo}})
		if len(errs) != 0 {
			t.Fatalf("Inspect() errors = %v", errs)
		}
		return statuses[repo]
	}

	if got := inspect("").DirtyCount; got != 1 {
		t.Fatalf("without global excludes DirtyCount = %d, want 1", got)
	}
	if got := inspect(ignore).DirtyCount; got != 0 {
		t.Fatalf("with global excludes DirtyCount = %d, want 0", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
