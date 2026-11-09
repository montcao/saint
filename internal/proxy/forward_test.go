package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// forwardHarness is a proxy with the forward path on, plus the two things a
// test needs to check afterwards: where it was asked to dial, and how often.
type forwardHarness struct {
	proxy *Proxy
	front *httptest.Server

	mu      sync.Mutex
	dialed  []string
	dialErr error

	dials atomic.Int64
}

// newForwardHarness builds a proxy whose forward path dials the given address
// whatever host it is asked for, so no test ever touches the network and a
// hostname in a test can be a name nobody owns.
//
// redirectTo empty means the default dialer is used, which is what the loopback
// guard lives in.
func newForwardHarness(t *testing.T, redirectTo string, mutate func(*ForwardConfig)) *forwardHarness {
	t.Helper()

	h := &forwardHarness{}
	fwd := &ForwardConfig{}
	if redirectTo != "" {
		fwd.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			err := h.dialErr
			h.mu.Unlock()
			h.dials.Add(1)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, redirectTo)
		}
	}
	if mutate != nil {
		mutate(fwd)
	}

	// The reverse path still needs an upstream; forward tests never reach it,
	// and the one test that does is checking exactly that it still works.
	upstream := newUpstream(t, &recordedRequest{})
	cfg := testConfig(t, upstream, nil)
	cfg.Forward = fwd

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	h.proxy = p
	h.front = httptest.NewServer(p)
	t.Cleanup(h.front.Close)
	return h
}

func (h *forwardHarness) addr() string {
	return strings.TrimPrefix(h.front.URL, "http://")
}

func (h *forwardHarness) dialedHosts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

// echoServer answers each connection with "pong-" plus the line it was sent,
// then half-closes. Five bytes in, ten bytes back, so byte counts are checked
// against known numbers rather than "more than zero".
func echoServer(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprintf(conn, "pong-%s", line)
			}()
		}
	}()
	return ln
}

// proxyCredentials is what a client puts in HTTP_PROXY, rendered the way an
// HTTP client sends it.
func proxyCredentials(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("saint:"+token))
}

// sendConnect opens a raw connection to the proxy and sends a CONNECT, which is
// the one request shape no Go client will construct for us.
func sendConnect(t *testing.T, proxyAddr, target, auth string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	request := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth != "" {
		request += "Proxy-Authorization: " + auth + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT error = %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("http.ReadResponse() error = %v", err)
	}
	return conn, reader, resp
}

// waitFor polls a condition. Tunnel byte counts land when the copy goroutines
// finish, which is after the client has its answer, so asserting on them
// immediately would be a race rather than a test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// hostRow finds one session's row for one host.
func hostRow(t *testing.T, report ForwardActivity, sessionID, host string) HostActivity {
	t.Helper()

	for _, session := range report.Sessions {
		if session.SessionID != sessionID {
			continue
		}
		for _, row := range session.Hosts {
			if row.Host == host {
				return row
			}
		}
	}
	t.Fatalf("no row for session %q host %q in %+v", sessionID, host, report)
	return HostActivity{}
}

// A CONNECT has to become a tunnel: bytes in both directions, contents
// untouched, and a record of the host and the volume.
func TestConnectTunnelsBytesBothWaysAndRecordsTheHost(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	conn, reader, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through tunnel error = %v", err)
	}
	got, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read through tunnel error = %v", err)
	}
	if got != "pong-ping\n" {
		t.Fatalf("tunnel returned %q, want %q", got, "pong-ping\n")
	}
	conn.Close()

	if dialed := h.dialedHosts(); len(dialed) != 1 || dialed[0] != "example.test:443" {
		t.Fatalf("dialed %v, want [example.test:443]", dialed)
	}

	waitFor(t, "the tunnel's byte counts", func() bool {
		return hostRow(t, h.proxy.ForwardActivity(), sessionID, "example.test").BytesFromHost == 10
	})

	row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "example.test")
	if row.Port != "443" {
		t.Errorf("Port = %q, want 443", row.Port)
	}
	if row.Tunnels != 1 || row.Requests != 0 || row.Denials != 0 {
		t.Errorf("Tunnels/Requests/Denials = %d/%d/%d, want 1/0/0", row.Tunnels, row.Requests, row.Denials)
	}
	if row.BytesToHost != 5 {
		t.Errorf("BytesToHost = %d, want 5", row.BytesToHost)
	}
	if row.FirstSeen.IsZero() || row.LastSeen.IsZero() || row.LastSeen.Before(row.FirstSeen) {
		t.Errorf("FirstSeen = %v, LastSeen = %v, want both set and in order", row.FirstSeen, row.LastSeen)
	}
}

// Plain HTTP arrives at a forward proxy in absolute form. It has to reach the
// target, come back intact, and be recorded the same way a tunnel is - and the
// credentials that authenticated the guest to saint must not travel on.
func TestAbsoluteFormRequestIsForwardedAndRecorded(t *testing.T) {
	t.Parallel()

	var gotProxyAuth, gotVia, gotSaintHeader string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		gotVia = r.Header.Get("Via")
		gotSaintHeader = r.Header.Get(SaintSessionHeader)
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("hello from the target"))
	}))
	t.Cleanup(target.Close)

	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), nil)

	// The proxy URL carries the session token exactly as HTTP_PROXY would, so
	// this exercises the credential path a guest actually uses.
	proxyURL, err := url.Parse("http://saint:" + sessionToken + "@" + h.addr())
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Post("http://example.test/things", "text/plain", strings.NewReader("12345"))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "hello from the target" {
		t.Fatalf("body = %q, want the target's answer", body)
	}
	if gotProxyAuth != "" {
		t.Error("Proxy-Authorization reached the target; it authenticates the guest to saint and stops here")
	}
	if gotSaintHeader != "" {
		t.Error("saint's own session header reached the target")
	}
	if !strings.Contains(gotVia, "saint") {
		t.Errorf("Via = %q, want it to name saint so a loop is detectable", gotVia)
	}
	if dialed := h.dialedHosts(); len(dialed) != 1 || dialed[0] != "example.test:80" {
		t.Fatalf("dialed %v, want [example.test:80]", dialed)
	}

	row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "example.test")
	if row.Requests != 1 || row.Tunnels != 0 {
		t.Errorf("Requests/Tunnels = %d/%d, want 1/0", row.Requests, row.Tunnels)
	}
	if row.Port != "80" {
		t.Errorf("Port = %q, want 80 filled in for a request that named none", row.Port)
	}
	if row.BytesToHost != 5 {
		t.Errorf("BytesToHost = %d, want the 5 body bytes", row.BytesToHost)
	}
	if row.BytesFromHost != int64(len("hello from the target")) {
		t.Errorf("BytesFromHost = %d, want %d", row.BytesFromHost, len("hello from the target"))
	}
}

// Forward-proxied traffic is authenticated with the session token, because a
// CONNECT carries nothing else. Without it the guest is told how to
// authenticate rather than being tunnelled anonymously.
func TestForwardTrafficRequiresTheSessionToken(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	_, _, resp := sendConnect(t, h.addr(), "example.test:443", "")
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("unauthenticated CONNECT: status = %d, want 407", resp.StatusCode)
	}
	if got := resp.Header.Get("Proxy-Authenticate"); !strings.Contains(got, "saint") {
		t.Errorf("Proxy-Authenticate = %q, want a challenge naming saint", got)
	}
	if got := h.dials.Load(); got != 0 {
		t.Fatalf("dials = %d, want 0 for an unauthenticated CONNECT", got)
	}

	_, _, wrong := sendConnect(t, h.addr(), "example.test:443", proxyCredentials("not-a-session-token"))
	if wrong.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("CONNECT with an unknown token: status = %d, want 407", wrong.StatusCode)
	}

	// A bearer proxy credential resolves to the same session, for clients that
	// send one.
	_, _, bearer := sendConnect(t, h.addr(), "example.test:443", "Bearer "+sessionToken)
	if bearer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT with a bearer proxy credential: status = %d, want 200", bearer.StatusCode)
	}
}

// AllowAnonymous is the documented consequence of an open forward proxy: it
// works, and what it records is that saint does not know who it was.
func TestAnonymousForwardTrafficIsRecordedWithoutASession(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.AllowAnonymous = true
	})

	conn, _, resp := sendConnect(t, h.addr(), "example.test:443", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous CONNECT: status = %d, want 200", resp.StatusCode)
	}
	conn.Close()

	row := hostRow(t, h.proxy.ForwardActivity(), "", "example.test")
	if row.Tunnels != 1 {
		t.Errorf("Tunnels = %d, want 1 recorded under the empty session ID", row.Tunnels)
	}
}

// The reverse-proxy path is not a forward proxy and must not have moved: an
// origin-form request still authenticates, still routes, and still leaves with
// the real provider key attached.
func TestOriginFormRequestStillTakesTheReversePath(t *testing.T) {
	t.Parallel()

	var got recordedRequest
	upstream := newUpstream(t, &got)

	echo := echoServer(t)
	h := &forwardHarness{}
	cfg := testConfig(t, upstream, nil)
	cfg.Forward = &ForwardConfig{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.apiKey != anthropicKey {
		t.Errorf("upstream saw x-api-key = %q, want the injected key", got.apiKey)
	}
	if n := h.dials.Load(); n != 0 {
		t.Errorf("the forward path dialled %d times for an origin-form request, want 0", n)
	}
	if report := p.ForwardActivity(); len(report.Sessions) != 0 {
		t.Errorf("ForwardActivity() = %+v, want nothing: no forward traffic happened", report)
	}
}

// A client given the same address as its base URL and as HTTP_PROXY sends the
// model API call in absolute form to saint's own authority. That has to stay on
// the reverse path - injected credential and all - rather than being forwarded
// to saint by saint.
func TestAbsoluteFormAimedAtTheProxyItselfTakesTheReversePath(t *testing.T) {
	t.Parallel()

	var got recordedRequest
	upstream := newUpstream(t, &got)

	echo := echoServer(t)
	dials := &atomic.Int64{}
	cfg := testConfig(t, upstream, nil)
	cfg.Forward = &ForwardConfig{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	// http.ProxyURL pointed at the proxy's own address is exactly the shape a
	// guest produces when HTTP_PROXY and the base URL are the same host.
	proxyURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.apiKey != anthropicKey {
		t.Errorf("upstream saw x-api-key = %q, want the injected key", got.apiKey)
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("the forward path dialled %d times, want 0", n)
	}
}

// Forward proxying is off unless asked for, and a guest that tries it is told
// so rather than left reading a hung connection as a network fault.
func TestForwardIsOffByDefault(t *testing.T) {
	t.Parallel()

	upstream := newUpstream(t, &recordedRequest{})
	p := newProxy(t, upstream, nil)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	_, _, resp := sendConnect(t, strings.TrimPrefix(front.URL, "http://"), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("CONNECT with forwarding off: status = %d, want 405", resp.StatusCode)
	}

	settings := p.Settings()
	if settings.Forward.Enabled {
		t.Error("Settings().Forward.Enabled is true on a proxy that was never given a ForwardConfig")
	}
}

// A container cannot reach the host's loopback on its own. Forward proxying
// must not be the thing that hands it one.
func TestLoopbackTargetsAreRefusedByDefault(t *testing.T) {
	t.Parallel()

	var reached atomic.Int64
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { local.Close() })
	go func() {
		for {
			conn, err := local.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			conn.Close()
		}
	}()

	// No DialContext override: this is the production dialer, which is where the
	// guard lives.
	h := newForwardHarness(t, "", nil)

	_, _, resp := sendConnect(t, h.addr(), local.Addr().String(), proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to loopback: status = %d, want 403", resp.StatusCode)
	}
	if got := reached.Load(); got != 0 {
		t.Fatalf("the loopback listener accepted %d connections, want 0", got)
	}

	allowed := newForwardHarness(t, "", func(c *ForwardConfig) { c.AllowLoopbackTargets = true })
	conn, _, ok := sendConnect(t, allowed.addr(), local.Addr().String(), proxyCredentials(sessionToken))
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to loopback with AllowLoopbackTargets: status = %d, want 200", ok.StatusCode)
	}
	conn.Close()
	waitFor(t, "the opted-in loopback connection", func() bool { return reached.Load() == 1 })
}

// A guest whose HTTP_PROXY and base URL are the same address would otherwise
// have saint forward to itself until it runs out of descriptors.
func TestSelfDirectedForwardingIsRefused(t *testing.T) {
	t.Parallel()

	// Loopback targets are allowed here precisely so the loop guard, not the
	// loopback guard, is what refuses this.
	h := newForwardHarness(t, "", func(c *ForwardConfig) { c.AllowLoopbackTargets = true })

	_, _, resp := sendConnect(t, h.addr(), h.addr(), proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusLoopDetected {
		t.Fatalf("CONNECT to the proxy itself: status = %d, want 508", resp.StatusCode)
	}

	// The plain HTTP half is caught by the hop marker instead, since that
	// request does arrive with headers.
	req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Via", forwardVia)
	req.Header.Set(SaintSessionHeader, sessionToken)
	proxyURL, err := url.Parse("http://" + h.addr())
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	looped, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	looped.Body.Close()
	if looped.StatusCode != http.StatusLoopDetected {
		t.Fatalf("request already carrying saint's Via: status = %d, want 508", looped.StatusCode)
	}
}

// An unreachable target is a bad gateway, and the guest is told nothing about
// the host's resolver or network.
func TestUnreachableTargetIsABadGateway(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)
	h.mu.Lock()
	h.dialErr = fmt.Errorf("dial tcp 10.1.2.3:443: connect: no route to host")
	h.mu.Unlock()

	_, _, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "10.1.2.3") {
		t.Errorf("the guest was told %q, which carries host network detail", body)
	}
}

// The zero value has to answer rather than panic: a Proxy built without New,
// as the limits tests do, is still a Proxy.
func TestZeroValueProxyReportsNoForwardActivity(t *testing.T) {
	t.Parallel()

	p := &Proxy{}
	if report := p.ForwardActivity(); len(report.Sessions) != 0 || report.Max != 0 {
		t.Fatalf("ForwardActivity() = %+v, want empty", report)
	}
	p.ForgetSession(sessionID)
}

func TestParseTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw         string
		defaultPort string
		wantHost    string
		wantPort    string
		wantErr     bool
	}{
		{raw: "example.test:443", defaultPort: "443", wantHost: "example.test", wantPort: "443"},
		{raw: "Example.TEST", defaultPort: "80", wantHost: "example.test", wantPort: "80"},
		{raw: "example.test.", defaultPort: "80", wantHost: "example.test", wantPort: "80"},
		{raw: "[::1]:8443", defaultPort: "443", wantHost: "::1", wantPort: "8443"},
		{raw: "[::1]", defaultPort: "443", wantHost: "::1", wantPort: "443"},
		{raw: "", defaultPort: "443", wantErr: true},
		{raw: ":443", defaultPort: "443", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()

			got, err := parseTarget(tc.raw, tc.defaultPort)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTarget(%q) = %+v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q) error = %v", tc.raw, err)
			}
			if got.host != tc.wantHost || got.port != tc.wantPort {
				t.Errorf("parseTarget(%q) = %s/%s, want %s/%s", tc.raw, got.host, got.port, tc.wantHost, tc.wantPort)
			}
		})
	}
}
