package toolprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testArtifact(t *testing.T, dir, name, contents string) Artifact {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(contents))
	return Artifact{p, hex.EncodeToString(h[:])}
}
func testProfile(t *testing.T) (string, Manifest) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := testArtifact(t, dir, "adapter", "#!/bin/sh\n[ -z \"$SSH_AUTH_SOCK$ANTHROPIC_API_KEY$CONTAINER_APP_ROOT\" ] || exit 7\nexit 0\n")
	if err = os.Chmod(a.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	disk := testArtifact(t, dir, "tools", "tiny tool disk")
	digest := "sha256:" + strings.Repeat("1", 64)
	m := Manifest{Schema: 1, RuntimeCommit: RuntimeCommit, Format: "erofs", Adapter: a, Tools: disk, Root: testArtifact(t, dir, "root", "private root template"), Kernel: testArtifact(t, dir, "kernel", "kernel"), Review: testArtifact(t, dir, "review", "review fixture, not release qualification"), ToolSHA256: disk.SHA256, ImageReference: "example.test/scaffold@" + digest, ImageDigest: digest, InitReference: "example.test/init@" + digest, InitDigest: digest}
	b, _ := json.Marshal(m)
	manifest := testArtifact(t, dir, "manifest.json", string(b))
	return manifest.Path + "#" + manifest.SHA256, m
}
func TestExplicitManifestAndEnvironment(t *testing.T) {
	ref, _ := testProfile(t)
	for _, key := range []string{"SSH_AUTH_SOCK", "ANTHROPIC_API_KEY", "CONTAINER_APP_ROOT"} {
		t.Setenv(key, "must-not-reach-helper")
	}
	if _, err := Preflight(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}
func TestRejectChangedArtifact(t *testing.T) {
	ref, m := testProfile(t)
	if err := os.WriteFile(m.Tools.Path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ref); err == nil {
		t.Fatal("changed tool disk accepted")
	}
}
func TestRejectChangedManifest(t *testing.T) {
	ref, _ := testProfile(t)
	p, _, _ := ParseRef(ref)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(" ")
	_ = f.Close()
	if _, err = Load(ref); err == nil {
		t.Fatal("changed manifest accepted")
	}
}
func TestRejectSymlinksHardlinksAndPermissions(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public", "parent"} {
		t.Run(kind, func(t *testing.T) {
			ref, m := testProfile(t)
			switch kind {
			case "symlink":
				if err := os.Rename(m.Tools.Path, m.Tools.Path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(m.Tools.Path+".real", m.Tools.Path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(m.Root.Path, m.Root.Path+".alias"); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(m.Kernel.Path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "parent":
				if err := os.Chmod(filepath.Dir(m.Tools.Path), 0o777); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(m.Tools.Path), 0o700) })
			}
			if _, err := Load(ref); err == nil {
				t.Fatal("unsafe profile accepted")
			}
		})
	}
}
func TestProfileRefRequiresExplicitDigest(t *testing.T) {
	for _, ref := range []string{"", "raw", "/tmp/manifest.json", "relative#" + strings.Repeat("1", 64), "/tmp/../x#" + strings.Repeat("1", 64)} {
		if _, _, err := ParseRef(ref); err == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
}

func TestCallDoesNotExposeHelperDiagnostics(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := testArtifact(t, dir, "helper", "#!/bin/sh\nprintf 'synthetic-private-credential' >&2\nexit 1\n")
	if err = os.Chmod(a.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	err = Call(context.Background(), a, "probe", nil)
	if err == nil || strings.Contains(err.Error(), "synthetic-private-credential") {
		t.Fatalf("diagnostics boundary failed: %v", err)
	}
}

func TestCallBoundsInputAndRejectsUnknownOperations(t *testing.T) {
	_, m := testProfile(t)
	if err := Call(context.Background(), m.Adapter, "create", strings.Repeat("x", 262145)); err == nil {
		t.Fatal("oversized request accepted")
	}
	if err := Call(context.Background(), m.Adapter, "unrecognized", nil); err == nil {
		t.Fatal("unknown operation accepted")
	}
}

func TestCallCancellationBoundsDescendantWait(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := testArtifact(t, dir, "helper", "#!/bin/sh\n/bin/sleep 30 &\nwait\n")
	if err = os.Chmod(a.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err = Call(ctx, a, "probe", nil); err == nil {
		t.Fatal("canceled helper succeeded")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("descendant held helper pipes open")
	}
}
