package tui

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

func TestSelectActiveRepoPrefersNewestDirtyRepo(t *testing.T) {
	t.Parallel()

	now := time.Now()
	statuses := []RepoStatus{
		{DirtyCount: 2, LastTouched: now.Add(-10 * time.Minute)},
		{DirtyCount: 1, LastTouched: now.Add(-2 * time.Minute)},
		{DirtyCount: 0},
	}

	if got := SelectActiveRepo(statuses); got != 1 {
		t.Fatalf("SelectActiveRepo() = %d, want 1", got)
	}
}

func TestSelectActiveRepoReturnsMinusOneWhenClean(t *testing.T) {
	t.Parallel()

	statuses := []RepoStatus{{DirtyCount: 0}, {DirtyCount: 0}}
	if got := SelectActiveRepo(statuses); got != -1 {
		t.Fatalf("SelectActiveRepo() = %d, want -1", got)
	}
}

// A directory with no git in it is an ordinary session directory, not a
// failure, and must not produce a warning.
func TestInspectNonGitDirectoryIsQuiet(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	inspector := NewRepoInspector(gitPath, nil)
	statuses, errs := inspector.Inspect(context.Background(), []sandbox.Repo{{Path: dir}})

	if len(errs) != 0 {
		t.Fatalf("Inspect() errors = %v, want none for a plain directory", errs)
	}
	status, ok := statuses[dir]
	if !ok {
		t.Fatal("Inspect() returned no status for the directory")
	}
	if !status.NoVCS {
		t.Error("NoVCS = false, want true for a directory with no git")
	}
	if status.Missing {
		t.Error("Missing = true, want false: the directory exists")
	}
	if status.Branch != "" || status.DirtyCount != 0 {
		t.Errorf("status = %+v, want no branch and no diff", status)
	}
}

// A repository with a git dir but no commits has no branch name. That is not a
// broken repo and must not be reported as one.
func TestInspectRepoWithNoCommitsIsQuiet(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	if out, err := exec.Command(gitPath, "-C", dir, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	inspector := NewRepoInspector(gitPath, nil)
	statuses, errs := inspector.Inspect(context.Background(), []sandbox.Repo{{Path: dir}})
	if len(errs) != 0 {
		t.Fatalf("Inspect() errors = %v, want none", errs)
	}
	if statuses[dir].NoVCS {
		t.Error("NoVCS = true, want false: it is a repository, just an empty one")
	}
}
