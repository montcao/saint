package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	"github.com/montcao/saint/internal/store"
)

// The proxy runs from a fixed copy of saint, not from wherever this binary
// happens to be.
//
// The macOS application firewall allows a binary by its path
// (docs/research/spikes.md), and `herdr plugin install` puts every install
// and update in a new directory. A proxy started from the plugin's own copy
// would lose its firewall exception on every update. The copy lives in the
// state directory, outside every mount, and is replaced only when this
// binary differs from it.

// stableBinaryPath is where the proxy's copy of saint lives.
func stableBinaryPath(st *store.Store) string {
	return filepath.Join(st.Paths().Root, "bin", "saint")
}

// ensureStableBinary makes the fixed copy match the running binary and
// returns its path.
func ensureStableBinary(st *store.Store) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dst := stableBinaryPath(st)
	if exe == dst {
		return dst, nil
	}
	return dst, copyIfDifferent(exe, dst)
}

// copyIfDifferent replaces dst with src unless they already hold the same
// bytes. The replacement is a rename, so a proxy running the old copy keeps
// running and a pane starting one never sees a half-written file.
func copyIfDifferent(src, dst string) error {
	want, err := fileDigest(src)
	if err != nil {
		return err
	}
	if have, err := fileDigest(dst); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".saint-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func fileDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

const socketFilterFW = "/usr/libexec/ApplicationFirewall/socketfilterfw"

// firewallArgs are the two socketfilterfw calls that let path accept
// connections from containers. Both need an administrator.
func firewallArgs(path string) [][]string {
	return [][]string{
		{"sudo", socketFilterFW, "--add", path},
		{"sudo", socketFilterFW, "--unblockapp", path},
	}
}

// runFirewall allows the proxy's fixed copy through the macOS application
// firewall, running sudo in the terminal so it can ask for a password.
func runFirewall(ctx context.Context, st *store.Store, in io.Reader, out io.Writer) error {
	path, err := ensureStableBinary(st)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", stableBinaryPath(st), err)
	}
	fmt.Fprintf(out, "saint: allowing %s through the macOS firewall (sudo asks for your password)\n", path)
	for _, args := range firewallArgs(path) {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = in
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", args[1], err)
		}
	}
	return nil
}

// buildRevision is the commit this binary was built from, as Go records it
// when building inside a git checkout; empty otherwise.
func buildRevision() (revision string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return revision, modified
}

// runVersion prints the commit this binary was built from. The plugin build
// script compares it with the checkout before using a prebuilt binary.
func runVersion(out io.Writer) error {
	revision, modified := buildRevision()
	if revision == "" {
		return errors.New("this binary was built outside a git checkout; its commit is unknown")
	}
	fmt.Fprintf(out, "commit: %s\n", revision)
	if modified {
		fmt.Fprintln(out, "modified: true")
	}
	return nil
}
