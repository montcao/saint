package tui

// Running git on the host against a repository a guest can write.
//
// Every mounted repo is read-write inside its microVM, .git included, and git
// executes commands named in a repository's own configuration: core.fsmonitor
// runs on `git status`, and so does the clean filter of any driver that
// .gitattributes assigns. saint's 3s refresh ran `git status` on the host, so an
// agent that wrote two lines into .git/config had a command running as the user,
// outside the sandbox, within seconds and with nobody touching anything. That
// was measured, not inferred; see SPIKES.md.
//
// Denylisting the dangerous keys does not work. Filter drivers are named by the
// attacker, [include] pulls configuration from any file, and the list of keys
// that execute something grows with git. So git here never reads the
// repository's configuration at all. It runs against a throwaway git directory
// that saint builds for each call:
//
//   - config is a file saint writes, carrying only the format fields git needs
//     to read the repository, each checked against the values git defines;
//   - HEAD is a copy of the real one, validated as a ref or an object name;
//   - index, objects, refs and the other data git reads are symlinks back to
//     the real repository.
//
// System and global configuration are switched off, inherited GIT_* variables
// are dropped, submodules are not entered (their configuration is just as
// writable), and optional locks are off so git does not write the real index.
// What remains is git reading data, which a guest controls anyway.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// errNoGitDir means the directory is not inside a git repository.
var errNoGitDir = errors.New("not a git repository")

// gitLocation is where a work tree's git data lives. For an ordinary clone the
// two directories are the same; a linked worktree keeps HEAD and index in its
// own directory and everything else in the common one.
type gitLocation struct {
	workTree  string
	gitDir    string
	commonDir string
}

// findGitDir walks up from dir to the nearest .git, as git's own discovery
// does, reading files rather than asking git. A .git file ("gitdir: ...") is
// followed, and so is a commondir file inside it.
func findGitDir(dir string) (gitLocation, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return gitLocation{}, err
	}
	for {
		loc, err := gitDirAt(dir)
		if err == nil {
			return loc, nil
		}
		if !errors.Is(err, errNoGitDir) {
			return gitLocation{}, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return gitLocation{}, errNoGitDir
		}
		dir = parent
	}
}

func gitDirAt(dir string) (gitLocation, error) {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return gitLocation{}, errNoGitDir
	}

	gitDir := dotGit
	if !info.IsDir() {
		line := strings.TrimSpace(readSmallFile(dotGit))
		target, ok := strings.CutPrefix(line, "gitdir:")
		if !ok {
			return gitLocation{}, errNoGitDir
		}
		gitDir = resolveFrom(dir, strings.TrimSpace(target))
	}
	if !isGitDir(gitDir) {
		return gitLocation{}, errNoGitDir
	}

	commonDir := gitDir
	if line := strings.TrimSpace(readSmallFile(filepath.Join(gitDir, "commondir"))); line != "" {
		commonDir = resolveFrom(gitDir, line)
	}
	return gitLocation{workTree: dir, gitDir: gitDir, commonDir: commonDir}, nil
}

func resolveFrom(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// isGitDir is git's own minimum: a HEAD file. Objects and refs may live in a
// common directory, so they are not required here.
func isGitDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && !info.IsDir()
}

// headPattern is what HEAD may hold: a symbolic ref under refs/, or an object
// name in either hash format.
var headPattern = regexp.MustCompile(`^(ref: refs/[^\x00-\x1f\x7f]+|[0-9a-f]{40}|[0-9a-f]{64})$`)

// isolatedGitDir builds the throwaway git directory for loc. The caller must
// call the returned cleanup.
func isolatedGitDir(loc gitLocation) (string, func(), error) {
	head := strings.TrimSpace(readSmallFile(filepath.Join(loc.gitDir, "HEAD")))
	if !headPattern.MatchString(head) {
		return "", nil, fmt.Errorf("unrecognised HEAD in %s", loc.gitDir)
	}

	shadow, err := os.MkdirTemp("", "saint-git-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(shadow) }

	if err := os.WriteFile(filepath.Join(shadow, "HEAD"), []byte(head+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	config := isolatedConfig(readSmallFile(filepath.Join(loc.commonDir, "config")))
	if err := os.WriteFile(filepath.Join(shadow, "config"), []byte(config), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}

	// Per-worktree data comes from the git dir, shared data from the common
	// dir. Only what exists is linked; git treats a missing one as empty.
	links := []struct{ name, from string }{
		{"index", loc.gitDir},
		{"objects", loc.commonDir},
		{"refs", loc.commonDir},
		{"packed-refs", loc.commonDir},
		{"reftable", loc.commonDir},
		{"shallow", loc.commonDir},
		{"info", loc.commonDir},
	}
	for _, l := range links {
		target := filepath.Join(l.from, l.name)
		if _, err := os.Lstat(target); err != nil {
			continue
		}
		if err := os.Symlink(target, filepath.Join(shadow, l.name)); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	// git refuses a git dir without refs/ and objects/. A repository with no
	// commits can lack refs content but not the directories, so this only
	// matters for a damaged repo, which then reads as "no branch" rather than
	// as an error from a path saint made up.
	for _, dir := range []string{"refs", "objects"} {
		if _, err := os.Lstat(filepath.Join(shadow, dir)); err != nil {
			if err := os.Mkdir(filepath.Join(shadow, dir), 0o700); err != nil {
				cleanup()
				return "", nil, err
			}
		}
	}
	return shadow, cleanup, nil
}

// isolatedConfig keeps only the fields git needs to read the repository's
// data, and only values git defines for them. Everything else in the real
// config - including every key that runs a command - is left behind.
func isolatedConfig(real string) string {
	version := "0"
	var extensions []string
	section := ""
	for _, raw := range strings.Split(real, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(strings.Fields(line + " ")[0], "[]\""))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), `"`))
		switch {
		case section == "core" && key == "repositoryformatversion" && (value == "0" || value == "1"):
			version = value
		case section == "extensions" && key == "objectformat" && (value == "sha1" || value == "sha256"):
			extensions = append(extensions, "\tobjectformat = "+value)
		case section == "extensions" && key == "refstorage" && (value == "files" || value == "reftable"):
			extensions = append(extensions, "\trefstorage = "+value)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[core]\n\trepositoryformatversion = %s\n\tbare = false\n", version)
	if version == "1" && len(extensions) > 0 {
		b.WriteString("[extensions]\n")
		for _, e := range extensions {
			b.WriteString(e + "\n")
		}
	}
	return b.String()
}

// isolatedGitEnv is the environment git runs in: the caller's, minus anything
// that steers git, plus the switches that keep system and global
// configuration out.
func isolatedGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// runIsolatedGit runs git for workTree against the throwaway git dir shadow.
// The -c settings repeat what the missing configuration already guarantees, so
// a git that grows a new default cannot quietly undo it.
func (i *RepoInspector) runIsolatedGit(ctx context.Context, shadow, workTree string, args ...string) (string, error) {
	full := append([]string{
		"--git-dir=" + shadow,
		"--work-tree=" + workTree,
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "status.submoduleSummary=false",
	}, args...)
	cmd := exec.CommandContext(ctx, i.gitPath, full...)
	cmd.Dir = workTree
	cmd.Env = isolatedGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if i.logf != nil {
		i.logf("git %q exit=%d", strings.Join(args, " "), exitCode(err))
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
