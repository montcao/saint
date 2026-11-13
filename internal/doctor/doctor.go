package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// sw_vers and uname ship with macOS at fixed absolute paths, so we pin them
// rather than resolving them through PATH at call time: a shim earlier in PATH
// would otherwise get to impersonate them (and, for security(1), harvest the
// user's API keys). container and git are deliberately left on exec.LookPath in
// cmd/saint because their install location legitimately varies - Homebrew
// prefixes, /usr/local, per-user toolchains - so there is no single correct
// absolute path to pin.
const (
	SwVersPath = "/usr/bin/sw_vers"
	UnamePath  = "/usr/bin/uname"
)

type Result struct {
	Name   string
	OK     bool
	Detail string
	Fix    string
}

type Doctor struct {
	ContainerPath string
	GitPath       string
	Store         *store.Store
	Keychain      *keychain.Keychain
	RepoRoot      string

	// ProxyStatus asks the running proxy, over its control socket, which
	// addresses it listens on. An error means no proxy answers. Nil skips
	// the proxy checks.
	ProxyStatus func(ctx context.Context) (listeners []string, err error)
}

func (d *Doctor) Run(ctx context.Context) ([]Result, error) {
	results := make([]Result, 0, 8)

	swVers, err := run(ctx, SwVersPath, "-productVersion")
	results = append(results, Result{
		Name:   "macOS version",
		OK:     err == nil && meetsMinimum(swVers, MinimumMacOS),
		Detail: detailOrError(swVers, err),
		Fix:    fmt.Sprintf("upgrade macOS to %d or newer", MinimumMacOS.Major),
	})

	arch, err := run(ctx, UnamePath, "-m")
	results = append(results, Result{
		Name:   "architecture",
		OK:     err == nil && strings.TrimSpace(arch) == "arm64",
		Detail: detailOrError(arch, err),
		Fix:    "use an Apple silicon Mac",
	})

	containerVersion, err := "", errors.New("not found on PATH")
	if d.ContainerPath != "" {
		containerVersion, err = run(ctx, d.ContainerPath, "--version")
	}
	results = append(results, Result{
		Name:   "container version",
		OK:     err == nil && meetsMinimum(containerVersion, MinimumContainerVersion),
		Detail: detailOrError(containerVersion, err),
		Fix:    fmt.Sprintf("install container CLI %s or newer", MinimumContainerVersion),
	})

	statusRaw, err := run(ctx, d.ContainerPath, "system", "status", "--format", "json")
	status := struct {
		Status string `json:"status"`
	}{}
	_ = json.Unmarshal([]byte(statusRaw), &status)
	results = append(results, Result{
		Name:   "container system",
		OK:     err == nil && status.Status == "running",
		Detail: strings.TrimSpace(status.Status),
		Fix:    "container system start --enable-kernel-install",
	})

	cfg, cfgErr := d.Store.EnsureConfig()
	results = append(results, Result{
		Name:   "~/.saint config",
		OK:     cfgErr == nil,
		Detail: d.Store.Paths().ConfigPath,
		Fix:    "mkdir -p ~/.saint",
	})

	for _, runtime := range RuntimeImageNames {
		imageName := "saint/" + runtime + ":latest"
		_, imageErr := run(ctx, d.ContainerPath, "image", "inspect", imageName)
		results = append(results, Result{
			Name:   imageName,
			OK:     imageErr == nil,
			Detail: "runtime image present",
			Fix:    fmt.Sprintf("container build -t %s %s", imageName, imageBuildContext(d.RepoRoot, runtime)),
		})
	}

	for _, provider := range []keychain.Provider{keychain.ProviderAnthropic, keychain.ProviderOpenAI} {
		ok, keyErr := d.Keychain.Has(ctx, provider)
		results = append(results, Result{
			Name:   "keychain " + string(provider),
			OK:     keyErr == nil && ok,
			Detail: "service=saint account=" + string(provider),
			Fix:    fmt.Sprintf("saint keychain set %s", provider),
		})
	}

	results = append(results, d.authResults(ctx, cfg, cfgErr)...)
	results = append(results, d.proxyResults(ctx, cfg, cfgErr)...)

	// `git config --get` exits 1 when the key is unset, which is the good state
	// here, so the exit status is intentionally ignored.
	hooksPath, _ := run(ctx, d.GitPath, "config", "--global", "core.hooksPath")
	paths := d.Store.Paths()
	results = append(results, hooksPathResult(strings.TrimSpace(hooksPath), paths.Home, paths.Root))

	results = append(results, Result{
		Name:   "default runtime",
		OK:     cfgErr == nil && cfg.DefaultRuntime != "",
		Detail: string(cfg.DefaultRuntime),
		Fix:    fmt.Sprintf("edit %s", d.Store.Paths().ConfigPath),
	})

	return results, nil
}

// hooksPathResult reports on the user's global core.hooksPath.
//
// saint deliberately does NOT recommend setting this: it is a global git
// setting, so pointing it anywhere silently disables every one of the user's
// repos' own hooks and turns a single directory into host code execution on
// every git command they run afterwards. Unset is the only good state. If it
// points inside ~/.saint - a tree whose session directories are mounted into
// guests - a compromised guest gets host code execution, so that case is called
// out explicitly.
func hooksPathResult(value, home, saintRoot string) Result {
	const name = "global core.hooksPath"
	const fix = "git config --global --unset core.hooksPath"

	if value == "" {
		return Result{Name: name, OK: true, Detail: "unset", Fix: fix}
	}

	detail := value + " overrides every repo's own hooks for all your git usage"
	if resolved := expandHome(value, home); underPath(resolved, saintRoot) {
		detail = value + " is inside " + saintRoot + "; anything that can write there gets host code execution on your next git command (sandbox-escape risk)"
	}
	return Result{Name: name, OK: false, Detail: detail, Fix: fix}
}

func expandHome(path, home string) string {
	if home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func underPath(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func detailOrError(output string, err error) string {
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(output)
}

func Format(results []Result) string {
	var b strings.Builder
	for _, result := range results {
		if result.OK {
			fmt.Fprintf(&b, "%s: ok", result.Name)
			if result.Detail != "" {
				fmt.Fprintf(&b, " (%s)", result.Detail)
			}
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, "%s: fix: %s", result.Name, result.Fix)
		if result.Detail != "" {
			fmt.Fprintf(&b, " (%s)", result.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func BuildImages(ctx context.Context, containerPath, repoRoot string) error {
	if repoRoot == "" {
		return fmt.Errorf("cannot locate the saint checkout containing images/; run saint doctor --build-images from inside it")
	}
	for _, runtime := range RuntimeImageNames {
		context := filepath.Join(repoRoot, "images", runtime)
		cmd := exec.CommandContext(ctx, containerPath, "build", "-t", "saint/"+runtime+":latest", context)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			// Keep the underlying error: a build that fails without writing to
			// stderr would otherwise produce an empty, unactionable message.
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				return fmt.Errorf("build %s: %w", context, err)
			}
			return fmt.Errorf("build %s: %w: %s", context, err, detail)
		}
	}
	return nil
}

func run(ctx context.Context, bin string, args ...string) (string, error) {
	if filepath.IsAbs(bin) {
		if err := requireExecutable(bin); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// requireExecutable verifies a pinned absolute path really is there before we
// try to run it, so a missing system binary produces an error naming the exact
// path instead of a bare "no such file or directory".
func requireExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("system binary %s: %w", path, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("system binary %s is not executable", path)
	}
	return nil
}

// proxyResults checks that the API proxy can actually be reached by a guest.
//
// The container gateway check is the runtime's own network: without it, no
// network a session creates will have one either. The bind check matters only
// for sessions from before per-session networks, which reach the proxy at
// proxy_bind; a proxy pinned to loopback binds fine and is invisible to every
// guest. Then the running proxy is asked, over its control socket, which
// session networks it serves, and each is probed, because a listener the
// macOS firewall blocks looks healthy until something connects.
func (d *Doctor) proxyResults(ctx context.Context, cfg store.Config, cfgErr error) []Result {
	if cfgErr != nil {
		return []Result{{
			Name:   "proxy",
			OK:     false,
			Detail: "config unavailable: " + cfgErr.Error(),
			Fix:    "check ~/.saint/config.json",
		}}
	}

	network := cfg.ProxyNetworkOrDefault()

	gateway, gwErr := sandbox.GatewayAddr(ctx, d.ContainerPath, network)
	results := []Result{{
		Name:   "container gateway",
		OK:     gwErr == nil,
		Detail: detailOrError(gateway, gwErr),
		Fix:    fmt.Sprintf("start the container system, or set proxy_network in %s", d.Store.Paths().ConfigPath),
	}}

	results = append(results, Result{
		Name:   "proxy bind",
		OK:     !cfg.ProxyBindIsLoopback(),
		Detail: cfg.ProxyBindHost(),
		Fix: fmt.Sprintf("set proxy_bind to %q in %s; guests cannot reach host loopback",
			store.ProxyBindAuto, d.Store.Paths().ConfigPath),
	})

	if d.ProxyStatus == nil {
		return results
	}
	listeners, err := d.ProxyStatus(ctx)
	if err != nil {
		return append(results, Result{
			Name:   "proxy running",
			OK:     false,
			Detail: "no proxy answers on " + d.Store.Paths().ControlSocket,
			Fix:    "open a saint pane in herdr, or run: saint proxy",
		})
	}
	detail := "control socket answers; no session network served yet"
	if len(listeners) > 0 {
		detail = "control socket answers; serving " + strings.Join(listeners, ", ")
	}
	results = append(results, Result{Name: "proxy running", OK: true, Detail: detail})
	for _, addr := range listeners {
		results = append(results, proxyReachableResult(addr))
	}
	return results
}

// proxyReachableResult distinguishes "no proxy running" from "proxy running but
// the macOS application firewall is dropping connections to it".
//
// That second case is easy to miss and hard to diagnose: the proxy binds the
// bridge gateway successfully and lsof shows it LISTENing, but every connection
// times out, because the firewall blocks inbound connections to non-loopback
// interfaces for binaries it has not been told to allow. A probe with `python3 -m
// http.server` does not show this, because /usr/bin/python3 is usually already
// in the firewall allowlist (docs/research/spikes.md).
func proxyReachableResult(addr string) Result {
	const name = "proxy reachable"

	if proxyReachable(addr) {
		return Result{Name: name, OK: true, Detail: addr}
	}

	// A listener we can see but cannot connect to is the firewall signature.
	if listening, owner := localListener(addr); listening {
		return Result{
			Name:   name,
			OK:     false,
			Detail: addr + " is bound by " + owner + " but refuses connections (macOS application firewall)",
			Fix:    firewallFix(),
		}
	}

	return Result{
		Name:   name,
		OK:     false,
		Detail: addr,
		Fix:    "open a saint pane in herdr, or run: saint proxy",
	}
}

// firewallFix names the exact commands that let saint accept connections on the
// container bridge. Both require admin privileges, so they are printed for the
// user to run rather than attempted.
func firewallFix() string {
	const fw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "$(command -v saint)"
	}
	return fmt.Sprintf("sudo %s --add %s && sudo %s --unblockapp %s", fw, exe, fw, exe)
}

// localListener reports whether this host has a process listening on addr.
// A bound-but-unreachable socket is what separates a firewall block from a
// proxy that simply is not running.
func localListener(addr string) (bool, string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, ""
	}
	// lsof is the portable-enough way to ask this on macOS without root.
	out, err := exec.Command("/usr/sbin/lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil || len(out) == 0 {
		return false, ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return false, ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) == 0 {
		return false, ""
	}
	return true, fields[0]
}

// proxyReachable reports whether something accepts connections at addr.
func proxyReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
