package inventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

// hermeticGit runs git for test setup with no system or global configuration,
// so a developer's own settings - commit signing, hooks, a default branch -
// cannot change what the test builds.
func hermeticGit(t *testing.T, gitPath, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitPath, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(isolatedGitEnv(),
		"GIT_AUTHOR_NAME=saint-test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=saint-test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// committedRepo is a repository on branch "work" with one commit and one
// uncommitted change.
func committedRepo(t *testing.T, gitPath string) string {
	t.Helper()
	repo := t.TempDir()
	hermeticGit(t, gitPath, repo, "init", "--quiet", "--initial-branch=work")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hermeticGit(t, gitPath, repo, "add", "f.txt")
	hermeticGit(t, gitPath, repo, "commit", "--quiet", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A guest can write .git/config and .gitattributes in every mounted repo. Each
// case plants something git would execute on `git status`, proves the plant is
// live by running plain git once, and then requires saint's inspection to run
// none of it while still reporting the repository correctly.
//
// Before the fix saint's 3s refresh ran plain `git -C <repo> status`, which is
// exactly the control below: a guest had a command running on the host within
// seconds, with nobody touching anything.
func TestInspectionRunsNothingAGuestPlantedInTheRepo(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	cases := []struct {
		name  string
		plant func(t *testing.T, repo, payload string)
	}{
		{
			name: "core.fsmonitor",
			plant: func(t *testing.T, repo, payload string) {
				appendFile(t, filepath.Join(repo, ".git", "config"), "[core]\n\tfsmonitor = "+payload+"\n")
			},
		},
		{
			name: "clean filter assigned by .gitattributes",
			plant: func(t *testing.T, repo, payload string) {
				appendFile(t, filepath.Join(repo, ".git", "config"), "[filter \"x\"]\n\tclean = "+payload+"\n")
				appendFile(t, filepath.Join(repo, ".gitattributes"), "* filter=x\n")
			},
		},
		{
			name: "fsmonitor pulled in through [include]",
			plant: func(t *testing.T, repo, payload string) {
				included := filepath.Join(repo, "innocent.cfg")
				appendFile(t, included, "[core]\n\tfsmonitor = "+payload+"\n")
				appendFile(t, filepath.Join(repo, ".git", "config"), "[include]\n\tpath = "+included+"\n")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := committedRepo(t, gitPath)
			marker := filepath.Join(t.TempDir(), "ran")
			payload := filepath.Join(t.TempDir(), "payload.sh")
			// A filter must pass content through; fsmonitor's output is ignored
			// on failure. One script serves both.
			if err := os.WriteFile(payload, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, repo, payload)

			// Control: plain git, as saint used to run it, executes the plant.
			// If this stops happening the test is no longer testing anything.
			plain := exec.Command(gitPath, "-C", repo, "status", "--porcelain")
			plain.Env = isolatedGitEnv() // no user config either way
			_ = plain.Run()
			if _, err := os.Stat(marker); err != nil {
				t.Skipf("this git did not run the planted %s under plain `git status`; nothing to defend against here", tc.name)
			}
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}

			inspector := NewRepoInspector(gitPath, nil)
			statuses, errs := inspector.Inspect(context.Background(), []sandbox.Repo{{Path: repo}})
			if len(errs) > 0 {
				t.Fatalf("Inspect() errors = %v", errs)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("saint's inspection ran the planted %s on the host", tc.name)
			}

			got := statuses[repo]
			if got.Branch != "work" {
				t.Errorf("Branch = %q, want %q", got.Branch, "work")
			}
			if got.DirtyCount == 0 {
				t.Errorf("DirtyCount = 0, want the modified f.txt counted")
			}
		})
	}
}

// Isolation must not cost the ordinary cases: a linked worktree keeps HEAD and
// index in its own git dir and everything else in the common one.
func TestInspectionReadsALinkedWorktree(t *testing.T) {
	t.Parallel()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	repo := committedRepo(t, gitPath)
	linked := filepath.Join(t.TempDir(), "linked")
	hermeticGit(t, gitPath, repo, "worktree", "add", "--quiet", "-b", "side", linked)
	if err := os.WriteFile(filepath.Join(linked, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statuses, errs := NewRepoInspector(gitPath, nil).Inspect(context.Background(), []sandbox.Repo{{Path: linked}})
	if len(errs) > 0 {
		t.Fatalf("Inspect() errors = %v", errs)
	}
	got := statuses[linked]
	if got.Branch != "side" {
		t.Errorf("Branch = %q, want %q", got.Branch, "side")
	}
	if got.DirtyCount != 1 {
		t.Errorf("DirtyCount = %d, want 1 (new.txt)", got.DirtyCount)
	}
}

// The config saint writes keeps only the fields git needs to read the data,
// with values git defines. Everything else is left behind.
func TestIsolatedConfigKeepsOnlyFormatFields(t *testing.T) {
	t.Parallel()

	real := `[core]
	repositoryformatversion = 1
	fsmonitor = /tmp/payload
	pager = /tmp/payload
[extensions]
	objectFormat = sha256
	refstorage = reftable
	worktreeConfig = true
[include]
	path = /tmp/evil.cfg
[filter "x"]
	clean = /tmp/payload
`
	got := isolatedConfig(real)
	for _, banned := range []string{"payload", "evil", "include", "filter", "worktreeconfig", "fsmonitor", "pager"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("isolatedConfig() kept %q:\n%s", banned, got)
		}
	}
	for _, want := range []string{"repositoryformatversion = 1", "objectformat = sha256", "refstorage = reftable"} {
		if !strings.Contains(got, want) {
			t.Errorf("isolatedConfig() lost %q:\n%s", want, got)
		}
	}

	if got := isolatedConfig("[extensions]\n\tobjectformat = $(touch /tmp/x)\n[core]\n\trepositoryformatversion = 1\n"); strings.Contains(got, "touch") {
		t.Errorf("isolatedConfig() kept a value git does not define:\n%s", got)
	}
}
