package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/store"
)

// gitRepo makes dir look like a repo, which is the first gate a mount has to
// pass.
func gitRepo(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return resolved
}

// newMountService builds a service whose home is a temp directory, so the
// default mount root is that directory and nothing outside it.
func newMountService(t *testing.T, roots []string) (*Service, string) {
	t.Helper()

	home := t.TempDir()
	st := store.New(home)
	cfg, err := st.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.MountRoots = roots
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return &Service{Store: st}, resolved
}

// The whole point: a guest may only ever see directories the user nominated.
func TestRepoOutsideTheMountRootsIsRefused(t *testing.T) {
	t.Parallel()

	svc, _ := newMountService(t, nil)
	outside := gitRepo(t, t.TempDir())

	_, err := svc.normalizeRepoPath(outside)
	if err == nil {
		t.Fatalf("normalizeRepoPath(%q) error = nil, want a refusal", outside)
	}
	if !strings.Contains(err.Error(), "mount_roots") {
		t.Errorf("error = %v, want it to name the setting that would allow it", err)
	}
}

func TestRepoInsideTheHomeRootIsAllowed(t *testing.T) {
	t.Parallel()

	svc, home := newMountService(t, nil)
	inside := gitRepo(t, filepath.Join(home, "work", "project"))

	got, err := svc.normalizeRepoPath(inside)
	if err != nil {
		t.Fatalf("normalizeRepoPath() error = %v", err)
	}
	if got != inside {
		t.Errorf("normalizeRepoPath() = %q, want %q", got, inside)
	}
}

// An explicit mount root is the escape hatch for repos kept elsewhere, such as
// an external volume.
func TestConfiguredMountRootAllowsAnOutsidePath(t *testing.T) {
	t.Parallel()

	elsewhere := t.TempDir()
	repo := gitRepo(t, filepath.Join(elsewhere, "project"))

	svc, _ := newMountService(t, []string{elsewhere})
	if _, err := svc.normalizeRepoPath(repo); err != nil {
		t.Fatalf("normalizeRepoPath() error = %v", err)
	}
}

// Credential and tool-state directories are not work. They stay denied even
// when they sit inside an allowed root.
func TestSensitiveHomeDirectoriesAreRefused(t *testing.T) {
	t.Parallel()

	svc, home := newMountService(t, nil)

	for _, name := range []string{".ssh", ".aws", ".gnupg", ".config", ".claude", "Library"} {
		repo := gitRepo(t, filepath.Join(home, name, "looks-like-a-repo"))
		_, err := svc.normalizeRepoPath(repo)
		if err == nil {
			t.Errorf("normalizeRepoPath(%q) error = nil, want a refusal", repo)
			continue
		}
		if !strings.Contains(err.Error(), "credentials or tool state") {
			t.Errorf("error for %s = %v, want it to say why", name, err)
		}
	}
}

// The example that prompted this: scratch space is not a place a session runs.
// On macOS /tmp resolves to /private/tmp, so the check has to survive the
// symlink.
func TestScratchAndSystemDirectoriesAreRefused(t *testing.T) {
	t.Parallel()

	svc, _ := newMountService(t, nil)

	for _, path := range []string{"/tmp", "/private/tmp", "/etc", "/usr", "/var", "/Library", "/Applications", "/System"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		_, err := svc.normalizeRepoPath(path)
		if err == nil {
			t.Errorf("normalizeRepoPath(%q) error = nil, want a refusal", path)
		}
	}
}

// A root that is itself a symlink must still match, or every repo under it
// would be rejected for no reason the user could see.
func TestSymlinkedMountRootStillMatches(t *testing.T) {
	t.Parallel()

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	repo := gitRepo(t, filepath.Join(real, "project"))

	svc, _ := newMountService(t, []string{link})
	if _, err := svc.normalizeRepoPath(repo); err != nil {
		t.Fatalf("normalizeRepoPath() error = %v", err)
	}
}

func TestMountRootsOrDefault(t *testing.T) {
	t.Parallel()

	if got := (store.Config{}).MountRootsOrDefault("/home/someone"); len(got) != 1 || got[0] != "/home/someone" {
		t.Errorf("MountRootsOrDefault() = %v, want the home directory", got)
	}
	cfg := store.Config{MountRoots: []string{" /a ", "", "  ", "/b"}}
	got := cfg.MountRootsOrDefault("/home/someone")
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("MountRootsOrDefault() = %v, want [/a /b]", got)
	}
}

// An agent should be able to work in any directory. The microVM is what makes
// that safe; version control has nothing to do with it.
func TestPlainDirectoryIsAccepted(t *testing.T) {
	t.Parallel()

	svc, home := newMountService(t, nil)
	plain := filepath.Join(home, "scratch", "prototype")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(plain)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}

	got, err := svc.normalizeRepoPath(plain)
	if err != nil {
		t.Fatalf("normalizeRepoPath() error = %v, want a plain directory to be accepted", err)
	}
	if got != resolved {
		t.Errorf("normalizeRepoPath() = %q, want %q", got, resolved)
	}
}

// Dropping the version-control requirement must not widen where a session can
// reach. The location rules are the boundary and they still apply.
func TestPlainDirectoryOutsideTheMountRootsIsStillRefused(t *testing.T) {
	t.Parallel()

	svc, _ := newMountService(t, nil)
	outside := filepath.Join(t.TempDir(), "prototype")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	if _, err := svc.normalizeRepoPath(outside); err == nil {
		t.Fatal("a plain directory outside every mount root was accepted")
	}
}

func TestFilesAndMissingPathsAreStillRefused(t *testing.T) {
	t.Parallel()

	svc, home := newMountService(t, nil)

	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := svc.normalizeRepoPath(file); err == nil {
		t.Error("a file was accepted as a session directory")
	}
	if _, err := svc.normalizeRepoPath(filepath.Join(home, "nope")); err == nil {
		t.Error("a path that does not exist was accepted")
	}
}
