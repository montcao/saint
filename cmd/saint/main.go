package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/doctor"
	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/tui"
	"github.com/montcao/saint/internal/usage"
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
	st := store.New(home)
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
	inspector := tui.NewRepoInspector(gitPath, logger.Printf)
	// Sessions created before tokens moved out of the mounted session directory
	// still keep theirs in the old place, where the proxy no longer looks. A
	// failure here is not fatal - it costs those sessions API access, not the
	// whole TUI - but it must be visible.
	if err := st.MigrateProxyTokens(); err != nil {
		logger.Printf("proxy token migration failed: %v", err)
	}

	// One meter is shared between the proxy that observes the traffic and the
	// TUI that renders it. Totals live only as long as this process, which is
	// also as long as the proxy that produces them.
	meter := usage.NewMeter()
	// The loader gets the meter only once a proxy in this process is actually
	// serving, below. A meter nothing feeds reads zero for everything, and a
	// zero that is rendered as a measurement - every session "idle", every
	// total nil - is a wrong answer rather than a gap.
	loader := &tui.SnapshotLoader{Store: st, Sandbox: sb, Inspector: inspector}
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

	// Point new sessions at the proxy. Without this the whole proxy is inert:
	// applecontainer.Create gates every ANTHROPIC_BASE_URL / ANTHROPIC_API_KEY
	// env var on a non-empty proxy URL, so an unset ProxyURL means containers
	// talk to api.anthropic.com directly with whatever credentials they find.
	guestURL, proxyErr := guestProxyURL(ctx, st, containerPath)
	svc.ProxyURL = guestURL

	if len(args) == 0 {
		// The proxy runs inside the TUI process so `saint` works with no second
		// terminal. It stops when the TUI does: containers survive, but their
		// agents lose API access until saint is relaunched, which is why the
		// header carries the proxy state at all times rather than failing quietly.
		status, serving, shutdown := startEmbeddedProxy(ctx, st, kc, containerPath, logger, meter, proxyErr)
		defer shutdown()
		if serving {
			loader.Usage = meter
		}

		model := tui.NewModel(loader, svc).
			WithProxyStatus(status).
			WithCredentials(newTUICredentials(st, kc))
		_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
		return err
	}

	if args[0] == "--list" {
		return runList(ctx, loader)
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
		return runNew(ctx, svc, args[1:])
	case "attach":
		return runAttach(ctx, svc, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runList(ctx context.Context, loader *tui.SnapshotLoader) error {
	snapshot, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	fmt.Print(tui.FormatPlainTable(snapshot))
	return nil
}

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

// newProxyServer builds the proxy listener. Both `saint proxy` and the TUI go
// through here so there is exactly one construction path and one set of
// timeouts.
//
// The returned journal belongs to the caller: it owns a goroutine and an open
// file, and must be closed when the server stops.
// proxyRuntime is everything a caller needs to run the local proxy and stop it
// again. It is a struct rather than four return values because stopping the
// proxy correctly needs more than the http.Server: hijacked tunnels and the
// journal are not the server's to close.
type proxyRuntime struct {
	Server  *http.Server
	Addr    string
	Journal *journal
	Proxy   *proxy.Proxy
}

func newProxyServer(ctx context.Context, st *store.Store, kc *keychain.Keychain, containerPath string, logger *debugLogger, meter *usage.Meter) (proxyRuntime, error) {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return proxyRuntime{}, err
	}
	anthropicURL, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		return proxyRuntime{}, err
	}
	openAIURL, err := url.Parse("https://api.openai.com")
	if err != nil {
		return proxyRuntime{}, err
	}

	anthropicSource, err := credentialSource(cfg, store.CredentialAnthropic, string(keychain.ProviderAnthropic), kc)
	if err != nil {
		return proxyRuntime{}, fmt.Errorf("anthropic credential: %w", err)
	}
	openAISource, err := credentialSource(cfg, store.CredentialOpenAI, string(keychain.ProviderOpenAI), kc)
	if err != nil {
		return proxyRuntime{}, fmt.Errorf("openai credential: %w", err)
	}

	jr := newJournal(
		st.Paths().ProxyLogPath,
		cfg.ProxyLogMaxBytesOrDefault(),
		cfg.ProxyLogKeepFilesOrDefault(),
	)

	tokens := newTokenRegistry(st)
	handler, err := proxy.New(proxy.Config{
		AnthropicURL: anthropicURL,
		OpenAIURL:    openAIURL,
		AnthropicKey: anthropicSource.Get,
		OpenAIKey:    openAISource.Get,
		TokenLookup:  tokens.Lookup,
		SessionAuth:  proxyAuthMode(tokens),
		Logf:         logger.Printf,
		// Forwarding and the guest's proxy variables are the same switch. A
		// guest pointed at a proxy that will not forward gets 405 on every git
		// clone and npm install; a proxy that forwards for a guest that was
		// never told about it does nothing at all. Neither half is useful
		// alone, so neither has its own setting.
		Forward: guestEgressForward(cfg),

		// What makes a session's activity visible. Begin is nil-safe, so
		// `saint proxy` - which has no TUI and passes no meter - is unaffected.
		OnRequestStart: meter.Begin,

		OnUsage: func(ev proxy.UsageEvent) {
			meter.Record(ev)
			// This runs on the goroutine copying an agent's response. record
			// queues and returns; it never touches the disk here.
			jr.record(usageRecord(ev, time.Now()))
			logger.Printf("%s", usageLine(ev))
		},
	})
	if err != nil {
		jr.Close()
		return proxyRuntime{}, err
	}

	bindHost, err := resolveProxyBindHost(ctx, cfg, containerPath)
	if err != nil {
		jr.Close()
		return proxyRuntime{}, err
	}

	// Only the request records cost anything on the request path, so the
	// wrapper exists only when they are wanted.
	var served http.Handler = handler
	if cfg.ProxyLogRequestsOrDefault() {
		served = newJournalHandler(handler, jr, tokens)
	}

	addr := net.JoinHostPort(bindHost, strconv.Itoa(cfg.ProxyPortOrDefault()))
	server := &http.Server{
		Addr:    addr,
		Handler: served,

		// ReadHeaderTimeout bounds Slowloris: a client cannot hold a connection
		// open indefinitely by dribbling out request headers.
		ReadHeaderTimeout: 10 * time.Second,

		// WriteTimeout is deliberately left at 0. Claude Code streams SSE
		// responses that can run for many minutes; any write deadline would
		// truncate a long response mid-stream. Do not "fix" this by setting one.
		WriteTimeout: 0,
	}
	return proxyRuntime{Server: server, Addr: addr, Journal: jr, Proxy: handler}, nil
}

// Shutdown stops the proxy in the order the parts actually depend on.
//
// http.Server.Shutdown does not touch hijacked connections, which is every
// forward-proxy tunnel - Go's documentation says so directly. Without the
// second step a tunnel and its goroutine outlive the server that made them, and
// saint appears to have exited while a guest still has an open socket through
// it. The journal closes last so records queued by requests that were still
// finishing are written rather than dropped.
func (r proxyRuntime) Shutdown(ctx context.Context) error {
	var errs []error
	if r.Server != nil {
		errs = append(errs, r.Server.Shutdown(ctx))
	}
	errs = append(errs, r.Proxy.CloseTunnels())
	if r.Journal != nil {
		r.Journal.Close()
	}
	return errors.Join(errs...)
}

// guestEgressForward returns the forward-proxy configuration, or nil when guest
// egress proxying is off - which is the default, and which leaves the proxy
// answering nothing but the two provider APIs.
//
// The defaults here are the restrictive ones on purpose. Anonymous forwarding
// is refused because this listener binds the container bridge, so every
// container on that bridge can reach it, not only the ones saint started; a
// guest identifies itself with the session token it already has. Loopback and
// link-local stay denied because a container cannot reach the host's 127.0.0.1
// on its own, and tunnelling there would hand a guest every unauthenticated
// service the developer runs locally, plus the 169.254.169.254 metadata
// address.
func guestEgressForward(cfg store.Config) *proxy.ForwardConfig {
	if !cfg.ProxyGuestEgressOrDefault() {
		return nil
	}
	return &proxy.ForwardConfig{}
}

func runProxy(ctx context.Context, st *store.Store, kc *keychain.Keychain, containerPath string, logger *debugLogger) error {
	// Ctrl-C previously severed in-flight SSE streams; shut down gracefully.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Standalone `saint proxy` has no TUI to render totals, so it meters to the
	// log only. Record is nil-safe.
	rt, err := newProxyServer(ctx, st, kc, containerPath, logger, nil)
	if err != nil {
		return err
	}
	defer rt.Journal.Close()

	errc := make(chan error, 1)
	go func() {
		fmt.Println("listening on", rt.Addr)
		errc <- rt.Server.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return rt.Shutdown(shutdownCtx)
	}
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

func runNew(ctx context.Context, svc *session.Service, args []string) error {
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

	created, err := svc.Create(ctx, session.CreateRequest{
		Name:            *nameFlag,
		Repos:           repos,
		Runtime:         runtime,
		Prompt:          *promptFlag,
		CPUs:            *cpusFlag,
		MemoryGB:        *memoryFlag,
		SkipPermissions: !*askFlag,
		Credential:      *credentialFlag,
	})
	if err != nil {
		return err
	}

	return execAttach(svc.AttachCmd(created.ID))
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

func runAttach(ctx context.Context, svc *session.Service, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: saint attach <name|id>")
	}

	found, err := svc.Find(args[0])
	if err != nil {
		return err
	}

	if err := svc.Start(ctx, found.ID); err != nil && !strings.Contains(err.Error(), "already running") {
		// `container start` is only needed for stopped sessions; a running
		// container will be attached below.
	}

	return execAttach(svc.AttachCmd(found.ID))
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

// guestProxyURL is the address a *guest* uses to reach the proxy. It always uses
// the discovered bridge gateway, never the bind host: a user may legitimately
// pin ProxyBind to 0.0.0.0 or a specific interface, and neither of those is a
// routable destination from inside a container.
func guestProxyURL(ctx context.Context, st *store.Store, containerPath string) (string, error) {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return "", err
	}
	gateway, err := sandbox.GatewayAddr(ctx, containerPath, cfg.ProxyNetworkOrDefault())
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(gateway, strconv.Itoa(cfg.ProxyPortOrDefault())), nil
}

// startEmbeddedProxy runs the proxy alongside the TUI and returns a one-line
// status for the header plus a shutdown func.
//
// A failure here is deliberately not fatal: saint remains usable for inspecting
// and attaching to sessions. It must be loud, though - silently falling back to
// "the agent uses whatever credentials it can find" is the one outcome that
// defeats the point of the proxy.
// The second result reports whether the proxy in *this* process is the one
// carrying the sessions' traffic. It is what makes the meter's numbers a
// measurement rather than a shape: an external proxy, a failed bind, or a
// listener the firewall is dropping all leave the meter reading zero forever,
// and a caller that rendered those as totals and idle sessions would be
// inventing both.
func startEmbeddedProxy(ctx context.Context, st *store.Store, kc *keychain.Keychain, containerPath string, logger *debugLogger, meter *usage.Meter, discoverErr error) (string, bool, func()) {
	noop := func() {}
	if discoverErr != nil {
		return "proxy off: " + discoverErr.Error(), false, noop
	}

	rt, err := newProxyServer(ctx, st, kc, containerPath, logger, meter)
	if err != nil {
		return "proxy off: " + err.Error(), false, noop
	}

	// Another saint (or a standalone `saint proxy`) may already hold the port.
	// Reuse it rather than fighting over the bind and killing the TUI.
	listener, err := net.Listen("tcp", rt.Addr)
	if err != nil {
		rt.Journal.Close()
		if probeProxy(rt.Addr) {
			return "proxy: external " + rt.Addr, false, noop
		}
		return "proxy off: " + err.Error(), false, noop
	}

	go func() {
		if err := rt.Server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("proxy stopped: %v", err)
		}
	}()

	status := "proxy: " + rt.Addr + " · " + describeAuthMode(st, kc, logger)

	// Binding succeeds even when the macOS application firewall will drop every
	// connection to a non-loopback interface, so a successful Listen proves
	// nothing. Self-probe: if we cannot reach our own listener, no guest can
	// either, and that has to be visible rather than looking healthy.
	serving := true
	if !probeProxy(rt.Addr) {
		status = "proxy blocked: " + rt.Addr + " (firewall? run saint doctor)"
		logger.Printf("proxy bound %s but is not reachable; check the macOS application firewall", rt.Addr)
		// Bound but unreachable means no guest request will ever arrive, so
		// nothing this proxy could report about a session is measured.
		serving = false
	}

	return status, serving, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = rt.Shutdown(shutdownCtx)
	}
}

// describeAuthMode reports which account new sessions will bill.
//
// This is on screen at all times rather than in a config file because the two
// modes charge different accounts, and "auto" resolving one way or the other is
// exactly the thing a user should not have to guess at.
func describeAuthMode(st *store.Store, kc *keychain.Keychain, logger *debugLogger) string {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return "auth unknown"
	}
	switch cfg.ProxyAuthOrDefault() {
	case store.ProxyAuthKey:
		return "api key"
	case store.ProxyAuthSubscription:
		if subscriptionAvailable(cfg, kc, logger) {
			return "subscription"
		}
		return "api key (no subscription token)"
	default:
		if subscriptionAvailable(cfg, kc, logger) {
			return "subscription"
		}
		return "api key"
	}
}

func subscriptionAvailable(cfg store.Config, kc *keychain.Keychain, logger *debugLogger) bool {
	source, err := credentialSource(cfg, store.CredentialClaudeSubscription, string(keychain.ProviderClaudeSubscription), kc)
	if err != nil {
		logger.Printf("subscription credential source unusable: %v", err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), credential.Timeout)
	defer cancel()

	// Ask without taking the secret where the source can answer that way.
	if inspector, ok := source.(credential.Inspector); ok {
		status, err := inspector.Status(ctx)
		return err == nil && status.Present && !status.Expired(time.Now())
	}
	value, err := source.Get(ctx)
	return err == nil && value != ""
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
