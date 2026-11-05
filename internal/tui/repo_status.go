package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

type RepoStatus struct {
	Path        string
	Branch      string
	DirtyCount  int
	LastTouched time.Time
	ReviewPaths []string
	Missing     bool
	ReadOnly    bool

	// NoVCS marks a directory that is not a git repository. saint runs agents
	// in any directory, so this is an ordinary state: there is no branch, no
	// diff, and no git metadata to audit.
	NoVCS bool
}

type RepoInspector struct {
	gitPath string
	logf    func(string, ...any)
}

func NewRepoInspector(gitPath string, logf func(string, ...any)) *RepoInspector {
	return &RepoInspector{gitPath: gitPath, logf: logf}
}

func (i *RepoInspector) Inspect(ctx context.Context, repos []sandbox.Repo) (map[string]RepoStatus, []error) {
	cache := make(map[string]RepoStatus, len(repos))
	var errs []error
	for _, repo := range repos {
		if _, ok := cache[repo.Path]; ok {
			continue
		}
		status, err := i.inspectOne(ctx, repo)
		cache[repo.Path] = status
		if err != nil {
			errs = append(errs, err)
		}
	}
	return cache, errs
}

func (i *RepoInspector) inspectOne(ctx context.Context, repo sandbox.Repo) (RepoStatus, error) {
	status := RepoStatus{
		Path:     repo.Path,
		ReadOnly: repo.ReadOnly,
	}
	info, err := os.Stat(repo.Path)
	if err != nil || !info.IsDir() {
		status.Missing = true
		return status, nil
	}

	loc, err := findGitDir(repo.Path)
	if err != nil {
		// Not a git repository, which is allowed and not worth a warning. The
		// directory still mounts; there is just nothing for git to report.
		status.NoVCS = true
		return status, nil
	}

	// `git status --porcelain` never reports anything under .git/, because those
	// files are outside the worktree. The metadata scan is the only thing that
	// sees a rewritten hook or a hijacked core.pager, so run it independently of
	// the porcelain call. It reads files and executes nothing.
	gitMeta := gitMetadataReviewPaths(loc.gitDir)

	// git itself never sees this repository's configuration; see
	// git_isolated.go for why, and for what it sees instead.
	shadow, cleanup, err := isolatedGitDir(loc)
	if err != nil {
		status.ReviewPaths = mergeReviewPaths(nil, gitMeta)
		return status, fmt.Errorf("%s: %w", repo.Path, err)
	}
	defer cleanup()

	branchOut, err := i.runIsolatedGit(ctx, shadow, loc.workTree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// A repository with no commits yet has a git dir but no HEAD. That is a
		// missing branch name, not a broken repo.
		status.Branch = ""
	} else {
		status.Branch = strings.TrimSpace(branchOut)
	}

	statusOut, err := i.runIsolatedGit(ctx, shadow, loc.workTree, "status", "--porcelain", "--ignore-submodules=all")
	if err != nil {
		status.ReviewPaths = mergeReviewPaths(nil, gitMeta)
		return status, fmt.Errorf("%s status: %w", repo.Path, err)
	}

	paths := parsePorcelain(statusOut)
	status.DirtyCount = len(paths)
	status.ReviewPaths = mergeReviewPaths(flaggedReviewPaths(paths), gitMeta)
	if len(paths) > 0 {
		status.LastTouched = newestMTime(repo.Path, paths)
	}
	return status, nil
}

// gitMetadataReviewPaths inspects the repo's git directory directly for the
// host-code-execution vectors a sandboxed agent can plant there. The primary
// repo is mounted read-write including .git/, so an agent can rewrite a hook or
// set a config key that runs on the host the next time the user touches the
// repo.
//
// This is stateless - no baseline snapshot - and stat/small-read only, so it is
// cheap enough for the 3s refresh.
func gitMetadataReviewPaths(gitDir string) []string {
	if gitDir == "" {
		return nil
	}

	var flagged []string
	flagged = append(flagged, scanHooksDir(filepath.Join(gitDir, "hooks"))...)
	flagged = append(flagged, scanGitConfig(readSmallFile(filepath.Join(gitDir, "config")))...)
	if excludeModified(readSmallFile(filepath.Join(gitDir, "info", "exclude"))) {
		flagged = append(flagged, ".git/info/exclude")
	}
	return flagged
}

// scanHooksDir flags every executable hook that is not one of git's shipped
// .sample files. Git only runs hooks with the executable bit set.
func scanHooksDir(hooksDir string) []string {
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return nil
	}
	var flagged []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".sample") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		flagged = append(flagged, ".git/hooks/"+name)
	}
	return flagged
}

// coreReviewKeys maps the lowercased config key to its canonical display name.
// Every one of these makes git run something the user did not type.
var coreReviewKeys = map[string]string{
	"pager":      "core.pager",
	"fsmonitor":  "core.fsmonitor",
	"sshcommand": "core.sshCommand",
	"hookspath":  "core.hooksPath",
	"editor":     "core.editor",
}

// scanGitConfig walks the repo's .git/config looking for keys that execute a
// command, plus shell aliases (an alias value starting with "!").
func scanGitConfig(data string) []string {
	var flagged []string
	section := ""
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				continue
			}
			header := strings.TrimSpace(line[1:end])
			if cut := strings.IndexAny(header, " \t\""); cut >= 0 {
				header = header[:cut]
			}
			section = strings.ToLower(header)
			// git allows "[core] pager = less" on one line.
			if rest := strings.TrimSpace(line[end+1:]); rest != "" {
				flagged = append(flagged, flagConfigEntry(section, rest)...)
			}
			continue
		}
		flagged = append(flagged, flagConfigEntry(section, line)...)
	}
	return flagged
}

func flagConfigEntry(section, line string) []string {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(line[:eq]))
	value := strings.TrimSpace(line[eq+1:])

	switch section {
	case "core":
		if display, ok := coreReviewKeys[key]; ok {
			return []string{".git/config:" + display}
		}
	case "alias":
		if strings.HasPrefix(strings.TrimSpace(strings.Trim(value, `"`)), "!") {
			return []string{".git/config:alias." + key}
		}
	}
	return nil
}

// excludeModified reports whether .git/info/exclude carries anything beyond the
// all-comment default git writes. Best effort: a rule here can hide a planted
// file from `git status` entirely.
func excludeModified(data string) bool {
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return true
	}
	return false
}

// readSmallFile reads at most maxMetadataBytes so a hostile .git/config cannot
// make the 3s refresh read an arbitrarily large file.
func readSmallFile(path string) string {
	const maxMetadataBytes = 128 << 10

	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes))
	if err != nil {
		return ""
	}
	return string(data)
}

func mergeReviewPaths(groups ...[]string) []string {
	var merged []string
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, path := range group {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			merged = append(merged, path)
		}
	}
	slices.Sort(merged)
	return merged
}

func parsePorcelain(data string) []string {
	lines := strings.Split(data, "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = parts[len(parts)-1]
		}
		if strings.HasPrefix(path, "\"") {
			if unquoted, err := strconv.Unquote(path); err == nil {
				path = unquoted
			}
		}
		paths = append(paths, path)
	}
	return paths
}

func flaggedReviewPaths(paths []string) []string {
	var flagged []string
	for _, path := range paths {
		if isReviewPath(path) {
			flagged = append(flagged, path)
		}
	}
	slices.Sort(flagged)
	return flagged
}

// reviewDirSegments match anywhere in the path, not just at the repo root, so a
// nested .github/workflows/ or a monorepo package's .vscode/ is caught too.
var reviewDirSegments = []string{
	".git/hooks/",
	".github/workflows/",
	".github/actions/",
	".husky/",
	".idea/",
	".vscode/",
	".claude/",
	".codex/",
	".devcontainer/",
}

// reviewBasenames are files that get executed, or that name something to
// execute, wherever in the tree they appear. Compared lowercased.
var reviewBasenames = map[string]struct{}{
	".envrc":                  {},
	"makefile":                {},
	"gnumakefile":             {},
	"package.json":            {},
	"justfile":                {},
	".justfile":               {},
	"taskfile.yml":            {},
	"taskfile.yaml":           {},
	".mise.toml":              {},
	"mise.toml":               {},
	".tool-versions":          {},
	"build.rs":                {},
	"build.gradle":            {},
	"build.gradle.kts":        {},
	"pyproject.toml":          {},
	"setup.py":                {},
	".pre-commit-config.yaml": {},
	".pre-commit-config.yml":  {},
	"dockerfile":              {},
	"docker-compose.yml":      {},
	"docker-compose.yaml":     {},
	"compose.yml":             {},
	"compose.yaml":            {},
}

// reviewSuffixes catch families rather than exact names.
var reviewSuffixes = []string{".code-workspace"}

// isReviewPath reports whether a changed path deserves the "!" review marker.
// Matching is suffix/segment based, not exact-match at the repo root: an agent
// that plants build.rs or .github/workflows/ci.yml three directories down still
// gets host code execution the next time the user builds or pushes.
func isReviewPath(path string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(path)), "./")
	if clean == "" {
		return false
	}
	lower := strings.ToLower(clean)

	for _, segment := range reviewDirSegments {
		if strings.HasPrefix(lower, segment) || strings.Contains(lower, "/"+segment) {
			return true
		}
	}
	for _, suffix := range reviewSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}

	base := lower
	if cut := strings.LastIndex(lower, "/"); cut >= 0 {
		base = lower[cut+1:]
	}
	_, ok := reviewBasenames[base]
	return ok
}

func newestMTime(root string, relPaths []string) time.Time {
	var newest time.Time
	for _, relPath := range relPaths {
		info, err := os.Stat(filepath.Join(root, relPath))
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

func SelectActiveRepo(statuses []RepoStatus) int {
	activeIndex := -1
	var newest time.Time
	for idx, status := range statuses {
		if status.DirtyCount == 0 || status.LastTouched.IsZero() {
			continue
		}
		if activeIndex == -1 || status.LastTouched.After(newest) {
			activeIndex = idx
			newest = status.LastTouched
		}
	}
	return activeIndex
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
