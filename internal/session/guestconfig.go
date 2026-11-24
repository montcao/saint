package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The guest gets a frozen, filtered copy of the user's Claude Code
// configuration, taken when the session is created, so the agent behaves the
// way it does in the user's own terminal.
//
// Three rules decide what crosses into the guest:
//
//   - Nothing that is or holds a credential. ~/.claude.json (OAuth account,
//     MCP servers) is never read; settings.json is reduced to an allowlist
//     that excludes env, apiKeyHelper and every MCP and plugin key.
//   - Nothing that runs commands on the user's behalf: hooks, status lines
//     and plugins are dropped. Plugins can also carry MCP servers, which
//     stay off.
//   - It is a snapshot, not a live mount. Renewing a session keeps the copy it
//     was created with, for the same reason skills are hash-pinned: renewing
//     must not quietly hand the guest instructions nobody saw for this
//     session. A new session picks up host changes.
//
// ~/.claude also holds every prompt (history.jsonl), every transcript
// (projects/), file history and auth state, so it is copied entry by entry
// from an allowlist and never mounted whole.

const (
	// guestConfigMaxFile and guestConfigMaxTotal bound the copy, so a stray
	// large file in a skill cannot fill the disk once per session.
	guestConfigMaxFile  = 2 << 20
	guestConfigMaxTotal = 32 << 20

	// guestClaudeDirName is the snapshot's directory inside a session's
	// guest-config directory, mounted read-only into the guest.
	guestClaudeDirName = "claude"
)

// guestSettingsKeys are the settings.json keys that are copied. Anything not
// listed is dropped, so a key Claude Code adds later stays on the host until
// someone decides it is safe.
var guestSettingsKeys = map[string]bool{
	"alwaysThinkingEnabled": true,
	"autoMode":              true,
	"cleanupPeriodDays":     true,
	"includeCoAuthoredBy":   true,
	"language":              true,
	"model":                 true,
	"outputStyle":           true,
	"permissions":           true,
	"spinnerTipsEnabled":    true,
	"tui":                   true,
}

// guestClaudeDirs are the directories copied from ~/.claude. Skills must
// contain a skill file (skillFile) at their top level; the others are copied as they are.
var guestClaudeDirs = []string{"skills", "agents", "commands", "output-styles"}

// skillFile marks a directory as a skill, the same test Claude Code applies.
const skillFile = "SKILL" + ".md"

// guestConfigSnapshot is what was copied and what was left behind, so the
// session can say so.
type guestConfigSnapshot struct {
	Dir     string
	Skipped []string
}

// hostClaudeDir is the user's Claude Code configuration directory.
func (s *Service) hostClaudeDir() string {
	if s.HostClaudeDir != "" {
		return s.HostClaudeDir
	}
	return filepath.Join(s.Store.Paths().Home, ".claude")
}

// guestConfigDir is the session's snapshot directory on the host. It is a
// sibling of the session directory, never inside it: the session directory is
// guest-writable, and the host must not write into a tree the guest can fill
// with symlinks.
func (s *Service) guestConfigDir(id string) string {
	return filepath.Join(s.Store.Paths().GuestConfigDir, id, guestClaudeDirName)
}

// existingGuestConfig returns the snapshot a session was created with, or ""
// if it has none (sessions created before snapshots existed).
func (s *Service) existingGuestConfig(id string) string {
	dir := s.guestConfigDir(id)
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// snapshotClaudeConfig copies the allowlisted configuration into a fresh
// snapshot directory for the session. A missing host directory is not an
// error: the session simply starts with no user configuration.
func (s *Service) snapshotClaudeConfig(id string) (guestConfigSnapshot, error) {
	src := s.hostClaudeDir()
	dst := s.guestConfigDir(id)
	if err := os.RemoveAll(filepath.Dir(dst)); err != nil {
		return guestConfigSnapshot{}, err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return guestConfigSnapshot{}, err
	}
	c := &configCopier{budget: guestConfigMaxTotal}
	snap := guestConfigSnapshot{Dir: dst}

	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return snap, nil
	}

	if err := copyFilteredSettings(filepath.Join(src, "settings.json"), filepath.Join(dst, "settings.json")); err != nil {
		c.skip("settings.json", err.Error())
	}
	c.copyFile(filepath.Join(src, "CLAUDE.md"), filepath.Join(dst, "CLAUDE.md"), "CLAUDE.md")
	for _, name := range guestClaudeDirs {
		c.copyTopDir(filepath.Join(src, name), filepath.Join(dst, name), name, name == "skills")
	}
	snap.Skipped = c.skipped
	return snap, nil
}

// copyFilteredSettings writes settings.json reduced to guestSettingsKeys.
func copyFilteredSettings(src, dst string) error {
	data, err := readBoundedRegular(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	kept := make(map[string]json.RawMessage, len(all))
	for key, value := range all {
		if guestSettingsKeys[key] {
			kept[key] = value
		}
	}
	out, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, append(out, '\n'), 0o600)
}

// readBoundedRegular reads a regular file no larger than guestConfigMaxFile,
// without following a symlink at path.
func readBoundedRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > guestConfigMaxFile {
		return nil, fmt.Errorf("larger than %d bytes", guestConfigMaxFile)
	}
	return os.ReadFile(path)
}

type configCopier struct {
	budget  int64
	skipped []string
}

func (c *configCopier) skip(rel, reason string) {
	c.skipped = append(c.skipped, rel+" ("+reason+")")
}

// copyTopDir copies one allowlisted directory. The directory itself and, for
// skills, each top-level entry may be a symlink, because that is how skills
// are commonly installed; nothing below that level is followed.
func (c *configCopier) copyTopDir(src, dst, rel string, skills bool) {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return
	}
	if !skills {
		c.copyTree(src, dst, rel)
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		c.skip(rel, err.Error())
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		entryRel := rel + "/" + name
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(src, name)
		target, err := os.Stat(path)
		if err != nil || !target.IsDir() {
			continue
		}
		if skill, err := os.Lstat(filepath.Join(path, skillFile)); err != nil || !skill.Mode().IsRegular() {
			c.skip(entryRel, "no "+skillFile)
			continue
		}
		c.copyTree(path, filepath.Join(dst, name), entryRel)
	}
}

// copyTree copies regular files and directories under src. Symlinks are
// skipped rather than followed, so a link inside a skill cannot pull in a
// file from elsewhere on the host.
func (c *configCopier) copyTree(src, dst, rel string) {
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		c.skip(rel, err.Error())
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		sub, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entryRel := rel
		if sub != "." {
			entryRel = rel + "/" + filepath.ToSlash(sub)
		}
		if err != nil {
			c.skip(entryRel, err.Error())
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, sub)
		switch {
		case d.IsDir():
			if sub != "." && (d.Name() == ".git" || d.Name() == "node_modules") {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			c.skip(entryRel, "symlink")
			return nil
		case !d.Type().IsRegular():
			return nil
		case looksLikeSecret(d.Name()):
			c.skip(entryRel, "looks like a credential")
			return nil
		}
		c.copyFile(path, target, entryRel)
		return nil
	})
}

// copyFile copies one regular file within the size limits.
func (c *configCopier) copyFile(src, dst, rel string) {
	info, err := os.Lstat(src)
	if err != nil {
		return
	}
	if !info.Mode().IsRegular() {
		c.skip(rel, "not a regular file")
		return
	}
	if info.Size() > guestConfigMaxFile {
		c.skip(rel, "too large")
		return
	}
	if info.Size() > c.budget {
		c.skip(rel, "over the total size limit")
		return
	}
	in, err := os.Open(src)
	if err != nil {
		c.skip(rel, err.Error())
		return
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		c.skip(rel, err.Error())
		return
	}
	n, err := io.Copy(out, io.LimitReader(in, guestConfigMaxFile+1))
	closeErr := out.Close()
	if err != nil || closeErr != nil || n > guestConfigMaxFile {
		_ = os.Remove(dst)
		c.skip(rel, "copy failed")
		return
	}
	c.budget -= n
}

// looksLikeSecret reports file names that commonly hold credentials. It is a
// backstop: the allowlist above is the real control.
func looksLikeSecret(name string) bool {
	lower := strings.ToLower(name)
	if lower == ".env" || strings.HasPrefix(lower, ".env.") || lower == ".netrc" || lower == ".npmrc" {
		return true
	}
	if strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_ed25519") || strings.HasPrefix(lower, "id_ecdsa") {
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return strings.Contains(lower, "credential") || strings.Contains(lower, "secret")
}

// gitIdentity is the user's git name and email, for the guest's commits.
// Values that could not be passed safely as environment variables are
// dropped.
func (s *Service) gitIdentity() (name, email string) {
	if s.GitIdentity == nil {
		return "", ""
	}
	name, email = s.GitIdentity()
	return cleanIdentity(name), cleanIdentity(email)
}

func cleanIdentity(value string) string {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, "\x00\r\n") || len(value) > 256 {
		return ""
	}
	return value
}
