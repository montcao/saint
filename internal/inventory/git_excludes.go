package inventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The isolated git reads no global configuration (git_isolated.go), which also
// drops the user's global ignore file, so a file ignored only there counted as
// a change. That file is the host user's own, not something a guest can write,
// so it is resolved once from the user's global configuration and handed back
// to the isolated git as a single setting. Nothing else from the global
// configuration comes with it.

// hostExcludesFile returns the user's global ignore file, or "" when there is
// none. env is the environment the user's own git would see.
//
// git's rule is core.excludesFile from the global configuration when set,
// otherwise $XDG_CONFIG_HOME/git/ignore, otherwise ~/.config/git/ignore. Only a
// file that exists is returned, so a missing one changes nothing.
func hostExcludesFile(ctx context.Context, gitPath string, env []string) string {
	path := ""
	cmd := exec.CommandContext(ctx, gitPath, "config", "--global", "--path", "--get", "core.excludesFile")
	cmd.Env = env
	// --global reads only the user's file, but running outside any repository
	// keeps git from discovering one on the way.
	cmd.Dir = os.TempDir()
	if out, err := cmd.Output(); err == nil {
		path = strings.TrimSpace(string(out))
	}
	if path == "" {
		if xdg := envValue(env, "XDG_CONFIG_HOME"); xdg != "" {
			path = filepath.Join(xdg, "git", "ignore")
		} else if home := envValue(env, "HOME"); home != "" {
			path = filepath.Join(home, ".config", "git", "ignore")
		}
	}
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

func envValue(env []string, key string) string {
	value := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}
