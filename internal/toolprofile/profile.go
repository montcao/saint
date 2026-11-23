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
	"time"
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

// openArtifact binds validation to the descriptor that will actually be read.
// Private host ownership remains the trust boundary; guest shares must not overlap.
func openArtifact(a Artifact) (*os.File, error) {
	if !digestValid(a.SHA256) {
		return nil, errors.New("invalid artifact SHA256")
	}
	if err := PrivatePath(a.Path, false); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || int(st.Uid) != os.Getuid() || st.Nlink != 1 || fi.Mode().Perm()&0o077 != 0 {
		f.Close()
		return nil, errors.New("opened artifact is not private and exclusive")
	}
	return f, nil
}

func Verify(a Artifact) error { return verifyContext(context.Background(), a) }

func verifyContext(ctx context.Context, a Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := openArtifact(a)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return fmt.Errorf("artifact digest mismatch: %s", a.Path)
	}
	return nil
}

func Load(ref string) (Manifest, error) { return loadContext(context.Background(), ref) }

func loadContext(ctx context.Context, ref string) (Manifest, error) {
	var m Manifest
	p, h, err := ParseRef(ref)
	if err != nil {
		return m, err
	}
	f, err := openArtifact(Artifact{p, h})
	if err != nil {
		return m, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return m, err
	}
	if len(data) > 65536 {
		return m, errors.New("manifest exceeds limit")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != h {
		return m, errors.New("manifest digest mismatch")
	}
	// Parse the very bytes whose digest was checked, never reopen the pathname.
	dec := json.NewDecoder(bytes.NewReader(data))
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
		if err = verifyContext(ctx, a); err != nil {
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
	switch op {
	case "probe", "create", "check", "stop", "remove":
	default:
		return errors.New("unsupported experimental adapter operation")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := verifyContext(ctx, adapter); err != nil {
		return err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if len(data) > 262144 {
		return errors.New("experimental adapter request exceeds limit")
	}
	cmd := exec.CommandContext(ctx, adapter.Path, op)
	// hdiutil is an adapter child. Cancel the owned process group as well so a
	// canceled helper cannot leave a late mount mutation after cleanup begins.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = bytes.NewReader(data)
	// Runtime errors can include request bodies or configuration. Never propagate
	// unstructured helper diagnostics to panes, logs or chat transcripts.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("experimental adapter %s failed (verify pinned runtime, ownership and reviewed artifacts): %w", op, err)
	}
	return nil
}

func Preflight(ctx context.Context, ref string) (Manifest, error) {
	m, err := loadContext(ctx, ref)
	if err != nil {
		return m, err
	}
	return m, Call(ctx, m.Adapter, "probe", nil)
}

// Contains compares filesystem identities along the inspected ancestor chain.
// Paths must exist and contain no symlink components. Inspection failures are
// returned to callers so allowlists and exclusions can both fail closed.
func Contains(path, root string) (bool, error) {
	chain, err := pathIdentities(path)
	if err != nil {
		return false, err
	}
	roots, err := pathIdentities(root)
	if err != nil {
		return false, err
	}
	for _, info := range chain {
		if os.SameFile(info, roots[0]) {
			return true, nil
		}
	}
	return false, nil
}

func pathIdentities(path string) ([]os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path must be absolute and canonical")
	}
	var chain []os.FileInfo
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (p != path && !info.IsDir()) {
			return nil, errors.New("unsafe path component")
		}
		chain = append(chain, info)
		if filepath.Dir(p) == p {
			return chain, nil
		}
	}
}

// Overlaps rejects either containment direction, including filesystem aliases.
// An uninspectable path cannot be established as disjoint.
func Overlaps(a, b string) bool {
	ab, err := Contains(a, b)
	if err != nil || ab {
		return true
	}
	ba, err := Contains(b, a)
	return err != nil || ba
}

// Stop hashing between reads when the enclosing pane operation is canceled.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
