package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// proxyRuntime is everything a caller needs to run the local proxy and stop it
// again. Stopping it correctly needs more than the HTTP servers: hijacked
// tunnels, the control socket and the journal are not theirs to close.
type proxyRuntime struct {
	Proxy     *proxy.Proxy
	Listeners *listenerSet
	Control   net.Listener
	Journal   *journal
	Prompter  *egressPrompter
	// ListenSession makes the proxy listen on a session's network.
	ListenSession func(ctx context.Context, sessionID string) (string, error)
}

// newProxyRuntime builds the proxy. There is exactly one construction path and
// one set of timeouts, whoever starts it.
func newProxyRuntime(st *store.Store, kc *keychain.Keychain, containerPath string, logger *debugLogger) (*proxyRuntime, error) {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return nil, err
	}
	anthropicURL, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		return nil, err
	}
	openAIURL, err := url.Parse("https://api.openai.com")
	if err != nil {
		return nil, err
	}

	anthropicSource, err := credentialSource(cfg, store.CredentialAnthropic, string(keychain.ProviderAnthropic), kc)
	if err != nil {
		return nil, fmt.Errorf("anthropic credential: %w", err)
	}
	openAISource, err := credentialSource(cfg, store.CredentialOpenAI, string(keychain.ProviderOpenAI), kc)
	if err != nil {
		return nil, fmt.Errorf("openai credential: %w", err)
	}

	jr := newJournal(
		st.Paths().ProxyLogPath,
		cfg.ProxyLogMaxBytesOrDefault(),
		cfg.ProxyLogKeepFilesOrDefault(),
	)

	tokens := newTokenRegistry(st)
	tokens.legacyNetwork = cfg.ProxyNetworkOrDefault()
	logf := func(format string, args ...any) {
		// The proxy's own decisions go to its output (proxy.out) as well as
		// the debug log, because a refused host has to be findable without
		// turning debugging on.
		fmt.Fprintf(os.Stdout, time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
		logger.Printf(format, args...)
	}

	prompter := newEgressPrompter(st, logf)
	policy := newLivePolicy(st)

	handler, err := proxy.New(proxy.Config{
		AnthropicURL: anthropicURL,
		OpenAIURL:    openAIURL,
		AnthropicKey: anthropicSource.Get,
		OpenAIKey:    openAISource.Get,
		TokenLookup:  tokens.Lookup,
		SessionAuth:  proxyAuthMode(tokens),
		Logf:         logger.Printf,
		Forward:      sessionForward(prompter, policy, jr),

		OnUsage: func(ev proxy.UsageEvent) {
			// This runs on the goroutine copying an agent's response. record
			// queues and returns; it never touches the disk here.
			jr.record(usageRecord(ev, time.Now()))
			logger.Printf("%s", usageLine(ev))
		},
	})
	if err != nil {
		jr.Close()
		return nil, err
	}

	// Only the request records cost anything on the request path, so the
	// wrapper exists only when they are wanted.
	var served http.Handler = handler
	if cfg.ProxyLogRequestsOrDefault() {
		served = newJournalHandler(handler, jr, tokens)
	}

	listeners := newListenerSet(cfg.ProxyPort, served, tokens.Network, logf)
	rt := &proxyRuntime{
		Proxy:     handler,
		Listeners: listeners,
		Journal:   jr,
		Prompter:  prompter,
	}
	rt.ListenSession = func(ctx context.Context, sessionID string) (string, error) {
		// A pane asks right after creating the session, so the cached records
		// may not have it yet.
		tokens.invalidate()
		return listenSession(ctx, st, containerPath, listeners, sessionID)
	}
	return rt, nil
}

// sessionForward is the forward-proxy configuration for sessions: on,
// enforced, and authenticated.
//
// A session on its own internal network has no other way out, so the policy
// here is the boundary. Anything neither on the shipped allowlist nor saved is
// put to the person at the session's herdr pane, and refused if nobody
// answers. Anonymous forwarding stays off: the token in the guest's proxy URL
// is how a request is tied to a session, and to the right pane to ask in.
// Loopback and link-local targets stay refused, because tunnelling there would
// hand a guest every unauthenticated service the developer runs locally, plus
// the 169.254.169.254 metadata address.
func sessionForward(prompter *egressPrompter, policy *livePolicy, jr *journal) *proxy.ForwardConfig {
	return &proxy.ForwardConfig{
		Enforce:    true,
		Ask:        prompter.Ask,
		LivePolicy: policy.get,
		OnDecision: func(ev proxy.DecisionEvent) {
			jr.record(egressRecord(ev, time.Now()))
		},
	}
}

// listenSession makes the proxy listen on a session's network, then tells the
// session's guest it may start its agent.
func listenSession(ctx context.Context, st *store.Store, containerPath string, listeners *listenerSet, sessionID string) (string, error) {
	if !store.ValidSessionID(sessionID) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	sessions, err := st.LoadSessions()
	if err != nil {
		return "", err
	}
	var session *store.Session
	for i := range sessions {
		if sessions[i].ID == sessionID {
			session = &sessions[i]
		}
	}
	if session == nil {
		return "", fmt.Errorf("no session %q", sessionID)
	}

	network := session.Network
	var host string
	if network == "" {
		// A session from before per-session networks was given the shared
		// network's gateway, or the address proxy_bind names.
		cfg, err := st.EnsureConfig()
		if err != nil {
			return "", err
		}
		network = cfg.ProxyNetworkOrDefault()
		if host, err = resolveProxyBindHost(ctx, cfg, containerPath); err != nil {
			return "", err
		}
	} else if host, err = sandbox.GatewayAddr(ctx, containerPath, network); err != nil {
		return "", err
	}

	addr, err := listeners.ensure(ctx, network, host)
	if err != nil {
		return "", err
	}
	ready := filepath.Join(st.Paths().SessionsDir, sessionID, sandbox.ProxyReadyFile)
	if err := os.WriteFile(ready, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("signal the guest: %w", err)
	}
	return addr, nil
}

// listenRunningSessions binds the network of every saint session whose
// container is running, so a proxy that restarts serves the sessions that
// outlived the last one without waiting for a pane to ask.
func listenRunningSessions(ctx context.Context, st *store.Store, sb sandbox.Sandbox, listen func(context.Context, string) (string, error), logf func(string, ...any)) {
	infos, err := sb.List(ctx)
	if err != nil {
		logf("proxy: list containers: %v", err)
		return
	}
	running := map[string]bool{}
	for _, info := range infos {
		if info.State == sandbox.StateRunning {
			running[info.Name] = true
		}
	}
	sessions, err := st.LoadSessions()
	if err != nil {
		logf("proxy: load sessions: %v", err)
		return
	}
	for _, session := range sessions {
		if !running["saint-"+session.ID] {
			continue
		}
		if _, err := listen(ctx, session.ID); err != nil {
			logf("proxy: session %s: %v", session.ID, err)
		}
	}
}

// Shutdown stops the proxy in the order the parts actually depend on.
//
// http.Server.Shutdown does not touch hijacked connections, which is every
// forward-proxy tunnel - Go's documentation says so directly. Without the
// second step a tunnel and its goroutine outlive the server that made them, and
// saint appears to have exited while a guest still has an open socket through
// it. The journal closes last so records queued by requests that were still
// finishing are written rather than dropped.
func (r *proxyRuntime) Shutdown(ctx context.Context) error {
	var errs []error
	if r.Control != nil {
		errs = append(errs, r.Control.Close())
	}
	if r.Listeners != nil {
		errs = append(errs, r.Listeners.shutdown(ctx))
	}
	errs = append(errs, r.Proxy.CloseTunnels())
	if r.Journal != nil {
		r.Journal.Close()
	}
	return errors.Join(errs...)
}

func runProxy(ctx context.Context, st *store.Store, kc *keychain.Keychain, containerPath string, logger *debugLogger) error {
	// Ctrl-C previously severed in-flight SSE streams; shut down gracefully.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := newProxyRuntime(st, kc, containerPath, logger)
	if err != nil {
		return err
	}

	control, err := listenControl(st.Paths().ControlSocket)
	if err != nil {
		rt.Journal.Close()
		return err
	}
	rt.Control = control
	go serveControl(control, controlHandlers{
		listeners: rt.Listeners.addrs,
		listen:    rt.ListenSession,
		egress:    rt.Prompter.servePrompt,
	}, rt.Prompter.logf)
	fmt.Println("control socket", st.Paths().ControlSocket)

	listenRunningSessions(ctx, st, sandbox.NewAppleContainer(containerPath, logger.Printf), rt.ListenSession, rt.Prompter.logf)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return rt.Shutdown(shutdownCtx)
}

// livePolicy is the saved allowlist as the proxy applies it: re-read when
// config.json changes, so a host saved by an answer or by `saint allow` applies
// to every session from the next request on, without a restart.
type livePolicy struct {
	store *store.Store

	mu      sync.Mutex
	modTime time.Time
	size    int64
	policy  *proxy.HostPolicy
}

func newLivePolicy(st *store.Store) *livePolicy {
	return &livePolicy{store: st}
}

// get returns the policy in force. A stat per forward-proxied request is the
// cost; a config that cannot be read keeps the last good policy rather than
// dropping every saved host.
func (l *livePolicy) get() *proxy.HostPolicy {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.store.Paths().ConfigPath)
	if err != nil {
		return l.policy
	}
	if l.policy != nil && info.ModTime().Equal(l.modTime) && info.Size() == l.size {
		return l.policy
	}
	cfg, err := l.store.LoadConfig()
	if err != nil {
		return l.policy
	}
	l.policy = &proxy.HostPolicy{
		Allow: append([]string(nil), cfg.ProxyAllowHosts...),
		Deny:  append([]string(nil), cfg.ProxyDenyHosts...),
	}
	l.modTime = info.ModTime()
	l.size = info.Size()
	return l.policy
}
