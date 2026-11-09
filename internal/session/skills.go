package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// resolveSkills turns requested skill names into validated, hashed mounts.
// It is the skills-side counterpart to normalizeRepos: a refusal here means
// the session does not start, the same as an unmountable repo.
func (s *Service) resolveSkills(cfg store.Config, names []string) ([]sandbox.SkillMount, error) {
	if len(names) == 0 {
		return nil, nil
	}
	home := resolveOrSelf(s.Store.Paths().Home)
	root := resolveOrSelf(cfg.SkillsRootOrDefault(home))

	mounts := make([]sandbox.SkillMount, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if err := sandbox.ValidateSkillName(name); err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		configured := cfg.SkillPath(name)
		if configured == "" {
			return nil, fmt.Errorf("skill %q is not configured", name)
		}

		resolved, err := normalizeSkillPath(configured)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}
		if !underPath(resolved, root) {
			return nil, fmt.Errorf(
				"skill %q resolves to %q, outside skills_root (%s)",
				name, resolved, root)
		}

		hash, err := hashSkillTree(resolved)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		mounts = append(mounts, sandbox.SkillMount{
			Name: name,
			Path: resolved,
			Hash: hash,
		})
	}
	return mounts, nil
}

func normalizeSkillPath(raw string) (string, error) {
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
	return resolved, nil
}

// hashSkillTree returns a deterministic digest of a skill directory's
// content, independent of file order or OS-reported timestamps - only path
// and byte content affect the result, so the same skill hashes the same on
// every machine that has it.
func hashSkillTree(dir string) (string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// A symlink inside a skill directory can point anywhere on the host,
		// including outside SkillsRoot - hashing its target would hash
		// content nobody validated as a skill at all. Reject rather than
		// follow, the same posture blockedSystemRoots takes toward paths
		// that reach outside their own boundary.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("skill contains a symlink at %q, which is not allowed", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		entries = append(entries, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SkillDrift reports, for a running session, which of its mounted skills no
// longer match what was hashed at Create time. It never re-mounts anything -
// see RenewCredential's own note that a running guest cannot be handed a new
// environment - it only tells the user a renewal (new container) would pick
// up a change.
func (s *Service) SkillDrift(session store.Session) (map[string]bool, error) {
	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return nil, err
	}
	drift := make(map[string]bool, len(session.SkillHashes))
	for name, was := range session.SkillHashes {
		path := cfg.SkillPath(name)
		if path == "" {
			drift[name] = true
			continue
		}
		resolved, err := normalizeSkillPath(path)
		if err != nil {
			drift[name] = true
			continue
		}
		now, err := hashSkillTree(resolved)
		if err != nil {
			drift[name] = true
			continue
		}
		drift[name] = now != was
	}
	return drift, nil
}
