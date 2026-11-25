// Package claudeimage resolves and prepares exact host-matched Claude images.
package claudeimage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	clauderecipe "github.com/montcao/saint/images/claude"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

func ValidVersion(v string) bool { return versionPattern.MatchString(v) }
func ParseVersion(output string) (string, error) {
	v := strings.TrimSuffix(strings.TrimSpace(output), " (Claude Code)")
	if !ValidVersion(v) {
		return "", errors.New("Claude --version must report an exact stable numeric version (for example 2.1.1); update or select a supported installed CLI, not latest")
	}
	return v, nil
}

type Selection struct {
	Digest         string `json:"digest"`
	Version        string `json:"version"`
	Architecture   string `json:"architecture"`
	Image          string `json:"image"`
	Recipe         string `json:"recipe"`
	HostExecutable string `json:"host_executable"`
	Cached         bool   `json:"cached"`
}

func ValidSelection(version, arch, recipe, image string) bool {
	if !ValidVersion(version) || (arch != "arm64" && arch != "amd64") || len(recipe) != 64 {
		return false
	}
	if _, err := hex.DecodeString(recipe); err != nil {
		return false
	}
	return image == "saint/claude:v"+version+"-"+arch+"-r"+recipe
}
func Select(version, arch string, recipe []byte) (Selection, error) {
	if !ValidVersion(version) {
		return Selection{}, errors.New("invalid Claude version")
	}
	if arch != "arm64" && arch != "amd64" {
		return Selection{}, errors.New("unsupported Linux guest architecture")
	}
	h := sha256.Sum256(append([]byte("saint-claude-image-v1\n"), recipe...))
	digest := hex.EncodeToString(h[:])
	return Selection{Version: version, Architecture: arch, Recipe: digest, Image: "saint/claude:v" + version + "-" + arch + "-r" + digest}, nil
}

// Run never invokes a shell on the host. Output from probes is bounded; build
// progress goes to the chosen writer, never into image layers or metadata.
type Runner func(context.Context, string, []string, io.Writer) (string, error)
type Manager struct {
	Container    string
	LockDir      string
	Architecture string
	Progress     io.Writer
	Run          Runner
	LookPath     func(string) (string, error)
}

func (m Manager) runner() Runner {
	if m.Run != nil {
		return m.Run
	}
	return run
}
func (m Manager) Installed(ctx context.Context) (version, bin string, err error) {
	lookup := m.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	bin, err = lookup("claude")
	if err != nil {
		return "", "", errors.New("installed host Claude CLI not found on PATH; install/select the desired CLI before creating a Claude session")
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return "", "", err
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return "", "", errors.New("cannot resolve installed host Claude executable")
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner()(bounded, bin, []string{"--version"}, nil)
	if err != nil {
		return "", "", errors.New("installed host Claude --version failed; check the selected CLI")
	}
	version, err = ParseVersion(output)
	if err != nil {
		return "", "", err
	}
	return version, bin, nil
}
func (m Manager) Resolve(ctx context.Context) (Selection, []byte, error) {
	version, bin, err := m.Installed(ctx)
	if err != nil {
		return Selection{}, nil, err
	}
	recipe := clauderecipe.Recipe()
	arch := m.Architecture
	if arch == "" {
		arch = runtime.GOARCH
	}
	selected, err := Select(version, arch, recipe)
	selected.HostExecutable = bin
	return selected, recipe, err
}
func (m Manager) Prepare(ctx context.Context) (Selection, error) {
	selected, recipe, err := m.Resolve(ctx)
	if err != nil {
		return Selection{}, err
	}
	out := m.Progress
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "saint: installed host Claude CLI %s; Linux/%s image %s. Running host agents may have a different version.\n", selected.Version, selected.Architecture, selected.Image)
	unlock, err := lock(ctx, m.LockDir, selected.Recipe+"-"+selected.Version+"-"+selected.Architecture)
	if err != nil {
		return Selection{}, err
	}
	defer unlock()
	run := m.runner()
	digest, present, err := m.cachedDigest(ctx, selected.Image)
	if err != nil {
		return Selection{}, err
	}
	if present {
		selected.Cached = true
		fmt.Fprintln(out, "saint: reusing cached Claude image; verifying its executable")
	} else {
		fmt.Fprintf(out, "saint: building exact Claude %s for Linux/%s. This may take several minutes; no substitute version will be used.\n", selected.Version, selected.Architecture)
		dir, err := os.MkdirTemp("", "saint-claude-build-")
		if err != nil {
			return Selection{}, err
		}
		defer os.RemoveAll(dir)
		// Only the recipe enters the context: no repository, host binary, npmrc,
		// credentials, SSH agent or private Claude configuration.
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), recipe, 0600); err != nil {
			return Selection{}, err
		}
		args := []string{"build", "--platform", "linux/" + selected.Architecture, "--progress", "plain", "--build-arg", "CLAUDE_VERSION=" + selected.Version, "-t", selected.Image, dir}
		if _, err := run(ctx, m.Container, args, out); err != nil {
			return Selection{}, fmt.Errorf("exact Claude %s image build failed: the version may be unavailable or incompatible with Linux/%s; check the build output and installed host version; no fallback was used: %w", selected.Version, selected.Architecture, err)
		}
	}
	if !present {
		digest, present, err = m.cachedDigest(ctx, selected.Image)
		if err != nil {
			return Selection{}, err
		}
		if !present {
			return Selection{}, errors.New("build completed without the exact requested image")
		}
	}
	selected.Digest = digest
	if err := m.verify(ctx, selected); err != nil {
		return Selection{}, err
	}
	fmt.Fprintf(out, "saint: verified guest Claude %s; self-updates remain disabled\n", selected.Version)
	return selected, nil
}

// VerifyRetained never resolves the host or builds a replacement for an existing session.
func (m Manager) VerifyRetained(ctx context.Context, s Selection) error {
	if !ValidSelection(s.Version, s.Architecture, s.Recipe, s.Image) {
		return errors.New("invalid retained Claude image identity")
	}
	digest, present, err := m.cachedDigest(ctx, s.Image)
	if err != nil {
		return err
	}
	if !present || digest != s.Digest || !ValidDigest(s.Digest) {
		return errors.New("retained Claude image is missing or changed; restore that exact image before renewing this session")
	}
	return m.verify(ctx, s)
}
func ValidDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	_, e := hex.DecodeString(s[7:])
	return e == nil
}

// A successful name inventory establishes absence. Any read/parse/limit error
// stops creation; it must never rebuild over a possibly existing image.
func (m Manager) cachedDigest(ctx context.Context, image string) (string, bool, error) {
	names, err := m.runner()(ctx, m.Container, []string{"image", "list", "--quiet"}, nil)
	if err != nil {
		return "", false, errors.New("cannot inspect image cache; check the container service and retry (no image rebuilt)")
	}
	present := false
	for _, name := range strings.Split(strings.TrimSpace(names), "\n") {
		if name == image || name == "docker.io/"+image {
			present = true
		}
	}
	if !present {
		return "", false, nil
	}
	return m.inspectDigest(ctx, image)
}

func (m Manager) inspectDigest(ctx context.Context, image string) (string, bool, error) {
	raw, err := m.runner()(ctx, m.Container, []string{"image", "inspect", image}, nil)
	if err != nil {
		return "", false, errors.New("cannot inspect cached image identity; no image rebuilt")
	}
	var entries []struct {
		Configuration struct {
			Name       string `json:"name"`
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"configuration"`
	}
	if json.Unmarshal([]byte(raw), &entries) != nil || len(entries) != 1 {
		return "", false, errors.New("invalid cached image metadata; no image rebuilt")
	}
	entry := entries[0].Configuration
	if (entry.Name != image && entry.Name != "docker.io/"+image) || !ValidDigest(entry.Descriptor.Digest) {
		return "", false, errors.New("cached image identity mismatch; no image rebuilt")
	}
	return entry.Descriptor.Digest, true, nil
}

func (m Manager) verify(ctx context.Context, s Selection) (err error) {
	// Apple container resolves local references by name, including digest-qualified
	// aliases. Register and inspect that alias before execution; never let a
	// missing local alias trigger an implicit remote pull.
	ref := Reference(s.Image, s.Digest)
	digest, present, e := m.cachedDigest(ctx, ref)
	if e != nil {
		return e
	}
	if !present {
		if _, e = m.runner()(ctx, m.Container, []string{"image", "tag", s.Image, ref}, nil); e != nil {
			return errors.New("cannot register local digest-qualified image reference")
		}
		digest, present, e = m.inspectDigest(ctx, ref)
	}
	if e != nil || !present || digest != s.Digest {
		return errors.New("local digest-qualified image identity mismatch; no guest executed")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	name := "saint-version-" + hex.EncodeToString(nonce[:])
	run := m.runner()
	if _, err = run(ctx, m.Container, []string{"network", "create", "--internal", name}, nil); err != nil {
		return fmt.Errorf("cannot confirm creation of owned verification network %s; inspect it before cleanup", name)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, e := run(cleanup, m.Container, []string{"network", "delete", name}, nil); e != nil {
			err = errors.Join(err, fmt.Errorf("verification network %s remains; inspect its owned verification container before cleanup", name))
		}
	}()
	bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	output, e := run(bounded, m.Container, []string{"run", "--rm", "--name", name, "--network", name, "--platform", "linux/" + s.Architecture, "--cpus", "1", "--memory", "1G", "--user", "agent", "--env", "DISABLE_UPDATES=1", "--entrypoint", "/usr/local/bin/claude", Reference(s.Image, s.Digest), "--version"}, nil)
	if e != nil {
		return fmt.Errorf("Claude %s failed Linux/%s verification; inspect owned container %s if cleanup is needed; no agent launched", s.Version, s.Architecture, name)
	}
	actual, e := ParseVersion(output)
	if e != nil || actual != s.Version {
		return fmt.Errorf("guest Claude version mismatch: requested %s; rebuild/remove only image %s before retrying; no agent launched", s.Version, s.Image)
	}
	return nil
}
func lock(ctx context.Context, dir, key string) (func(), error) {
	if dir == "" {
		return nil, errors.New("image cache lock directory is not configured")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, key+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type boundedBuffer struct{ data bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > 65536-b.data.Len() {
		return 0, errors.New("version/image inspection output exceeded 64 KiB")
	}
	return b.data.Write(p)
}
func run(ctx context.Context, bin string, args []string, progress io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	// The container CLI needs its ordinary per-user service location, but no
	// provider keys, npm config, SSH agent or platform override from the caller.
	for _, k := range []string{"HOME", "PATH", "TMPDIR", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var output boundedBuffer
	if progress != nil {
		cmd.Stdout = progress
		cmd.Stderr = progress
	} else {
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
	}
	err := cmd.Run()
	return output.data.String(), err
}

// Reference omits the tag because Apple container normalizes digest references
// to repository@digest when registering them locally.
func Reference(image, digest string) string {
	return strings.SplitN(image, ":", 2)[0] + "@" + digest
}
