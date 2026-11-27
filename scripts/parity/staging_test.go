package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testStages(t *testing.T) *stagedScripts {
	t.Helper()
	h, err := newScriptDirectory("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	g, err := newScriptDirectory("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.root.Close(); g.root.Close(); os.RemoveAll(h.path); os.RemoveAll(g.path) })
	return &stagedScripts{hostPane: "host", guestPane: "guest", host: h, guest: g}
}
func TestStagedLongMultilineCommandUsesSeparateCopies(t *testing.T) {
	s := testStages(t)
	script := "# " + strings.Repeat("long diagnostic payload ", 100) + "\nprintf 'alpha\\nbeta\\n'"
	host, check, err := s.prepare("host", script, "synthetic")
	if err != nil || check() != nil {
		t.Fatal(err)
	}
	guest, _, err := s.prepare("guest", script, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if host == guest || strings.Contains(host, s.guest.path) || len(host) > stagedCommandLimit || len(guest) > stagedCommandLimit {
		t.Fatal("unsafe invocation")
	}
	if string(s.host.files[0].data) != string(s.guest.files[0].data) {
		t.Fatal("copies differ")
	}
	// A guest can change its copy, but cannot thereby change host execution.
	if err := os.WriteFile(filepath.Join(s.guest.path, "p0"), []byte("exit 42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := exec.CommandContext(t.Context(), "sh", "-c", host).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	got, err := extract(string(data), "synthetic")
	if err != nil || got != "alpha\nbeta" {
		t.Fatal(got, err)
	}
	if err := s.guest.verify(s.guest.files[0]); err == nil {
		t.Fatal("guest mutation accepted")
	}
}
func TestStagedIdentityAndCleanup(t *testing.T) {
	for _, mutation := range []string{"content", "symlink", "replacement", "mode", "unexpected", "fifo"} {
		t.Run(mutation, func(t *testing.T) {
			s := testStages(t)
			_, check, err := s.prepare("host", "true", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.host.path, "p0")
			switch mutation {
			case "fifo":
				os.Remove(path)
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "content":
				os.WriteFile(path, []byte("false"), 0600)
			case "symlink":
				os.Remove(path)
				os.Symlink("missing", path)
			case "replacement":
				os.Rename(path, path+".old")
				os.WriteFile(path, s.host.files[0].data, 0600)
			case "mode":
				os.Chmod(path, 0644)
			case "unexpected":
				os.WriteFile(filepath.Join(s.host.path, "unowned"), []byte("retain"), 0600)
			}
			if mutation != "unexpected" && check() == nil {
				t.Fatal("mutation accepted")
			}
			if s.cleanup() == nil {
				t.Fatal("uncertain cleanup reported success")
			}
			if _, err := os.Lstat(s.host.path); err != nil {
				t.Fatal("changed directory removed", err)
			}
		})
	}
}
func TestStagedRefusalBeforeInputAndCollision(t *testing.T) {
	s := testStages(t)
	original := s.host.path
	for _, bad := range []string{strings.Repeat("x", 121), "/tmp/unsafe\npath"} {
		s.host.path = bad
		d := driver{stages: s, timeout: time.Second, run: func(context.Context, string, ...string) (string, error) {
			t.Fatal("input sent for unsafe path")
			return "", nil
		}}
		if _, err := d.probe(t.Context(), "host", "true", "fixture"); err == nil {
			t.Fatal("unsafe path accepted")
		}
	}
	s.host.path = original
	os.WriteFile(filepath.Join(original, "p0"), []byte("existing"), 0600)
	if _, _, err := s.prepare("host", "true", "fixture"); err == nil {
		t.Fatal("collision overwritten")
	}
	data, _ := os.ReadFile(filepath.Join(original, "p0"))
	if string(data) != "existing" {
		t.Fatal("collision changed")
	}
}
func TestStagedChangedPendingStopsBeforeEnter(t *testing.T) {
	s := testStages(t)
	pending := ""
	d := driver{stages: s, prefix: "!", timeout: 10 * time.Millisecond, run: func(_ context.Context, _ string, a ...string) (string, error) {
		if a[1] == "send-keys" && a[3] == "Enter" {
			t.Fatal("submitted changed script")
		}
		if a[1] == "send-text" {
			pending = a[3]
			os.WriteFile(filepath.Join(s.host.path, "p0"), []byte("false"), 0600)
		}
		if a[1] == "read" {
			if pending != "" {
				return inputBox("!", pending), nil
			}
			return inputBox("❯", "") + inputBox("!", ""), nil
		}
		return "{}", nil
	}}
	// Supply expected UI transitions while changing only the staged bytes.
	mode := "❯"
	base := d.run
	d.run = func(c context.Context, b string, a ...string) (string, error) {
		if a[1] == "send-keys" && a[3] == "!" {
			mode = "!"
		}
		if a[1] == "read" {
			return inputBox(mode, pending), nil
		}
		return base(c, b, a...)
	}
	if _, err := d.probe(t.Context(), "host", "true", "fixture"); err == nil {
		t.Fatal("mutation accepted")
	}
	if !s.uncertain || s.cleanup() == nil {
		t.Fatal("uncertain execution not retained")
	}
	if _, err := os.Stat(filepath.Join(s.host.path, "p0")); err != nil {
		t.Fatal("uncertain file removed")
	}
}
func TestStagedSuccessCleanupAndExactInput(t *testing.T) {
	s := testStages(t)
	for _, p := range []string{"host", "guest"} {
		command, _, err := s.prepare(p, "printf 'one\\ntwo\\n'", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if !inputMatches(inputBox("!", command), "!", command) {
			t.Fatal("exact invocation rejected")
		}
		for _, bad := range []string{command + " ", command + ";true", strings.Replace(command, "sh ", "sh\n", 1)} {
			if inputMatches(inputBox("!", bad), "!", command) {
				t.Fatal("altered invocation accepted")
			}
		}
	}
	if err := s.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.host.path); !os.IsNotExist(err) {
		t.Fatal("host artifact retained", err)
	}
	if _, err := os.Stat(filepath.Join(s.guest.path, "p0")); err != nil {
		t.Fatal("guest artifact automatically removed", err)
	}

}
func TestStagingRejectsTemporaryShares(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newStagedScripts(repo, "host", "guest"); err == nil {
		t.Fatal("temporary share overlaps host storage")
	}
}

func TestStageDirectoryIdentityAncestry(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "CaseFolder")
	child := filepath.Join(a, "child")
	b := filepath.Join(root, "other")
	for _, p := range []string{child, b} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{a, child}, {child, a}, {a, a}} {
		yes, err := directoriesOverlap(pair[0], pair[1])
		if err != nil || !yes {
			t.Fatal(pair, yes, err)
		}
	}
	yes, err := directoriesOverlap(a, b)
	if err != nil || yes {
		t.Fatal(yes, err)
	}
	if _, err := directoriesOverlap(a, filepath.Join(root, "missing")); err == nil {
		t.Fatal("inspection failure ignored")
	}
	alias := filepath.Join(root, "casefolder")
	info, e := os.Stat(alias)
	orig, _ := os.Stat(a)
	if e != nil || !os.SameFile(info, orig) {
		t.Skip("case-insensitive filesystem unavailable")
	}
	yes, err = directoriesOverlap(alias, child)
	if err != nil || !yes {
		t.Fatal("case alias overlap bypass", err)
	}
}

func TestDirectorySwapCannotEscapeApprovedParent(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	var replacement string
	_, err := allocateScriptDirectory(parent, func(root *os.Root, name string) {
		replacement = filepath.Join(parent, name)
		if err := root.Remove(name); err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink(outside, name); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("followed replaced directory")
	}
	info, err := os.Lstat(replacement)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replacement removed", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatal("outside directory modified", err)
	}
	data, _ := os.ReadFile(sentinel)
	if string(data) != "unchanged" {
		t.Fatal("sentinel changed")
	}
}
