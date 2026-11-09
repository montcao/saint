package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/montcao/saint/internal/sandbox"
)

func (s *Service) normalizeRepos(repos []sandbox.Repo) ([]sandbox.Repo, error) {
	if len(repos) == 0 {
		if strings.TrimSpace(s.Workdir) == "" {
			return nil, errors.New("at least one repo is required")
		}
		repos = []sandbox.Repo{{Path: s.Workdir}}
	}

	normalized := make([]sandbox.Repo, 0, len(repos))
	seen := make(map[string]struct{}, len(repos))
	for i, repo := range repos {
		resolved, err := s.normalizeRepoPath(repo.Path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		normalized = append(normalized, sandbox.Repo{
			Path:     resolved,
			ReadOnly: i > 0 && repo.ReadOnly,
		})
	}
	return normalized, nil
}

// blockedSystemRoots are directories no session may mount, whatever the mount
// roots say. They are defence in depth: the allowlist in checkMountRoot is the
// real bound, and these only catch a mount root configured too widely.
//
// Paths are resolved before this check, so the macOS /tmp -> /private/tmp
// symlink is covered by naming both. Bare /private and /var are deliberately
// absent: on macOS they contain the per-user temporary directories, so denying
// them outright would reject legitimate paths on systems that put a home there.
// Those are excluded by the allowlist instead.
var blockedSystemRoots = []string{
	"/etc", "/usr", "/bin", "/sbin", "/proc", "/sys", "/dev",
	"/System", "/Library", "/Applications", "/opt", "/nix", "/Network", "/cores",
	"/tmp", "/private/tmp", "/private/etc", "/private/var/root", "/var/root",
}

// blockedHomeDirs are home subdirectories that hold credentials or tool state
// rather than work.
var blockedHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".config", ".claude", ".cursor", ".docker",
	".kube", ".gcloud", ".azure", ".npm", ".gem", ".cargo", "Library",
}

// resolveOrSelf resolves symlinks, falling back to the original path when it
// cannot - a path that does not exist is handled by the caller's own checks.
func resolveOrSelf(path string) string {
	if actual, err := filepath.EvalSymlinks(path); err == nil {
		return actual
	}
	return path
}

// checkMountRoot enforces that a repo lives under a configured mount root.
func (s *Service) checkMountRoot(resolved string) error {
	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return err
	}
	home := resolveOrSelf(s.Store.Paths().Home)

	roots := cfg.MountRootsOrDefault(home)
	if len(roots) == 0 {
		return errors.New("no mount roots are configured and the home directory is unknown")
	}

	for _, root := range roots {
		expanded := root
		if expanded == "~" {
			expanded = home
		} else if strings.HasPrefix(expanded, "~/") {
			expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
		}
		// Resolve the root the same way the repo was, so a symlinked root still
		// matches rather than silently rejecting everything under it.
		expanded = resolveOrSelf(expanded)
		if underPath(resolved, expanded) {
			return nil
		}
	}

	return fmt.Errorf(
		"repo path %q is outside every mount root (%s); add its parent to \"mount_roots\" in ~/.saint/config.json to allow it",
		resolved, strings.Join(roots, ", "),
	)
}

func (s *Service) normalizeRepoPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("repo path contains NUL")
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = s.Workdir
	}
	if raw == "" {
		return "", errors.New("repo path is required")
	}

	home := s.Store.Paths().Home
	if raw == "~" {
		raw = home
	} else if strings.HasPrefix(raw, "~/") {
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}

	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %q: %w", raw, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}

	// Deliberately no version-control requirement. An agent should be able to
	// work in any directory - a scratch prototype, a tarball, a checkout of
	// something that predates git - and the microVM is what makes that safe.
	// What bounds a session is where the directory is, not what is inside it.

	// Compare against the resolved home. The repo path has been through
	// EvalSymlinks, so an unresolved home would silently fail to match on any
	// system where the home directory is reached through a symlink - and every
	// check below would quietly pass.
	home = resolveOrSelf(home)

	if resolved == string(filepath.Separator) {
		return "", errors.New("repo path cannot be /")
	}
	if samePath(resolved, home) {
		return "", errors.New("repo path cannot be your home directory")
	}
	if underPath(resolved, s.Store.Paths().Root) {
		return "", errors.New("repo path cannot be inside ~/.saint")
	}

	// Credential and tool-state directories are never work, and a guest that can
	// read them can act as the user everywhere else. Checked before the system
	// roots so the message says which of the two problems it is.
	for _, name := range blockedHomeDirs {
		sensitive := filepath.Join(home, name)
		if samePath(resolved, sensitive) || underPath(resolved, sensitive) {
			return "", fmt.Errorf("repo path %q holds credentials or tool state and cannot be mounted into a session", resolved)
		}
	}

	for _, blocked := range blockedSystemRoots {
		if samePath(resolved, blocked) || underPath(resolved, blocked) {
			return "", fmt.Errorf("repo path %q is a system directory and cannot be mounted into a session", resolved)
		}
	}

	// The positive gate. Everything above is a denylist, and denylists leak;
	// this is the rule that actually bounds what a guest can ever see.
	if err := s.checkMountRoot(resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func underPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
