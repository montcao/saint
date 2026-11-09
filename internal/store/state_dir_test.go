package store

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests set SAINT_HOME with t.Setenv, so none of them can run in
// parallel.

func TestNewFromEnvDefaultsToHomeDotSaint(t *testing.T) {
	t.Setenv(StateDirEnv, "")

	home := t.TempDir()
	st, err := NewFromEnv(home)
	if err != nil {
		t.Fatalf("NewFromEnv() error = %v", err)
	}
	if got, want := st.Paths().Root, filepath.Join(home, ".saint"); got != want {
		t.Fatalf("Root = %q, want %q", got, want)
	}
}

// With SAINT_HOME set, all of saint's state goes there and nothing is written
// under ~/.saint, so a second copy cannot touch the first copy's sessions,
// tokens or config.
func TestNewFromEnvUsesSaintHome(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "second")
	t.Setenv(StateDirEnv, state+string(filepath.Separator))

	st, err := NewFromEnv(home)
	if err != nil {
		t.Fatalf("NewFromEnv() error = %v", err)
	}
	paths := st.Paths()
	if paths.Root != state {
		t.Fatalf("Root = %q, want %q", paths.Root, state)
	}
	if paths.Home != home {
		t.Fatalf("Home = %q, want %q", paths.Home, home)
	}
	for name, p := range map[string]string{
		"SessionsDir":  paths.SessionsDir,
		"SessionsPath": paths.SessionsPath,
		"ConfigPath":   paths.ConfigPath,
		"LogPath":      paths.LogPath,
		"HooksDir":     paths.HooksDir,
		"ProxyLogPath": paths.ProxyLogPath,
		"TokensDir":    paths.TokensDir,
	} {
		if filepath.Dir(p) != state {
			t.Errorf("%s = %q, want it directly under %q", name, p, state)
		}
	}

	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	if _, err := os.Stat(paths.ConfigPath); err != nil {
		t.Fatalf("config not written under SAINT_HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".saint")); !os.IsNotExist(err) {
		t.Fatalf("~/.saint was touched (stat error = %v)", err)
	}
}

// A relative SAINT_HOME would resolve against each process's own working
// directory, so the pane and the proxy it starts could use different state.
func TestNewFromEnvRejectsRelativePath(t *testing.T) {
	t.Setenv(StateDirEnv, "relative/state")

	if _, err := NewFromEnv(t.TempDir()); err == nil {
		t.Fatal("NewFromEnv() accepted a relative SAINT_HOME")
	}
}
