package proxy

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file adds a second, separate role to the proxy: a forward proxy for the
// traffic that is not model API traffic.
//
// The reverse-proxy path in proxy.go sees exactly one thing - requests an agent
// aimed at the provider base URL saint handed it. Everything else a session
// does is invisible there: git fetch, npm install, a web-search tool, an update
// check. A guest whose HTTP_PROXY and HTTPS_PROXY point here sends that traffic
// through this file instead, which is how the host gets to see where an agent
// went.
//
// What this observes is precisely "traffic that honours the proxy environment
// variables". It is not all egress, it is not a list of everywhere a session
// went, and nothing here may ever be named or described as if it were:
//
//   - a statically linked binary, or one that reads no environment, ignores the
//     variables and never appears here;
//   - anything using raw sockets, its own DNS, or a non-HTTP protocol has
//     nothing to send here;
//   - a guest may unset the variables and keep working, because the container
//     keeps ordinary internet access either way.
//
// That last point is the important one. Nothing in this file is an egress
// control. The optional policy below refuses what reaches this proxy; it does
// not stop anything leaving the microVM, and a report built from this data
// answers "where did the traffic that came through saint go", never "where did
// this session go".
//
// TLS is never intercepted. A CONNECT is tunnelled byte for byte, so what is
// recorded is the host the guest named and how many bytes crossed - never a
// URL, a header, or a payload. Reading inside would mean generating
// certificates and installing a CA in the guest, which is a far larger and far
// more invasive change than seeing where an agent goes.

const (
	// DefaultMaxTrackedHosts caps the activity table. Session IDs come from a
	// lookup this package does not own and hostnames come from the guest, so
	// both halves of the key are attacker-influenced and the table needs a
	// ceiling rather than a hope. See ForwardActivity for what happens at it.
	DefaultMaxTrackedHosts = 4096

	// DefaultForwardDialTimeout bounds connecting to a target host. A guest that
	// names an unroutable host should get an error while its tool still cares,
	// not a socket held open until the kernel gives up.
	DefaultForwardDialTimeout = 15 * time.Second

	// forwardVia marks requests this proxy has already forwarded, so a guest
	// pointed at saint for both its base URL and its HTTP_PROXY cannot make the
	// proxy forward to itself forever. It is a hop marker, not a version claim.
	forwardVia = "1.1 saint"
)

// ForwardConfig turns the forward-proxy path on.
//
// Nil - the default - leaves it off entirely: a CONNECT or an absolute-form
// request is refused with 405 and nothing is dialled, which is what this proxy
// did before this file existed.
type ForwardConfig struct {
	// AllowAnonymous accepts forward-proxied traffic that carries no session
	// credential.
	//
	// Off by default, and spelled as an opt-in for the reason the rest of the
	// limits are: this proxy binds the container bridge gateway, so every
	// container on that bridge can reach the port, not only the ones saint
	// started. With this on, any of them gets a tunnel to any host the policy
	// allows, recorded under the empty session ID because saint genuinely does
	// not know who it was.
	//
	// With it off, a guest authenticates the way every HTTP client already knows
	// how to: credentials in the proxy URL, HTTP_PROXY=http://saint:TOKEN@host:port,
	// which clients turn into Proxy-Authorization on the CONNECT and on
	// absolute-form requests. TOKEN is the session's existing proxy token, so
	// nothing new is issued and the user is still never asked for anything. A
	// client that cannot send proxy credentials gets 407 and falls back to
	// talking to the network directly - it is refused by saint, not cut off.
	AllowAnonymous bool

	// Policy optionally restricts which hosts may be reached. Nil allows every
	// host, which is the default.
	Policy *HostPolicy

	// Enforce turns the host policy from a record into a control.
	//
	// Off, the proxy observes: every host a session reaches is recorded and
	// none is refused, which is the behaviour this feature shipped with. On,
	// anything neither ruled on nor in the default allowlist is put to a
	// person through Ask, and refused if nobody answers.
	//
	// It is separate from Policy because the two say different things. A policy
	// with an empty allowlist means "no opinion"; enforcement is the decision
	// that no opinion should mean no.
	Enforce bool

	// Ask is consulted for a host nothing has ruled on. Nil means there is
	// nobody to ask, and with Enforce on, an unruled host is then denied - a
	// headless saint does not quietly become a permissive one.
	Ask func(ctx context.Context, sessionID, host string) Decision

	// AllowLoopbackTargets permits forward-proxying to loopback and link-local
	// addresses. Off by default, and the one denial that applies even with no
	// policy configured.
	//
	// The asymmetry is deliberate. A container cannot reach the host's
	// 127.0.0.1 on its own, so tunnelling there would hand a guest something it
	// did not otherwise have: every unauthenticated service the developer runs
	// locally, reached through saint. Link-local goes with it because
	// 169.254.169.254 is the cloud metadata address. Ordinary private ranges are
	// not blocked - the container already routes to those directly, so refusing
	// them would cost a real workload and buy nothing.
	AllowLoopbackTargets bool

	// MaxTrackedHosts caps how many (session, host) pairs the activity table
	// holds. Zero takes DefaultMaxTrackedHosts. There is no way to remove the
	// cap: an unbounded table is a leak, not a feature.
	MaxTrackedHosts int

	// DialTimeout bounds connecting to a target. Zero takes
	// DefaultForwardDialTimeout.
	DialTimeout time.Duration

	// DialContext overrides how target connections are made. Production leaves
	// it nil; tests point it at a local listener so no test reaches the network.
	// Overriding it also replaces the loopback guard above, which lives in the
	// default dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// HostPolicy is an optional allow/deny list for forward-proxied hosts.
//
// Matching is on the hostname, lowercased, with the port ignored: a rule about
// example.com is about the host, not one of its ports. A rule is either an
// exact hostname or a wildcard written "*.example.com" or ".example.com", which
// matches example.com and any subdomain of it.
//
// Deny wins over Allow. An empty Allow allows everything not denied - the
// default, and what "off unless asked for" means here. A non-empty Allow
// refuses every host it does not name.
//
// Remember what a denial is worth: it refuses traffic that came to saint. The
// same guest can reach the same host by not using the proxy.
type HostPolicy struct {
	Allow []string
	Deny  []string
}

// permits reports whether the policy lets a hostname through. A nil policy
// permits everything.
func (pol *HostPolicy) permits(hostname string) bool {
	if pol == nil {
		return true
	}
	for _, rule := range pol.Deny {
		if matchHostRule(rule, hostname) {
			return false
		}
	}
	if len(pol.Allow) == 0 {
		return true
	}
	for _, rule := range pol.Allow {
		if matchHostRule(rule, hostname) {
			return true
		}
	}
	return false
}

func matchHostRule(rule, hostname string) bool {
	rule = strings.ToLower(strings.TrimSpace(rule))
	rule = strings.TrimPrefix(rule, "*")
	if rule == "" || rule == "." {
		return false
	}
	if domain, wildcard := strings.CutPrefix(rule, "."); wildcard {
		return hostname == domain || strings.HasSuffix(hostname, "."+domain)
	}
	return hostname == rule
}

// HostActivity is what one session did with one host, as seen on the
// forward-proxy path.
//
// Bytes are what crossed the tunnel, in each direction, including TLS framing.
// They are not payload sizes and there is no way for this proxy to know payload
// sizes without decrypting, which it does not do.
type HostActivity struct {
	// Host is the hostname the guest named, lowercased. It is not resolved and
	// not verified: it is what the guest asked for.
	Host string

	// Port is the port the guest named, with the scheme default filled in when
	// the request omitted it - 443 for CONNECT, 80 for plain HTTP.
	Port string

	// Tunnels counts CONNECT tunnels opened, Requests counts absolute-form
	// plain HTTP requests forwarded. They are separate because they see
	// different amounts: a tunnel's contents are opaque, a plain request's are
	// not, and collapsing them into one "connections" number would hide that.
	Tunnels  int64
	Requests int64

	// Denials counts attempts the host policy refused. A host with denials and
	// no tunnels was never connected to by saint.
	Denials int64

	BytesToHost   int64
	BytesFromHost int64

	FirstSeen time.Time
	LastSeen  time.Time
}

// SessionForwardActivity groups one session's hosts.
type SessionForwardActivity struct {
	// SessionID is empty when the traffic carried no session credential, which
	// only happens when ForwardConfig.AllowAnonymous is on. An empty ID means
	// "saint does not know who this was", never "no session".
	SessionID string

	// Hosts is sorted by host then port so a renderer never has to decide.
	Hosts []HostActivity
}

// ForwardActivity is the whole of what the forward-proxy path observed.
type ForwardActivity struct {
	// Sessions is sorted by session ID.
	Sessions []SessionForwardActivity

	// Tracked and Max describe the table itself.
	Tracked int
	Max     int

	// Dropped counts observations discarded because the table was full.
	//
	// At the cap, hosts already in the table keep counting and new
	// (session, host) pairs are not added. Dropped is how a reader tells a
	// complete table from a truncated one, and it must be shown wherever this
	// data is: a table that silently stopped recording is exactly the confident
	// wrong answer this codebase forbids.
	Dropped int64
}

// ForwardActivity reports the hosts this proxy was asked to reach on the
// forward-proxy path, per session.
//
// It covers only traffic that honoured HTTP_PROXY/HTTPS_PROXY and reached this
// proxy. Anything a session sent straight out of the microVM - a tool that
// ignores the variables, a static binary, raw sockets, non-HTTP protocols - is
// not here and cannot be. Render it as what came through saint, never as
// everywhere a session went.
//
// Safe to call at any time, including while tunnels are live; the byte counters
// of a live tunnel are as of the moment of the call.
func (p *Proxy) ForwardActivity() ForwardActivity {
	if p == nil || p.forward == nil {
		return ForwardActivity{}
	}
	return p.forward.table.snapshot()
}

// ForgetSession drops a session's rows from the activity table.
//
// The table is otherwise append-only within its cap, and session IDs outlive
// nothing here, so the package that owns session lifecycle calls this when a
// session ends. Doing so is the only way the table shrinks.
func (p *Proxy) ForgetSession(sessionID string) {
	if p == nil || p.forward == nil {
		return
	}
	p.forward.table.forget(sessionID)
}

// CloseTunnels closes every hijacked connection this proxy is holding open, and
// stops it accepting new ones.
//
// http.Server.Shutdown does not touch hijacked connections - its documentation
// says so outright - and a CONNECT tunnel is hijacked by definition. Without
// this, quitting saint while a tunnel is open returns from Shutdown with the
// socket, its two copy goroutines and the guest's connection all still live:
// the server is gone and the thing it created has outlived it. In a running
// saint that is a process that will not exit, which is the failure this
// codebase treats as the worst one it can ship. In a test it is a leaked
// goroutine that makes some unrelated test flake.
//
// It is a shutdown operation and it is final. After it returns, a CONNECT that
// was mid-hijack is closed rather than tunnelled, and later ones are refused
// the same way, because a tunnel that opened just after the sweep would be one
// the shutdown believed it had closed. Calling it twice, on a proxy with no
// tunnels, or on one with forward proxying switched off, is safe and does
// nothing.
//
// The error is what closing the sockets reported, joined. Connections the far
// end had already closed are left out of it: that is the state this method was
// asking for, not a failure to reach it.
func (p *Proxy) CloseTunnels() error {
	if p == nil || p.forward == nil {
		return nil
	}
	return p.forward.tunnels.closeAll()
}

// forwarder holds everything the forward-proxy path needs that a Proxy does not
// already have.
type forwarder struct {
	cfg       ForwardConfig
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	transport http.RoundTripper
	table     *forwardTable
	now       func() time.Time

	// tunnels holds the hijacked connections that are open right now. It is
	// what makes shutdown able to reach them at all; see Proxy.CloseTunnels.
	tunnels *liveTunnels
}

func newForwarder(cfg ForwardConfig) *forwarder {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = DefaultForwardDialTimeout
	}
	dial := cfg.DialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: timeout, Control: guardTarget(cfg.AllowLoopbackTargets)}
		dial = dialer.DialContext
	}
	f := &forwarder{
		cfg:     cfg,
		dial:    dial,
		table:   newForwardTable(cfg.MaxTrackedHosts),
		now:     time.Now,
		tunnels: newLiveTunnels(),
	}
	f.transport = &http.Transport{
		DialContext: dial,

		// Proxy is nil on purpose. Inheriting the host's own HTTP_PROXY here
		// would chain saint behind whatever the developer's shell happens to be
		// configured for, quietly changing where a guest's traffic goes.
		Proxy: nil,

		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
	}
	return f
}

// errLoopbackTarget refuses a target that resolved to the host's own loopback
// or a link-local address. See ForwardConfig.AllowLoopbackTargets.
var errLoopbackTarget = errors.New("target address is loopback or link-local")

// guardTarget checks the resolved address rather than the name, which is what
// makes it a check at all: a hostname is free to resolve to 127.0.0.1, and only
// the address the dialer is about to connect to says where the bytes go.
func guardTarget(allowLoopback bool) func(network, address string, c syscall.RawConn) error {
	if allowLoopback {
		return nil
	}
	return func(_, address string, _ syscall.RawConn) error {
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			// The dialer hands this function a resolved address; anything else
			// is a shape this guard does not understand, and refusing is the
			// safe direction.
			return fmt.Errorf("%w: unparseable address %q", errLoopbackTarget, address)
		}
		addr := addrPort.Addr().Unmap()
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
			return errLoopbackTarget
		}
		return nil
	}
}

// target is an authority a forward-proxied request named.
type target struct {
	host string
	port string
}

func (t target) addr() string { return net.JoinHostPort(t.host, t.port) }

// parseTarget normalises the authority from a request line. defaultPort fills
// in what the guest omitted, which CONNECT never does and plain HTTP usually
// does.
func parseTarget(raw, defaultPort string) (target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return target{}, errors.New("no host in request")
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host, port = strings.Trim(raw, "[]"), defaultPort
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || port == "" {
		return target{}, fmt.Errorf("malformed host %q", raw)
	}
	return target{host: host, port: port}, nil
}

// isForwardRequest reports whether a request arrived in one of the two shapes a
// forward proxy receives.
//
// The two shapes cannot be confused with the reverse-proxy path: an agent
// pointed at saint's base URL sends origin-form requests, whose request line
// carries a path and no authority.
func isForwardRequest(r *http.Request) bool {
	if r.Method == http.MethodConnect {
		return true
	}
	return r.URL != nil && r.URL.IsAbs() && r.URL.Host != ""
}

// isSelfAddressed reports whether an absolute-form request named the address it
// arrived on.
//
// The comparison is against the connection's own local address, which is exact:
// it is the address the guest actually reached, not a guess about how saint was
// configured. A guest that names the same machine by some other name is not
// caught here and meets the loop guards instead. CONNECT is excluded because a
// CONNECT to saint's own address is a tunnel request, not a request for saint,
// and serveConnect refuses it after the dial.
func isSelfAddressed(r *http.Request) bool {
	if r.Method == http.MethodConnect || r.URL == nil {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	return strings.EqualFold(r.URL.Host, local.String())
}

// serveForward handles both forward-proxy shapes.
//
// Nothing on this path is ever given a provider credential. saint's own header
// is stripped, the guest's Proxy-Authorization stops here, and what goes out is
// what the guest sent - this proxy is a pipe with a notebook, not a signer.
func (p *Proxy) serveForward(w http.ResponseWriter, r *http.Request) {
	f := p.forward
	if f == nil {
		// Off is the default, and the guest is told plainly rather than being
		// left to read a tunnel failure as a network problem.
		w.Header().Set("Allow", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
		http.Error(w, "saint: forward proxying is not enabled", http.StatusMethodNotAllowed)
		return
	}

	// A guest pointed at saint for both ANTHROPIC_BASE_URL and HTTP_PROXY can
	// send this proxy a request aimed at itself. Plain HTTP carries the hop
	// marker that catches it; the CONNECT case is caught after the dial, by
	// comparing the address we reached with the one the guest reached us on.
	if hasForwardVia(r.Header) {
		http.Error(w, "saint: request already crossed this proxy", http.StatusLoopDetected)
		return
	}

	sessionID, ok := p.authenticateForward(w, r)
	if !ok {
		return
	}

	if r.Method == http.MethodConnect {
		p.serveConnect(w, r, sessionID)
		return
	}
	p.serveAbsolute(w, r, sessionID)
}

// authenticateForward resolves a forward-proxied request to a session.
//
// A CONNECT carries no application headers at all, so the per-session token
// cannot arrive the way it does on the reverse path. Proxy-Authorization is the
// only channel a forward proxy has, which is why the session token doubles as
// proxy credentials. When it is absent the request is refused with 407 unless
// AllowAnonymous is set - see the consequence stated there.
//
// It writes the refusal itself and reports whether the caller may continue.
func (p *Proxy) authenticateForward(w http.ResponseWriter, r *http.Request) (string, bool) {
	for _, candidate := range forwardTokens(r.Header) {
		if sessionID, ok := p.cfg.TokenLookup(candidate); ok {
			return sessionID, true
		}
	}
	if p.forward.cfg.AllowAnonymous {
		return "", true
	}
	// Basic, because that is what every client sends when given credentials in
	// a proxy URL. The realm names saint so a human reading a tool's error
	// knows what asked.
	w.Header().Set("Proxy-Authenticate", `Basic realm="saint"`)
	http.Error(w, "saint: proxy authentication required", http.StatusProxyAuthRequired)
	return "", false
}

// forwardTokens lists the places a session token can arrive on a forward-proxied
// request, best first. It reads secrets and returns them to the caller; it must
// never log one.
func forwardTokens(h http.Header) []string {
	tokens := make([]string, 0, 3)
	if value := strings.TrimSpace(h.Get(SaintSessionHeader)); value != "" {
		tokens = append(tokens, value)
	}
	auth := strings.TrimSpace(h.Get("Proxy-Authorization"))
	scheme, rest, found := strings.Cut(auth, " ")
	if !found {
		return tokens
	}
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(scheme) {
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return tokens
		}
		user, password, split := strings.Cut(string(decoded), ":")
		// The token is the password in HTTP_PROXY=http://saint:TOKEN@host:port.
		// A URL with only a username is accepted too, so a user who wrote the
		// token in the shorter form is not left with a 407 they cannot explain.
		if split && password != "" {
			tokens = append(tokens, password)
		}
		if user != "" {
			tokens = append(tokens, user)
		}
	case "bearer":
		if rest != "" {
			tokens = append(tokens, rest)
		}
	}
	return tokens
}

// serveConnect tunnels an HTTPS connection and records the host.
//
// Everything after the 200 is opaque bytes. saint learns the authority from the
// request line and the volume in each direction, and that is the whole of it.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request, sessionID string) {
	f := p.forward

	tgt, err := parseTarget(r.Host, "443")
	if err != nil {
		http.Error(w, "saint: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !f.allows(r.Context(), sessionID, tgt.host) {
		f.table.observe(sessionID, tgt, observeDenial, f.now())
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward denied CONNECT %s (host policy)", sessionID, tgt.addr())
		}
		http.Error(w, "saint: host "+tgt.host+" is not allowed by this proxy's host policy", http.StatusForbidden)
		return
	}

	// Dial before hijacking. A failed dial can still be answered with a status
	// the guest's client understands; once the connection is hijacked, the only
	// way to report anything is to hand-write an HTTP response.
	upstream, err := f.dial(r.Context(), "tcp", tgt.addr())
	if err != nil {
		status, message := forwardDialFailure(err)
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward CONNECT %s failed: %v", sessionID, tgt.addr(), err)
		}
		http.Error(w, message, status)
		return
	}
	if sameEndpoint(r.Context(), upstream) {
		upstream.Close()
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward CONNECT %s refused: target is this proxy", sessionID, tgt.addr())
		}
		http.Error(w, "saint: that host is this proxy", http.StatusLoopDetected)
		return
	}

	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		upstream.Close()
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward CONNECT %s cannot hijack: %v", sessionID, tgt.addr(), err)
		}
		http.Error(w, "saint: this connection cannot be tunnelled", http.StatusInternalServerError)
		return
	}
	defer client.Close()
	defer upstream.Close()

	// Register before the 200 goes out. From the hijack onward this connection
	// is no longer one http.Server knows about, so between those two moments it
	// is reachable by nothing at all - and that is exactly the window a quit can
	// land in. Registering first means the worst case is a tunnel closed a
	// moment after it opened, rather than one that outlives the server.
	remove, tracked := f.tunnels.add(client, upstream)
	if !tracked {
		// CloseTunnels has already swept. Serving this tunnel would create the
		// connection that shutdown just finished guaranteeing does not exist,
		// so it is abandoned - the deferred closes above end both ends, and the
		// guest sees the connection drop rather than a tunnel that works for a
		// second.
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward CONNECT %s abandoned: proxy is shutting down", sessionID, tgt.addr())
		}
		return
	}
	defer remove()

	// Clear the deadline the server set while reading request headers. Left in
	// place it kills a quiet tunnel - an idle SSH-over-HTTPS session, a long
	// poll - seconds after it opens, which looks like a broken network.
	_ = client.SetDeadline(time.Time{})

	if _, err := brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	entry := f.table.observe(sessionID, tgt, observeTunnel, f.now())
	if p.cfg.Logf != nil {
		p.cfg.Logf("session=%s forward tunnel %s (contents not observed)", sessionID, tgt.addr())
	}

	// brw.Reader, not client, is the read side: a client is allowed to pipeline
	// its first bytes behind the CONNECT, and those are already sitting in that
	// buffer.
	tunnel(brw.Reader, client, upstream, entry)
	f.table.touch(entry, f.now())
}

// tunnel copies bytes both ways until either side is done, counting each
// direction into the activity entry as it goes.
//
// Counting as it goes rather than at the end matters: a tunnel can stay open
// for hours, and a table that reported zero bytes for a live connection would
// be showing a number nobody measured.
func tunnel(clientRead io.Reader, clientWrite, upstream net.Conn, entry *hostEntry) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(countingWriter{w: upstream, add: entry.addToHost}, clientRead)
		// Half-close so the far end sees the guest's EOF and can answer it,
		// rather than the whole tunnel dying the moment one direction ends.
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(countingWriter{w: clientWrite, add: entry.addFromHost}, upstream)
		closeWrite(clientWrite)
	}()
	wg.Wait()
}

// serveAbsolute forwards a plain HTTP request that arrived in absolute form,
// which is how a forward proxy receives non-TLS traffic.
func (p *Proxy) serveAbsolute(w http.ResponseWriter, r *http.Request, sessionID string) {
	f := p.forward

	if r.URL.Scheme != "http" {
		// https in a request line means a client tried to send a TLS request
		// without a tunnel. Naming the scheme is the only useful thing to say.
		http.Error(w, "saint: cannot forward scheme "+r.URL.Scheme+" without CONNECT", http.StatusBadRequest)
		return
	}
	tgt, err := parseTarget(r.URL.Host, "80")
	if err != nil {
		http.Error(w, "saint: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !f.allows(r.Context(), sessionID, tgt.host) {
		f.table.observe(sessionID, tgt, observeDenial, f.now())
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward denied %s http://%s%s (host policy)", sessionID, r.Method, tgt.addr(), r.URL.Path)
		}
		http.Error(w, "saint: host "+tgt.host+" is not allowed by this proxy's host policy", http.StatusForbidden)
		return
	}

	entry := f.table.observe(sessionID, tgt, observeRequest, f.now())

	outbound := r.Clone(r.Context())
	// RequestURI is set on server requests and forbidden on client ones.
	outbound.RequestURI = ""
	outbound.URL.Host = tgt.addr()
	outbound.Host = tgt.addr()
	removeHopByHop(outbound.Header)
	// saint's own header never leaves the host, on either path.
	outbound.Header.Del(SaintSessionHeader)
	outbound.Header.Add("Via", forwardVia)
	if outbound.Body != nil {
		outbound.Body = &countingBody{src: outbound.Body, add: entry.addToHost}
	}

	if p.cfg.Logf != nil {
		p.cfg.Logf("session=%s forward %s http://%s%s", sessionID, r.Method, tgt.addr(), r.URL.Path)
	}

	resp, err := f.transport.RoundTrip(outbound)
	if err != nil {
		status, message := forwardDialFailure(err)
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s forward %s http://%s%s failed: %v", sessionID, r.Method, tgt.addr(), r.URL.Path, err)
		}
		http.Error(w, message, status)
		return
	}
	defer resp.Body.Close()

	removeHopByHop(resp.Header)
	header := w.Header()
	for key, values := range resp.Header {
		header[key] = values
	}
	w.WriteHeader(resp.StatusCode)

	// Flush per chunk: some of what crosses a forward proxy is a long poll or a
	// progress stream, and buffering it here would stall a tool that is working
	// correctly.
	_, _ = io.Copy(flushWriter{
		w:  countingWriter{w: w, add: entry.addFromHost},
		rc: http.NewResponseController(w),
	}, resp.Body)
	f.table.touch(entry, f.now())
}

// forwardDialFailure turns an outbound failure into what the guest is told.
//
// The raw error carries resolver detail and local network topology, so it goes
// to the log and never to the guest - the same rule the reverse path's
// ErrorHandler follows. The one distinction worth making is the loopback
// refusal, because that is saint's decision rather than the network's.
func forwardDialFailure(err error) (int, string) {
	if errors.Is(err, errLoopbackTarget) {
		return http.StatusForbidden, "saint: refusing to proxy to a loopback or link-local address"
	}
	return http.StatusBadGateway, "saint: cannot reach that host"
}

// sameEndpoint reports whether a dial came back to the address the guest
// reached this proxy on. That is the CONNECT loop: a guest whose HTTPS_PROXY
// and base URL are the same address would otherwise have saint tunnel to itself
// until it runs out of descriptors.
func sameEndpoint(ctx context.Context, conn net.Conn) bool {
	local, ok := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || conn == nil || conn.RemoteAddr() == nil {
		return false
	}
	return local.String() == conn.RemoteAddr().String()
}

func hasForwardVia(h http.Header) bool {
	for _, value := range h.Values("Via") {
		if strings.Contains(strings.ToLower(value), "saint") {
			return true
		}
	}
	return false
}

// hopByHopHeaders are the headers that belong to one connection and must not be
// passed on. Proxy-Authorization is in the list because it authenticated the
// guest to saint and means nothing to the target.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHop(h http.Header) {
	// Connection names further headers that are themselves hop-by-hop, and they
	// have to go before Connection itself does.
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

type countingWriter struct {
	w   io.Writer
	add func(int64)
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

type countingBody struct {
	src io.ReadCloser
	add func(int64)
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

func (c *countingBody) Close() error { return c.src.Close() }

// flushWriter pushes each chunk to the guest as it arrives. A response writer
// that cannot flush is not an error: the bytes still arrive, just later.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		_ = f.rc.Flush()
	}
	return n, err
}

// closeWrite ends one direction of a tunnel. A connection that cannot half-close
// - net.Pipe in tests, anything not TCP - is closed outright, which ends the
// tunnel rather than leaving a goroutine blocked on a read that will never
// return.
func closeWrite(conn net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// observation kinds, kept as a type so a caller cannot pass the wrong string.
type observation int

const (
	observeTunnel observation = iota
	observeRequest
	observeDenial
)

// liveTunnels holds the hijacked connections the forward path currently has
// open, so shutdown can reach them. Proxy.CloseTunnels says why anything has
// to.
//
// There is no configured ceiling here, and that is a deliberate difference from
// forwardTable below rather than an omission. That table is capped because its
// keys accumulate for the life of the process and both halves - session IDs and
// guest-supplied hostnames - are attacker-influenced. This set holds one entry
// per socket that is open at this instant, and every entry removes itself when
// its tunnel ends, so its size is the number of live tunnels: a quantity the
// file descriptor limit already bounds far below anything a map cares about. A
// number on top of that could only do one of two things and both are worse than
// no cap. Refusing a tunnel at the ceiling would drop legitimate agent traffic
// to protect a map. Serving it untracked would leave running exactly the
// connection CloseTunnels exists to close.
type liveTunnels struct {
	mu     sync.Mutex
	closed bool
	next   uint64
	open   map[uint64]tunnelPair
}

// tunnelPair is the two ends of one tunnel. Both are kept, not just the
// guest's: closing only the client end leaves the goroutine copying from the
// target blocked on a read that nothing is going to answer.
type tunnelPair struct {
	client   net.Conn
	upstream net.Conn
}

func newLiveTunnels() *liveTunnels {
	return &liveTunnels{open: make(map[uint64]tunnelPair)}
}

// add registers a live tunnel and returns the function that unregisters it.
//
// The key is a counter rather than the connection itself, so a tunnel removes
// exactly the entry it added even if the runtime hands out an identical
// net.Conn value later.
//
// ok is false once closeAll has run, and the caller must then abandon the
// tunnel rather than serve it.
func (s *liveTunnels) add(client, upstream net.Conn) (remove func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	id := s.next
	s.next++
	s.open[id] = tunnelPair{client: client, upstream: upstream}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.open, id)
	}, true
}

// closeAll ends every tunnel and closes the set to new ones.
func (s *liveTunnels) closeAll() error {
	s.mu.Lock()
	s.closed = true
	pairs := make([]tunnelPair, 0, len(s.open))
	for _, pair := range s.open {
		pairs = append(pairs, pair)
	}
	clear(s.open)
	s.mu.Unlock()

	// The sockets are closed outside the lock. Close on a connection with a
	// blocked reader takes as long as the kernel takes, and holding the lock
	// across all of them would stall every tunnel trying to unregister itself
	// at the same moment - which, during a quit, is all of them.
	var errs []error
	for _, pair := range pairs {
		errs = append(errs, closeTunnelEnd(pair.client), closeTunnelEnd(pair.upstream))
	}
	return errors.Join(errs...)
}

// closeTunnelEnd closes one end and drops the error that only says it was
// already closed. A tunnel finishing on its own at the same moment reports
// that, and passing it on from a method whose entire job is to close things
// would be noise no caller can act on.
func closeTunnelEnd(conn net.Conn) error {
	if conn == nil {
		return nil
	}
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// len reports how many tunnels are open. Tests use it to check that the set
// drains rather than inferring it from the client side.
func (s *liveTunnels) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// forwardTable is the activity table.
//
// Both halves of its key come from outside: session IDs from a lookup this
// package does not own, hostnames from the guest. So it is capped, and the
// number of observations the cap cost is kept rather than discarded - a
// truncated table that looks complete would be worse than no table.
type forwardTable struct {
	max int

	mu       sync.Mutex
	sessions map[string]map[string]*hostEntry
	tracked  int
	dropped  int64
}

// hostEntry is one (session, host) row. Its own mutex covers every field: the
// byte counters are written by two tunnel goroutines per connection, by every
// concurrent tunnel to the same host, and read by ForwardActivity at any time.
type hostEntry struct {
	host string
	port string

	mu            sync.Mutex
	tunnels       int64
	requests      int64
	denials       int64
	bytesToHost   int64
	bytesFromHost int64
	firstSeen     time.Time
	lastSeen      time.Time
}

func (e *hostEntry) addToHost(n int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.bytesToHost += n
	e.mu.Unlock()
}

func (e *hostEntry) addFromHost(n int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.bytesFromHost += n
	e.mu.Unlock()
}

func newForwardTable(max int) *forwardTable {
	if max <= 0 {
		max = DefaultMaxTrackedHosts
	}
	return &forwardTable{max: max, sessions: make(map[string]map[string]*hostEntry)}
}

// observe records one event against a (session, host) row and returns the row,
// which is nil when the table was full. Every caller of the returned entry has
// to tolerate nil, because "the table was full" must not be able to break a
// tunnel that is otherwise fine.
func (t *forwardTable) observe(sessionID string, tgt target, kind observation, now time.Time) *hostEntry {
	key := tgt.addr()

	t.mu.Lock()
	hosts, ok := t.sessions[sessionID]
	if !ok {
		hosts = make(map[string]*hostEntry)
		t.sessions[sessionID] = hosts
	}
	entry, ok := hosts[key]
	if !ok {
		if t.tracked >= t.max {
			t.dropped++
			t.mu.Unlock()
			return nil
		}
		entry = &hostEntry{host: tgt.host, port: tgt.port, firstSeen: now}
		hosts[key] = entry
		t.tracked++
	}
	t.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	switch kind {
	case observeTunnel:
		entry.tunnels++
	case observeRequest:
		entry.requests++
	case observeDenial:
		entry.denials++
	}
	entry.lastSeen = now
	return entry
}

// touch marks a row as active now. It is what a tunnel calls when it closes, so
// LastSeen means "last byte crossed" rather than "connection opened".
func (t *forwardTable) touch(entry *hostEntry, now time.Time) {
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.lastSeen = now
	entry.mu.Unlock()
}

func (t *forwardTable) forget(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tracked -= len(t.sessions[sessionID])
	delete(t.sessions, sessionID)
}

func (t *forwardTable) snapshot() ForwardActivity {
	t.mu.Lock()
	defer t.mu.Unlock()

	report := ForwardActivity{
		Sessions: make([]SessionForwardActivity, 0, len(t.sessions)),
		Tracked:  t.tracked,
		Max:      t.max,
		Dropped:  t.dropped,
	}
	for sessionID, hosts := range t.sessions {
		row := SessionForwardActivity{SessionID: sessionID, Hosts: make([]HostActivity, 0, len(hosts))}
		for _, entry := range hosts {
			entry.mu.Lock()
			row.Hosts = append(row.Hosts, HostActivity{
				Host:          entry.host,
				Port:          entry.port,
				Tunnels:       entry.tunnels,
				Requests:      entry.requests,
				Denials:       entry.denials,
				BytesToHost:   entry.bytesToHost,
				BytesFromHost: entry.bytesFromHost,
				FirstSeen:     entry.firstSeen,
				LastSeen:      entry.lastSeen,
			})
			entry.mu.Unlock()
		}
		slices.SortFunc(row.Hosts, func(a, b HostActivity) int {
			if by := cmp.Compare(a.Host, b.Host); by != 0 {
				return by
			}
			return cmp.Compare(a.Port, b.Port)
		})
		report.Sessions = append(report.Sessions, row)
	}
	slices.SortFunc(report.Sessions, func(a, b SessionForwardActivity) int {
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return report
}

// ForwardSettings is the forward-proxy posture, as values a caller can render
// honestly. Like Settings, it formats nothing.
type ForwardSettings struct {
	// Enabled reports whether the forward-proxy path is on at all. Everything
	// below is meaningless when it is false.
	Enabled bool

	// AnonymousAllowed reports whether traffic with no session credential is
	// accepted. True means any container on the bridge can use this proxy.
	AnonymousAllowed bool

	// LoopbackTargetsAllowed reports whether the host's own loopback and
	// link-local addresses may be reached through this proxy.
	LoopbackTargetsAllowed bool

	// AllowHosts and DenyHosts are the policy rules in force, sorted. An empty
	// AllowHosts means every host not denied, which is not the same as no host.
	AllowHosts []string
	DenyHosts  []string

	// MaxTrackedHosts is the ceiling on (session, host) rows.
	MaxTrackedHosts int
}

func (p *Proxy) forwardSettings() ForwardSettings {
	if p.forward == nil {
		return ForwardSettings{}
	}
	cfg := p.forward.cfg
	settings := ForwardSettings{
		Enabled:                true,
		AnonymousAllowed:       cfg.AllowAnonymous,
		LoopbackTargetsAllowed: cfg.AllowLoopbackTargets,
		MaxTrackedHosts:        p.forward.table.max,
	}
	if cfg.Policy != nil {
		settings.AllowHosts = slices.Sorted(slices.Values(cfg.Policy.Allow))
		settings.DenyHosts = slices.Sorted(slices.Values(cfg.Policy.Deny))
	}
	return settings
}

// allows is the single place a forward-proxied host is ruled on.
//
// Both request shapes - a CONNECT and an absolute-form HTTP request - come
// through here, because a policy that applied to one and not the other would be
// a policy with a documented way around it.
func (f *forwarder) allows(ctx context.Context, sessionID, host string) bool {
	// Observation mode keeps the original behaviour exactly: explicit rules are
	// honoured, and anything else passes. Turning enforcement on is what makes
	// "not ruled on" mean no.
	if !f.cfg.Enforce {
		return f.cfg.Policy.permits(host)
	}

	decider := &policyDecider{
		policy:    f.cfg.Policy,
		allowlist: defaultAllowedHosts,
		ask:       f.cfg.Ask,
	}
	return decider.Decide(ctx, sessionID, host) == DecisionAllow
}
