package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/doctor"
	"github.com/montcao/saint/internal/inventory"
	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

// systemSecurityPath is pinned rather than resolved through PATH: a `security`
// shim earlier in PATH would harvest both API keys on the next run.
const systemSecurityPath = "/usr/bin/security"

// requireSystemBinary verifies a pinned absolute path is present and executable
// so a missing one produces an error naming the exact path.
func requireSystemBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("system binary %s: %w", path, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("system binary %s is not executable", path)
	}
	return nil
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	st, err := store.NewFromEnv(home)
	if err != nil {
		return err
	}
	logger := newDebugLogger(st)

	// container and git stay on exec.LookPath on purpose: both are installed
	// wherever the user's package manager put them (Homebrew prefixes,
	// /usr/local, per-user toolchains), so there is no single correct absolute
	// path to pin. security, sw_vers and uname are different - they ship with
	// macOS at fixed /usr/bin locations, and resolving them through PATH would
	// let a shim earlier in PATH impersonate them. A fake `security` would
	// harvest both API keys on the next run, so those three are pinned instead.
	// (doctor pins sw_vers and uname; see internal/doctor.)
	//
	// A missing container CLI is not fatal here. saint starts anyway so the
	// first-run screen and `saint doctor` can say what is missing; an empty
	// path makes every runtime call report sandbox.ErrNotInstalled.
	containerPath, err := exec.LookPath("container")
	if err != nil {
		logger.Printf("container CLI not found on PATH: %v", err)
		containerPath = ""
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	if err := requireSystemBinary(systemSecurityPath); err != nil {
		return err
	}

	sb := sandbox.NewAppleContainer(containerPath, logger.Printf)
	kc := keychain.New(systemSecurityPath, logger.Printf)
	inspector := inventory.NewRepoInspector(gitPath, logger.Printf)
	// Sessions created before tokens moved out of the mounted session directory
	// still keep theirs in the old place, where the proxy no longer looks. A
	// failure here is not fatal - it costs those sessions API access, not
	// every command - but it must be visible.
	if err := st.MigrateProxyTokens(); err != nil {
		logger.Printf("proxy token migration failed: %v", err)
	}

	loader := &inventory.Loader{Store: st, Sandbox: sb, Inspector: inspector}
	workdir, err := os.Getwd()
	if err != nil {
		return err
	}
	svc := &session.Service{
		Store:   st,
		Sandbox: sb,
		Workdir: workdir,
		Logf:    logger.Printf,
	}
	svc.Credentials = func(spec string) (credential.Source, error) {
		return sessionCredential(st, kc, spec)
	}

	// Every command that starts a session goes through deps, which sets the
	// proxy URL and starts the proxy. An unset ProxyURL would leave the
	// guest talking to the provider directly with whatever it can find.
	deps := sessionDeps{
		svc:         svc,
		ensureProxy: newProxyStarter(st).ensure,
		listen: func(ctx context.Context, sessionID string) (string, error) {
			resp, err := controlCall(ctx, st.Paths().ControlSocket, controlRequest{Op: controlOpListen, Session: sessionID})
			return resp.Addr, err
		},
		probe:        probeProxy,
		reachTimeout: proxyReachTimeout,
		startService: sb.StartSystem,
		checkService: func(ctx context.Context) error {
			_, err := sb.List(ctx)
			return err
		},
		pane: currentHerdrPane,
	}

	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usageText)
		return nil
	}

	switch args[0] {
	case "ls":
		return runList(ctx, loader)
	case "doctor":
		return runDoctor(ctx, st, kc, containerPath, gitPath, args[1:])
	case "proxy":
		if len(args) > 1 && args[1] == "log" {
			return runProxyLog(st, args[2:])
		}
		return runProxy(ctx, st, kc, containerPath, logger)
	case "auth":
		return runAuth(ctx, st, kc, args[1:])
	case "keychain":
		return runKeychain(ctx, kc, args[1:])
	case "new":
		return runNew(ctx, deps, args[1:])
	case "attach":
		return runAttach(ctx, deps, args[1:])
	case "stop":
		return runStop(ctx, svc, args[1:])
	case "rm":
		return runRemove(ctx, svc, args[1:])
	case "herdr":
		return runHerdr(ctx, deps, args[1:])
	case "allow":
		return runAllow(st, args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runList(ctx context.Context, loader *inventory.Loader) error {
	snapshot, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	fmt.Print(inventory.FormatTable(snapshot))
	for _, warning := range snapshot.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	return nil
}

const usageText = `saint runs coding agents in Apple container microVMs, behind a proxy that
holds their credentials. In herdr, open a sandboxed pane with the saint.new
action; see README.md.

Usage:
  saint new [-rt claude|codex] [-name N] [-ask] <repo>...   create a session and attach
  saint attach <session>     attach to a session
  saint ls                   list sessions and files to review
  saint stop <session>       stop a session's microVM
  saint rm <session>         delete a session (repositories are not touched)
  saint auth                 show and set credentials
  saint proxy                run the credential proxy in the foreground
  saint proxy log            what the agents called, most recent first
  saint allow [<host>]       list, or save, hosts every session may reach
  saint allow -rm <host>     remove a saved host
  saint doctor               check the host, runtime, credentials and proxy
  saint herdr open|pane|egress   herdr plugin entrypoints
`

func runDoctor(ctx context.Context, st *store.Store, kc *keychain.Keychain, containerPath, gitPath string, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	buildImages := fs.Bool("build-images", false, "attempt image builds")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Not os.Getwd(): doctor is expected to be run from whatever repo the user
	// is working in, and the binary may be installed anywhere, so the checkout
	// holding images/ is located rather than assumed. An empty result is
	// handled downstream by printing a placeholder instead of a wrong path.
	repoRoot := doctor.ResolveRepoRoot(doctor.DefaultRepoRootCandidates()...)
	doc := &doctor.Doctor{
		ContainerPath: containerPath,
		GitPath:       gitPath,
		Store:         st,
		Keychain:      kc,
		RepoRoot:      repoRoot,
		ProxyStatus: func(ctx context.Context) ([]string, error) {
			resp, err := controlCall(ctx, st.Paths().ControlSocket, controlRequest{Op: controlOpPing})
			return resp.Listeners, err
		},
	}
	results, err := doc.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Print(doctor.Format(results))
	if *buildImages {
		if err := doctor.BuildImages(ctx, containerPath, repoRoot); err != nil {
			return fmt.Errorf("build images: %w", err)
		}
	}
	return nil
}

// resolveProxyBindHost turns the configured bind setting into a concrete
// address, discovering the container bridge gateway when set to "auto".
func resolveProxyBindHost(ctx context.Context, cfg store.Config, containerPath string) (string, error) {
	if !cfg.ProxyBindIsAuto() {
		return cfg.ProxyBindHost(), nil
	}
	return sandbox.GatewayAddr(ctx, containerPath, cfg.ProxyNetworkOrDefault())
}

// keychainAdapter lets internal/credential use the keychain without importing
// its Provider type.
type keychainAdapter struct{ kc *keychain.Keychain }

func (a keychainAdapter) Get(ctx context.Context, account string) (string, error) {
	if a.kc == nil {
		return "", errors.New("keychain is not configured")
	}
	return a.kc.Get(ctx, keychain.Provider(account))
}

// opPath locates the 1Password CLI, or returns "" when it is not installed.
// A missing op is only an error for a user who actually configured an op://
// reference, and credential.Parse reports that.
func opPath() string {
	path, err := exec.LookPath("op")
	if err != nil {
		return ""
	}
	return path
}

// credentialSource resolves one configured secret reference.
func credentialSource(cfg store.Config, name, fallbackAccount string, kc *keychain.Keychain) (credential.Source, error) {
	return credential.Parse(cfg.CredentialSpec(name), fallbackAccount, keychainAdapter{kc: kc}, opPath())
}

// sessionCredential resolves the credential a session asked for.
//
// The spec is either a source name a session chose - "claude-code" for the
// host's live login - or empty, meaning whatever ~/.saint/config.json has
// configured for the subscription credential.
func sessionCredential(st *store.Store, kc *keychain.Keychain, spec string) (credential.Source, error) {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec) == "" {
		spec = cfg.CredentialSpec(store.CredentialClaudeSubscription)
	}
	return credential.Parse(spec, string(keychain.ProviderClaudeSubscription), keychainAdapter{kc: kc}, opPath())
}

// proxyAuthMode maps a stored session's mode onto the proxy's own constants.
//
// The strings are equal today. The mapping exists so that changing either side
// is a compile error rather than a silent change to which account pays.
func proxyAuthMode(tokens *tokenRegistry) proxy.SessionAuth {
	return func(sessionID string) string {
		if tokens.Auth(sessionID) == store.ProxyAuthSubscription {
			return proxy.AuthSubscription
		}
		return proxy.AuthKey
	}
}

// usageLine renders one metered request for the log.
//
// It carries the session ID and counters only. The session token, the provider
// key and any part of the request or response body must never appear here.
func usageLine(ev proxy.UsageEvent) string {
	shape := "single"
	if ev.Stream {
		shape = "stream"
	}
	if ev.Partial {
		shape += " partial"
	}
	model := ev.Model
	if model == "" {
		model = "unknown-model"
	}
	if !ev.Usage.Reported {
		// Distinguish an unmeasured request from a free one. Rendering zeros
		// here would put a confident wrong number in front of the user.
		return fmt.Sprintf("usage session=%s %s %s %s tokens=unreported", ev.SessionID, ev.Provider, model, shape)
	}
	return fmt.Sprintf(
		"usage session=%s %s %s %s in=%d out=%d cache_read=%d cache_write=%d",
		ev.SessionID, ev.Provider, model, shape,
		ev.Usage.InputTokens, ev.Usage.OutputTokens,
		ev.Usage.CacheReadTokens, ev.Usage.CacheCreationTokens,
	)
}

func runKeychain(ctx context.Context, kc *keychain.Keychain, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: saint keychain set|rm anthropic|openai|claude-subscription")
	}
	provider := keychain.Provider(args[1])
	switch provider {
	case keychain.ProviderAnthropic, keychain.ProviderOpenAI, keychain.ProviderClaudeSubscription:
	default:
		return fmt.Errorf("unknown provider %q: expected anthropic, openai or claude-subscription", args[1])
	}
	switch args[0] {
	case "set":
		if provider == keychain.ProviderClaudeSubscription {
			// The token comes from Claude Code itself. Driving its browser flow
			// from here would be fragile for no gain: the output is a paste.
			fmt.Fprintln(os.Stderr, "Generate a token with `claude setup-token`, then paste it here.")
			fmt.Fprintln(os.Stderr, "It authenticates against your Claude subscription and lasts one year.")
		}
		fmt.Fprintf(os.Stderr, "Enter %s key: ", provider)
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		return kc.Set(ctx, provider, string(value))
	case "rm":
		return kc.Remove(ctx, provider)
	default:
		return fmt.Errorf("unknown keychain action %q", args[0])
	}
}

func runNew(ctx context.Context, deps sessionDeps, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	runtimeFlag := fs.String("rt", "", "runtime: claude or codex")
	nameFlag := fs.String("name", "", "session name")
	promptFlag := fs.String("p", "", "initial prompt")
	cpusFlag := fs.Int("cpus", 0, "CPU limit")
	memoryFlag := fs.Int("memory", 0, "memory limit in GB")
	// Skipping prompts is the default because the microVM is the boundary: an
	// agent that stops to ask before every edit inside its own sandbox is
	// mostly friction. -ask puts you back in the loop.
	askFlag := fs.Bool("ask", false, "make the agent ask before each action")
	credentialFlag := fs.String("credential", "", "credential: claude-code, claude_subscription or api-key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var runtime sandbox.Runtime
	switch strings.TrimSpace(*runtimeFlag) {
	case "", "claude":
		runtime = sandbox.Runtime(*runtimeFlag)
	case "codex":
		runtime = sandbox.RuntimeCodex
	default:
		return fmt.Errorf("unknown runtime %q", *runtimeFlag)
	}

	repos := make([]sandbox.Repo, 0, len(fs.Args()))
	for _, path := range fs.Args() {
		repos = append(repos, sandbox.Repo{Path: path})
	}

	return startAndAttach(ctx, deps, bufio.NewReader(os.Stdin), os.Stdout, session.CreateRequest{
		Name:            *nameFlag,
		Repos:           repos,
		Runtime:         runtime,
		Prompt:          *promptFlag,
		CPUs:            *cpusFlag,
		MemoryGB:        *memoryFlag,
		SkipPermissions: !*askFlag,
		Credential:      *credentialFlag,
	})
}

func execAttach(cmd *exec.Cmd, err error) error {
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runAttach(ctx context.Context, deps sessionDeps, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: saint attach <name|id>")
	}
	return reattach(ctx, deps, args[0])
}

type debugLogger struct {
	path    string
	enabled bool
	mu      sync.Mutex
}

func newDebugLogger(st *store.Store) *debugLogger {
	return &debugLogger{
		path:    st.Paths().LogPath,
		enabled: os.Getenv("SAINT_DEBUG") == "1",
	}
}

func (d *debugLogger) Printf(format string, args ...any) {
	if !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(d.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	line := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, _ = f.WriteString(line)
}

// probeProxy reports whether something is already accepting connections at addr.
func probeProxy(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
