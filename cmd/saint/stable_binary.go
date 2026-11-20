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
	"strings"

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

const (
	socketFilterFW = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	osascript      = "/usr/bin/osascript"
)

// errFirewallCanceled is the password dialog being dismissed: nothing changed.
var errFirewallCanceled = errors.New("the macOS password dialog was canceled; the firewall was not changed")

// firewallScript is the AppleScript that runs tool --add and tool --unblockapp
// on the path given as its first argument. The path is never written into
// the script: it arrives through argv and reaches the shell as AppleScript's
// `quoted form of`, so no character in it can change the command. With admin
// set, macOS asks for an administrator's password in its own dialog.
func firewallScript(tool string, admin bool) []string {
	run := `do shell script ` + appleScriptString(tool+" --add ") + ` & p & ` +
		appleScriptString(" && "+tool+" --unblockapp ") + ` & p`
	if admin {
		run += " with administrator privileges"
	}
	return []string{
		"on run argv",
		"set p to quoted form of (item 1 of argv)",
		run,
		"end run",
	}
}

// appleScriptString quotes a fixed string for AppleScript source. Only the
// tool's path and fixed flags pass through it, never the binary's path.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// firewallArgs is the osascript call that lets path accept connections from
// containers: socketfilterfw --add, then --unblockapp, both as an
// administrator, behind one macOS password dialog.
func firewallArgs(path string) []string {
	args := []string{osascript}
	for _, line := range firewallScript(socketFilterFW, true) {
		args = append(args, "-e", line)
	}
	return append(args, path)
}

// runFirewall allows the proxy's fixed copy through the macOS application
// firewall. The password is asked for in macOS's own dialog rather than in
// the pane, where the pane's own reads of the terminal would compete for it.
// run is (*exec.Cmd).Run, or a fake in tests.
func runFirewall(ctx context.Context, st *store.Store, out io.Writer, run func(*exec.Cmd) error) error {
	path, err := ensureStableBinary(st)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", stableBinaryPath(st), err)
	}
	fmt.Fprintf(out, "saint: allowing %s through the macOS firewall; macOS asks for your password in a dialog\n", path)
	args := firewallArgs(path)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := run(cmd); err != nil {
		return firewallError(err, stderr.String())
	}
	fmt.Fprintf(out, "saint: the macOS firewall now lists %s as allowed\n", path)
	return nil
}

// firewallError says what went wrong from osascript's exit and stderr. A
// dismissed dialog is error -128, "User canceled".
func firewallError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if strings.Contains(stderr, "(-128)") || strings.Contains(stderr, "User canceled") {
		return errFirewallCanceled
	}
	if stderr != "" {
		return fmt.Errorf("socketfilterfw: %s", stderr)
	}
	return fmt.Errorf("socketfilterfw: %w", err)
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
