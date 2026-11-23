// Package toolprofile verifies explicitly selected, locally reviewed experimental
// profiles. It never downloads artifacts, changes runtime settings, or picks a default.
package toolprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const RuntimeCommit = "a15b46097aa4c2325da13c1fb5192b9274aab083"

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// The manifest digest is an explicit local trust decision, not a publisher signature.
// A release must additionally establish publisher provenance and security review.
type Manifest struct {
	Schema         int      `json:"schema"`
	RuntimeCommit  string   `json:"runtime_commit"`
	Format         string   `json:"format"`
	Adapter        Artifact `json:"adapter"`
	Tools          Artifact `json:"tools"`
	Root           Artifact `json:"root"`
	Kernel         Artifact `json:"kernel"`
	ToolSHA256     string   `json:"tool_sha256"`
	ImageReference string   `json:"image_reference"`
	ImageDigest    string   `json:"image_digest"`
	InitReference  string   `json:"init_reference"`
	InitDigest     string   `json:"init_digest"`
	// Review is a checked-in/local report covering the entire guest tool inventory,
	// kernel, init image and dependency lock. Its hash makes the chosen review explicit.
	Review Artifact `json:"review"`
}

func digestValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

func ParseRef(ref string) (string, string, error) {
	p, h, ok := strings.Cut(ref, "#")
	if !ok || !filepath.IsAbs(p) || filepath.Clean(p) != p || !digestValid(h) {
		return "", "", errors.New("experimental profile requires /absolute/manifest.json#<sha256>; review the manifest before opting in")
	}
	return p, h, nil
}

// PrivatePath rejects symlinks at every component, unsafe ownership and writable
// parent directories. Sticky system temporary roots are permitted; the immediate
// artifact directory must still be private to this uid.
func PrivatePath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("path must be absolute and canonical")
	}
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe path: %s", p)
		}
		if p == path {
			if fi.IsDir() != directory || (!directory && !fi.Mode().IsRegular()) || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 || (!directory && st.Nlink != 1) {
				return fmt.Errorf("%s must be owned, private, regular (or directory), and have no hardlink aliases", p)
			}
		} else if !fi.IsDir() || (st.Uid != 0 && int(st.Uid) != os.Getuid()) || (fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0) {
			return fmt.Errorf("unsafe ancestor: %s", p)
		}
		if p == "/" {
			break
		}
	}
	return nil
}

func Verify(a Artifact) error {
	if !digestValid(a.SHA256) {
		return errors.New("invalid artifact SHA256")
	}
	if err := PrivatePath(a.Path, false); err != nil {
		return err
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("artifact digest mismatch: %s", a.Path)
	}
	return nil
}

func Load(ref string) (Manifest, error) {
	var m Manifest
	p, h, err := ParseRef(ref)
	if err != nil {
		return m, err
	}
	if err = Verify(Artifact{p, h}); err != nil {
		return m, err
	}
	f, err := os.Open(p)
	if err != nil {
		return m, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return m, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return m, errors.New("trailing manifest data")
	}
	if m.Schema != 1 || m.RuntimeCommit != RuntimeCommit || (m.Format != "erofs" && m.Format != "ulfo") || !digestValid(m.ToolSHA256) {
		return m, errors.New("unsupported profile schema, runtime commit or disk format")
	}
	for _, v := range []struct{ ref, digest string }{{m.ImageReference, m.ImageDigest}, {m.InitReference, m.InitDigest}} {
		if !strings.HasSuffix(v.ref, "@"+v.digest) || !strings.HasPrefix(v.digest, "sha256:") || !digestValid(strings.TrimPrefix(v.digest, "sha256:")) {
			return m, errors.New("image and init references must be pinned by SHA256")
		}
	}
	for _, a := range []Artifact{m.Adapter, m.Tools, m.Root, m.Kernel, m.Review} {
		if err = Verify(a); err != nil {
			return m, err
		}
	}
	if m.Format == "erofs" && m.Tools.SHA256 != m.ToolSHA256 {
		return m, errors.New("raw tool digest differs")
	}
	return m, nil
}

// Call passes only explicit input and a minimal environment to the verified
// helper. Provider credentials, SSH_AUTH_SOCK and CONTAINER_* overrides are absent.
func Call(ctx context.Context, adapter Artifact, op string, input any) error {
	if err := Verify(adapter); err != nil {
		return err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, adapter.Path, op)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("experimental adapter %s: %w: %s", op, err, stderr.String())
	}
	return nil
}

func Preflight(ctx context.Context, ref string) (Manifest, error) {
	m, err := Load(ref)
	if err != nil {
		return m, err
	}
	return m, Call(ctx, m.Adapter, "probe", nil)
}

func Overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}
