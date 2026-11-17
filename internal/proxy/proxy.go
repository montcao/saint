package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type KeySource func(context.Context) (string, error)

// Auth modes. They mirror store.ProxyAuthKey and store.ProxyAuthSubscription;
// this package does not import the store, so the strings are repeated here and
// the mapping is one line at the call site.
const (
	// AuthKey has the proxy hold the real provider key. The guest presents a
	// per-session token, which is stripped and replaced on the way out.
	AuthKey = "key"

	// AuthSubscription has the guest hold its own Claude subscription
	// credential, which this proxy forwards untouched.
	//
	// This is not a preference. A claude.ai subscription stays the active
	// credential only while Claude Code holds it and the gateway forwards it
	// verbatim; a gateway that injects its own credential replaces the
	// subscription and the traffic is billed per token instead. So in this mode
	// the proxy is an observer, not a credential boundary - it authenticates and
	// meters, but the secret it would otherwise protect is in the guest.
	AuthSubscription = "subscription"
)

// SaintSessionHeader carries the per-session token in subscription mode.
//
// The usual credential headers are occupied by the guest's own subscription
// token in that mode, so saint needs a channel of its own. Claude Code puts
// whatever ANTHROPIC_CUSTOM_HEADERS names onto every request, which is how this
// header gets there. It is stripped before forwarding: it is saint's, and no
// provider should ever see it.
const SaintSessionHeader = "x-saint-session"

// SessionAuth reports the auth mode recorded for a session.
//
// Mode comes from the session record rather than from the request, so a guest
// cannot talk the proxy into injecting a real key by changing which header it
// sends.
type SessionAuth func(sessionID string) string

// TokenLookup resolves an inbound per-session proxy token to the session that
// owns it. Implementations must compare in constant time against every
// candidate rather than doing a map lookup keyed by the raw secret.
type TokenLookup func(token string) (sessionID string, ok bool)

type Config struct {
	AnthropicURL *url.URL
	OpenAIURL    *url.URL
	AnthropicKey KeySource
	OpenAIKey    KeySource

	// TokenLookup authenticates callers. It is required: without it the proxy
	// would re-sign every request it received with the user's real provider
	// key, which is a key-minting oracle for anything that can reach the port.
	TokenLookup TokenLookup

	// Transport overrides the outbound RoundTripper. Production leaves this nil;
	// tests use it to point the pinned upstream hosts at a local test server
	// without weakening the host allowlist.
	Transport http.RoundTripper

	// SessionAuth decides, per session, whether this proxy injects a credential
	// or forwards the guest's own. Nil means every session is AuthKey, which is
	// the behaviour that predates subscription support.
	SessionAuth SessionAuth

	Logf func(string, ...any)

	// OnUsage receives per-request token accounting when the upstream reports it.
	//
	// It is pure observation. Leaving it nil is the default and makes this proxy
	// behave exactly as it did before metering existed; setting it must never
	// change whether a request succeeds. It is called from the response-copy
	// goroutine, so different requests can invoke it concurrently and the
	// implementation must be safe for that.
	OnUsage func(UsageEvent)

	// OnRequestStart is called when an attributed request begins crossing the
	// proxy and returns a function called exactly once when it finishes.
	//
	// It exists because "is this session working or idle" has no honest answer
	// anywhere else. saint does not run the agents it manages and cannot see
	// inside them, so the only thing it truly knows is whether a request from
	// that session is crossing this proxy right now. Anything richer would be a
	// guess wearing the clothes of a measurement.
	//
	// Nil is the default and costs nothing. The hook fires only once a request
	// has been attributed to a session, never for an unauthenticated one: saint
	// does not know who that was, and charging it to somebody would be an
	// invented answer about a real person's session.
	//
	// The forward-proxy path deliberately does not call it. A CONNECT tunnel
	// stays open for its whole lifetime - an agent that opened one to a git host
	// at the start of a task still holds it while it sits waiting for its user -
	// so counting tunnels would pin that session at "working" until it exits.
	// That is a false signal and a permanent one, which is worse than the gap it
	// would fill. Absolute-form plain HTTP on that path is short-lived and would
	// be honest, but it is the small half of forward traffic, and a count that
	// means "thinking" for one shape of request and "connected" for another is
	// a number nobody can read.
	//
	// It is called from the goroutine serving the request, so several sessions
	// can be inside it at once and the implementation must be safe for that.
	OnRequestStart func(sessionID string) (done func())

	// MaxRequestBytes caps a single request body. Zero takes
	// DefaultMaxRequestBytes; UnlimitedRequestBytes removes the cap.
	//
	// Unlike the ceiling below, this one is on by default. An uncapped proxy on
	// the bridge gateway is an unbounded upload channel onto the user's provider
	// account for anything that holds a session token, and the default is set
	// far enough above real agent traffic that no legitimate turn meets it.
	MaxRequestBytes int64

	// RequestsPerMinute caps how many upstream calls one session may make.
	//
	// Zero, the default, is unlimited. A ceiling low enough to be worth having
	// is workload-specific - a long agentic run bursts hard and legitimately -
	// and one guessed here would arrive as agents failing mid-task on an
	// upgrade. The user turns it on knowing their own workload.
	RequestsPerMinute int

	// Forward turns on the forward-proxy path in forward.go, which is a
	// separate role from everything above: it carries no credential, routes to
	// no pinned upstream, and exists so a guest that honours HTTP_PROXY reveals
	// which hosts it contacts. Nil, the default, leaves it off.
	Forward *ForwardConfig
}

type Proxy struct {
	cfg Config

	// limiter is nil when rate limiting is off, which is both the default and
	// the cheapest path through allow().
	limiter *rateLimiter

	// forward is nil when forward proxying is off, which is the default and is
	// what makes a CONNECT a refusal rather than a tunnel.
	forward *forwarder
}

// allowedUpstreamHosts is the complete set of hosts this proxy will forward to.
//
// Loopback is deliberately absent: allowlisting 127.0.0.1/localhost would turn
// the proxy into an SSRF pivot into host-only services the moment upstreams
// became configurable. The check runs here, at construction, rather than
// per-request - where it compared a hardcoded constant against itself and could
// never fail.
var allowedUpstreamHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"api.openai.com":    {},
}

func New(cfg Config) (*Proxy, error) {
	if cfg.AnthropicURL == nil || cfg.OpenAIURL == nil {
		return nil, errors.New("both upstream URLs are required")
	}
	if cfg.TokenLookup == nil {
		return nil, errors.New("token lookup is required")
	}
	if cfg.AnthropicKey == nil || cfg.OpenAIKey == nil {
		return nil, errors.New("both key sources are required")
	}
	for _, upstream := range []*url.URL{cfg.AnthropicURL, cfg.OpenAIURL} {
		if err := checkUpstream(upstream); err != nil {
			return nil, fmt.Errorf("upstream %s: %w", upstream.Redacted(), err)
		}
	}
	p := &Proxy{cfg: cfg}
	if cfg.RequestsPerMinute > 0 {
		p.limiter = newRateLimiter(cfg.RequestsPerMinute, time.Now)
	}
	if cfg.Forward != nil {
		p.forward = newForwarder(*cfg.Forward)
	}
	return p, nil
}

func checkUpstream(upstream *url.URL) error {
	if upstream.Scheme != "https" {
		return fmt.Errorf("scheme %q is not https", upstream.Scheme)
	}
	if _, ok := allowedUpstreamHosts[strings.ToLower(upstream.Hostname())]; !ok {
		return errors.New("host is not allowlisted")
	}
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A forward-proxied request is a different shape and takes a different
	// path: a CONNECT, or a request line carrying an absolute URL. Neither can
	// be confused with the reverse-proxy path below, which sees origin-form
	// request lines - a path and no authority - because that is what a client
	// pointed at saint's base URL sends. Everything from here down is unchanged
	// by forward proxying existing.
	//
	// The exception is a request whose absolute URL names this proxy itself.
	// That happens for real: a guest given the same address as its provider
	// base URL and as HTTP_PROXY sends its model API calls in absolute form,
	// and HTTP has always said a server receiving its own authority should
	// treat the request as an ordinary one. Forwarding it instead would have
	// saint dial itself.
	if isForwardRequest(r) && !isSelfAddressed(r) {
		p.serveForward(w, r)
		return
	}

	// Authenticate first. Nothing about an unauthenticated request is this
	// proxy's business - not its path, not whether that path is routable, and
	// not a log line describing it.
	sessionID, ok := p.authenticate(r.Header)
	if ok && !inScope(r.Context(), sessionID) {
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s refused: token presented on another session's network", sessionID)
		}
		ok = false
	}
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// From here the request belongs to a session, so it can be counted as one
	// in flight. The end is deferred at this single point rather than spelled
	// out at each of the exits below - an unrouted path, a refused limit, a
	// keychain that would not open, a panic further down - because a decrement
	// that does not run leaves that session reading "working" for the rest of
	// saint's life, and nothing later in the process corrects it.
	defer p.beginRequest(sessionID)()

	// The session's mode decides how generously an unknown path is treated, so
	// it has to be known before routing.
	mode := p.authMode(sessionID)

	provider, upstream, keyFn, err := p.route(r.URL.Path, r.Header, mode)
	if err != nil {
		// Log unmatched paths unconditionally. A route gap presents inside the
		// guest as an unexplained hard failure, and a 404 nobody can see is the
		// worst failure mode this proxy has.
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s unrouted %s %s (no upstream; extend route() if the agent needs it)", sessionID, r.Method, r.URL.Path)
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Limits come next, before the credential is fetched: a request that is
	// going to be refused should never reach the keychain, let alone the key.
	limited, ok := p.enforceLimits(w, r, sessionID)
	if !ok {
		return
	}

	var key string
	if mode == AuthKey {
		key, err = keyFn(r.Context())
		if err != nil {
			// The caller gets a generic message; the cause goes to the log so a
			// locked keychain is diagnosable instead of a silent 502.
			if p.cfg.Logf != nil {
				p.cfg.Logf("session=%s key lookup failed for %s: %v", sessionID, provider, err)
			}
			http.Error(w, "missing key", http.StatusBadGateway)
			return
		}
	}
	if p.cfg.Logf != nil {
		// Never log the session token or the upstream key.
		p.cfg.Logf("session=%s %s %s -> %s (%s)", sessionID, r.Method, r.URL.Path, provider, mode)
	}

	target := *upstream
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host

		// saint's own header never leaves the host, in either mode.
		req.Header.Del(SaintSessionHeader)

		if mode == AuthSubscription {
			// Forward the guest's credential and every anthropic-* header
			// exactly as they arrived. anthropic-beta carries the OAuth
			// capability the upstream requires for a subscription credential,
			// and stripping it fails the request with a 401 that looks like a
			// saint bug rather than a missing header.
			return
		}

		stripAuthHeaders(req.Header)
		switch provider {
		case "anthropic":
			req.Header.Set("x-api-key", key)
		case "openai":
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}

	transport := p.cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Director = director
	reverse.Transport = transport
	reverse.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
		if limited.tooLarge() {
			// The body passed the cap in flight, so this proxy aborted the
			// upstream request rather than the upstream failing. Say which:
			// a guest told 502 retries the same oversized request forever.
			if p.cfg.Logf != nil {
				p.cfg.Logf("session=%s refused %s %s: body exceeded the %d byte limit mid-request", sessionID, r.Method, r.URL.Path, p.maxRequestBytes())
			}
			http.Error(rw, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Do not hand the raw transport error to the guest: it can carry
		// upstream hostnames, resolver detail and local network topology. Keep
		// the detail on the host side.
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s upstream %s error: %v", sessionID, provider, err)
		}
		http.Error(rw, "upstream request failed", http.StatusBadGateway)
	}
	if p.cfg.OnUsage != nil {
		reverse.ModifyResponse = p.meterResponse(sessionID, provider)
	}
	reverse.ServeHTTP(w, r)
}

// meterResponse returns a ModifyResponse hook that observes token usage on the
// way past.
//
// It never rewrites headers and never returns an error. Both matter: the
// reverse proxy decides whether to flush immediately from the response
// Content-Type, so touching headers could turn a live SSE stream into a
// buffered one, and a non-nil error here would divert a perfectly good response
// into ErrorHandler and hand the guest a 502 because accounting failed.
func (p *Proxy) meterResponse(sessionID, provider string) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp == nil || resp.Body == nil {
			return nil
		}

		// Where this request actually went, taken from the response's own
		// request rather than from anything remembered: that is the URL the
		// transport used, after any rewriting, so it cannot disagree with what
		// left the machine. The query is dropped - a path names a capability,
		// a query can carry content.
		endpoint := ""
		if resp.Request != nil && resp.Request.URL != nil {
			endpoint = resp.Request.URL.Host + resp.Request.URL.Path
		}
		scan := newBodyScanner(resp.Header.Get("Content-Type"))
		if scan == nil {
			return nil
		}
		_, stream := scan.(*sseScanner)
		resp.Body = &meteredBody{
			src:      resp.Body,
			scan:     scan,
			encoding: meterEncoding(resp.Header.Get("Content-Encoding")),
			emit: func(res scanResult) {
				p.emitUsage(UsageEvent{
					SessionID: sessionID,
					Provider:  provider,
					Model:     res.model,
					Usage:     res.usage,
					Stream:    stream,
					Partial:   !res.complete,
					Endpoint:  endpoint,
				})
			},
		}
		return nil
	}
}

// beginRequest tells the host that an attributed request is now crossing the
// proxy, and returns the function that says it has finished.
//
// The result is never nil, so the call site can be spelled
// `defer p.beginRequest(id)()` with nothing to check: an unset hook, or one
// that hands back a nil done, still yields a no-op. It is idempotent too,
// because the cost of a double call is a counter that drifts below the truth
// and starts reporting busy sessions as idle.
//
// Both halves are panic-contained the way emitUsage is. The hook belongs to
// another package and runs on the goroutine serving a guest's request, so a
// panic there must not kill that request - and the recover sits inside the
// returned function as well as around the hook because a panic must not be
// allowed to swallow the decrement either.
func (p *Proxy) beginRequest(sessionID string) func() {
	if p.cfg.OnRequestStart == nil {
		return func() {}
	}

	var done func()
	func() {
		defer func() {
			if r := recover(); r != nil {
				if p.cfg.Logf != nil {
					p.cfg.Logf("session=%s request-start callback panicked: %v", sessionID, r)
				}
				// A hook that panicked never returned a done, and calling
				// whatever it half-assigned would be worse than counting
				// nothing at all.
				done = nil
			}
		}()
		done = p.cfg.OnRequestStart(sessionID)
	}()
	if done == nil {
		return func() {}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			defer func() {
				if r := recover(); r != nil && p.cfg.Logf != nil {
					p.cfg.Logf("session=%s request-end callback panicked: %v", sessionID, r)
				}
			}()
			done()
		})
	}
}

// emitUsage delivers an event to the host callback.
//
// The callback belongs to another package and runs on the goroutine copying an
// agent's response. A panic there would otherwise kill an in-flight request, so
// it is contained here and reported rather than propagated.
func (p *Proxy) emitUsage(ev UsageEvent) {
	defer func() {
		if r := recover(); r != nil {
			if p.cfg.Logf != nil {
				p.cfg.Logf("session=%s usage callback panicked: %v", ev.SessionID, r)
			}
		}
	}()
	p.cfg.OnUsage(ev)
}

// authMode reports how this session's traffic should be handled.
func (p *Proxy) authMode(sessionID string) string {
	if p.cfg.SessionAuth == nil {
		return AuthKey
	}
	if mode := p.cfg.SessionAuth(sessionID); mode == AuthSubscription {
		return AuthSubscription
	}
	return AuthKey
}

// authenticate resolves the inbound credential to a session. Callers may send
// the token either as an Anthropic-style x-api-key or an OpenAI-style bearer,
// so both are tried.
func (p *Proxy) authenticate(h http.Header) (string, bool) {
	for _, candidate := range inboundTokens(h) {
		if candidate == "" {
			continue
		}
		if sessionID, ok := p.cfg.TokenLookup(candidate); ok {
			return sessionID, true
		}
	}
	return "", false
}

func inboundTokens(h http.Header) []string {
	tokens := make([]string, 0, 3)
	// saint's own header comes first. In subscription mode it is the only place
	// the session token can be, because the credential headers hold the guest's
	// subscription token.
	if value := strings.TrimSpace(h.Get(SaintSessionHeader)); value != "" {
		tokens = append(tokens, value)
	}
	if value := strings.TrimSpace(h.Get("x-api-key")); value != "" {
		tokens = append(tokens, value)
	}
	if auth := strings.TrimSpace(h.Get("Authorization")); auth != "" {
		if rest, found := strings.CutPrefix(auth, "Bearer "); found {
			tokens = append(tokens, strings.TrimSpace(rest))
		} else {
			tokens = append(tokens, auth)
		}
	}
	return tokens
}

// route picks the upstream for a request.
//
// The path prefixes are a closed list in key mode and a fast path in
// subscription mode, and the difference is the whole point.
//
// In key mode this proxy attaches the user's own API key to whatever it
// forwards, so the set of paths it will forward is the set of endpoints a guest
// can reach with that key. Widening it to "any path that looks like Anthropic's
// protocol" hands a compromised guest the rest of the API on the user's
// credential - a strictly larger surface than inference, for no benefit the
// user asked for.
//
// In subscription mode the guest holds its own credential and this proxy
// attaches nothing, so forwarding an unknown path grants it nothing it could
// not already do by talking to the provider directly. That is where forward
// compatibility is free, and it is the mode that needs it: Claude Code gains
// capabilities across releases and they arrive as new paths, so a pinned list
// 404s the next feature and surfaces inside the guest as an unexplained hard
// failure.
func (p *Proxy) route(path string, header http.Header, mode string) (string, *url.URL, KeySource, error) {
	switch {
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/complete"), strings.HasPrefix(path, "/v1/models"):
		return "anthropic", p.cfg.AnthropicURL, p.cfg.AnthropicKey, nil
	case strings.HasPrefix(path, "/v1/chat/completions"), strings.HasPrefix(path, "/v1/responses"), strings.HasPrefix(path, "/v1/embeddings"):
		return "openai", p.cfg.OpenAIURL, p.cfg.OpenAIKey, nil
	}

	// Beyond here the path is one this proxy has never heard of. Only a session
	// carrying its own credential gets the benefit of the doubt.
	if mode != AuthSubscription {
		return "", nil, nil, fmt.Errorf("no upstream for path %q", path)
	}

	// Every Anthropic-format request carries anthropic-version, so an unknown
	// path with that header is Anthropic's and belongs upstream, not in a 404.
	if header.Get("anthropic-version") != "" || header.Get("anthropic-beta") != "" {
		return "anthropic", p.cfg.AnthropicURL, p.cfg.AnthropicKey, nil
	}
	if header.Get("openai-beta") != "" || header.Get("openai-organization") != "" {
		return "openai", p.cfg.OpenAIURL, p.cfg.OpenAIKey, nil
	}
	return "", nil, nil, fmt.Errorf("no upstream for path %q", path)
}

func stripAuthHeaders(h http.Header) {
	for key := range h {
		lower := strings.ToLower(key)
		switch {
		case lower == "authorization", lower == "x-api-key":
			h.Del(key)
		case strings.HasPrefix(lower, "openai-api-key"), strings.HasPrefix(lower, "anthropic-api-key"):
			h.Del(key)
		case strings.HasPrefix(lower, "openai-auth"), strings.HasPrefix(lower, "anthropic-auth"):
			h.Del(key)
		}
	}
}
