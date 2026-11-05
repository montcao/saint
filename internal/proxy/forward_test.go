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

// A denied host is refused before anything is dialled, and the attempt is
// recorded - a host with a denial and no connection is exactly the thing the
// table exists to show.
func TestHostPolicyDeniesAndRecordsWithoutDialling(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.Policy = &HostPolicy{Deny: []string{"*.blocked.test"}}
	})

	_, _, resp := sendConnect(t, h.addr(), "api.blocked.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to a denied host: status = %d, want 403", resp.StatusCode)
	}
	if got := h.dials.Load(); got != 0 {
		t.Fatalf("dials = %d, want 0: a denied host must never be connected to", got)
	}

	row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "api.blocked.test")
	if row.Denials != 1 || row.Tunnels != 0 {
		t.Errorf("Denials/Tunnels = %d/%d, want 1/0", row.Denials, row.Tunnels)
	}

	// An allow list that does not name the host refuses it too, and the host it
	// does name still goes through.
	h2 := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.Policy = &HostPolicy{Allow: []string{"allowed.test"}}
	})
	_, _, denied := sendConnect(t, h2.addr(), "other.test:443", proxyCredentials(sessionToken))
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("host outside the allow list: status = %d, want 403", denied.StatusCode)
	}
	_, _, allowed := sendConnect(t, h2.addr(), "allowed.test:443", proxyCredentials(sessionToken))
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("host on the allow list: status = %d, want 200", allowed.StatusCode)
	}
}

func TestHostPolicyMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy *HostPolicy
		host   string
		want   bool
	}{
		{name: "nil policy allows everything", policy: nil, host: "anything.test", want: true},
		{name: "empty policy allows everything", policy: &HostPolicy{}, host: "anything.test", want: true},
		{name: "exact deny", policy: &HostPolicy{Deny: []string{"bad.test"}}, host: "bad.test", want: false},
		{name: "exact deny does not match a subdomain", policy: &HostPolicy{Deny: []string{"bad.test"}}, host: "sub.bad.test", want: true},
		{name: "wildcard deny matches a subdomain", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "sub.bad.test", want: false},
		{name: "wildcard deny matches the domain itself", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "bad.test", want: false},
		{name: "dot form is the same wildcard", policy: &HostPolicy{Deny: []string{".bad.test"}}, host: "sub.bad.test", want: false},
		{name: "wildcard does not match a lookalike suffix", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "notbad.test", want: true},
		{name: "allow list refuses what it does not name", policy: &HostPolicy{Allow: []string{"good.test"}}, host: "other.test", want: false},
		{name: "allow list permits what it names", policy: &HostPolicy{Allow: []string{"good.test"}}, host: "good.test", want: true},
		{name: "deny wins over allow", policy: &HostPolicy{Allow: []string{"good.test"}, Deny: []string{"good.test"}}, host: "good.test", want: false},
		{name: "rules are case insensitive", policy: &HostPolicy{Deny: []string{"BAD.test"}}, host: "bad.test", want: false},
		{name: "an empty rule matches nothing", policy: &HostPolicy{Deny: []string{"", "*"}}, host: "bad.test", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.policy.permits(tc.host); got != tc.want {
				t.Errorf("permits(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
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

// Many tunnels at once, run under -race, must produce counts that add up.
func TestConcurrentTunnelsAreCountedExactly(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	const tunnels = 24
	var wg sync.WaitGroup
	for i := range tunnels {
		wg.Add(1)
		go func() {
			defer wg.Done()

			host := fmt.Sprintf("host-%d.test:443", i%4)
			conn, reader, resp := sendConnect(t, h.addr(), host, proxyCredentials(sessionToken))
			if resp.StatusCode != http.StatusOK {
				t.Errorf("CONNECT %s: status = %d, want 200", host, resp.StatusCode)
				return
			}
			if _, err := conn.Write([]byte("ping\n")); err != nil {
				t.Errorf("write to %s error = %v", host, err)
				return
			}
			if _, err := reader.ReadString('\n'); err != nil {
				t.Errorf("read from %s error = %v", host, err)
			}
			conn.Close()
		}()
	}
	wg.Wait()

	total := func() (tunnelCount, bytesTo, bytesFrom int64) {
		for _, session := range h.proxy.ForwardActivity().Sessions {
			for _, row := range session.Hosts {
				tunnelCount += row.Tunnels
				bytesTo += row.BytesToHost
				bytesFrom += row.BytesFromHost
			}
		}
		return
	}

	waitFor(t, "every tunnel's bytes", func() bool {
		_, bytesTo, bytesFrom := total()
		return bytesTo == tunnels*5 && bytesFrom == tunnels*10
	})
	if got, _, _ := total(); got != tunnels {
		t.Fatalf("tunnels recorded = %d, want %d", got, tunnels)
	}
	report := h.proxy.ForwardActivity()
	if len(report.Sessions) != 1 || len(report.Sessions[0].Hosts) != 4 {
		t.Fatalf("report = %+v, want one session with four hosts", report)
	}
	if report.Tracked != 4 || report.Dropped != 0 {
		t.Errorf("Tracked/Dropped = %d/%d, want 4/0", report.Tracked, report.Dropped)
	}
}

// At the cap the table stops adding rows, keeps counting the ones it has, says
// how many observations it lost - and does not interfere with the traffic.
func TestHostTableCapStopsGrowingAndSaysSo(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(target.Close)

	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), func(c *ForwardConfig) {
		c.MaxTrackedHosts = 2
	})
	proxyURL, err := url.Parse("http://saint:" + sessionToken + "@" + h.addr())
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	for i := range 5 {
		resp, err := client.Get(fmt.Sprintf("http://host-%d.test/", i))
		if err != nil {
			t.Fatalf("Get() through a full table error = %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200: a full table must not refuse traffic", i, resp.StatusCode)
		}
	}

	report := h.proxy.ForwardActivity()
	if report.Tracked != 2 || report.Max != 2 {
		t.Fatalf("Tracked/Max = %d/%d, want 2/2", report.Tracked, report.Max)
	}
	if report.Dropped != 3 {
		t.Fatalf("Dropped = %d, want 3: the report has to say what it stopped recording", report.Dropped)
	}
	if len(report.Sessions) != 1 || len(report.Sessions[0].Hosts) != 2 {
		t.Fatalf("report = %+v, want one session with the two rows it kept", report)
	}

	// The rows it kept keep counting.
	if got := report.Sessions[0].Hosts[0].Requests; got != 1 {
		t.Errorf("kept row Requests = %d, want 1", got)
	}
}

// The table is the only thing here that grows, so a session that ends has to be
// removable.
func TestForgetSessionDropsItsRows(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	conn, _, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	conn.Close()

	h.proxy.ForgetSession(sessionID)
	report := h.proxy.ForwardActivity()
	if len(report.Sessions) != 0 || report.Tracked != 0 {
		t.Fatalf("report after ForgetSession = %+v, want empty", report)
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

// Settings has to describe the forward posture as values, so a renderer can say
// what is on without this package deciding how.
func TestForwardSettingsReportThePosture(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.AllowAnonymous = true
		c.MaxTrackedHosts = 7
		c.Policy = &HostPolicy{Deny: []string{"zeta.test", "alpha.test"}}
	})

	forward := h.proxy.Settings().Forward
	if !forward.Enabled || !forward.AnonymousAllowed || forward.LoopbackTargetsAllowed {
		t.Errorf("Forward = %+v, want enabled, anonymous allowed, loopback refused", forward)
	}
	if forward.MaxTrackedHosts != 7 {
		t.Errorf("MaxTrackedHosts = %d, want 7", forward.MaxTrackedHosts)
	}
	want := []string{"alpha.test", "zeta.test"}
	if len(forward.DenyHosts) != len(want) {
		t.Fatalf("DenyHosts = %v, want %v", forward.DenyHosts, want)
	}
	for i := range want {
		if forward.DenyHosts[i] != want[i] {
			t.Fatalf("DenyHosts = %v, want %v sorted", forward.DenyHosts, want)
		}
	}
	if len(forward.AllowHosts) != 0 {
		t.Errorf("AllowHosts = %v, want empty, which means every host not denied", forward.AllowHosts)
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

// http.Server.Shutdown does not touch hijacked connections, so a tunnel
// outlives the server that created it unless the proxy closes it itself. The
// client has to see the tunnel end, and the tracking set has to drain.
func TestCloseTunnelsEndsLiveTunnelsAndDrainsTheSet(t *testing.T) {
	t.Parallel()

	// An httptest server that answers nothing keeps the far end of the tunnel
	// open and silent, which is the state a quit finds a long-lived agent
	// tunnel in.
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(target.Close)

	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), nil)

	conn, reader, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if got := h.proxy.forward.tunnels.len(); got != 1 {
		t.Fatalf("tracked tunnels = %d, want 1", got)
	}

	// Nothing is sent through the tunnel, so this read blocks until something
	// ends the connection. If the read returns before CloseTunnels is called,
	// the tunnel was never live and the rest of this test proves nothing.
	ended := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		ended <- err
	}()

	select {
	case err := <-ended:
		t.Fatalf("tunnel ended on its own before CloseTunnels: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}

	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("read through a closed tunnel returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client never saw the tunnel close")
	}

	waitFor(t, "the tunnel set to drain", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})

	// The bytes that did cross are still in the activity table: closing tunnels
	// is a shutdown step, not a way to forget what was observed.
	if row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "example.test"); row.Tunnels != 1 {
		t.Errorf("Tunnels = %d after CloseTunnels, want 1", row.Tunnels)
	}
	conn.Close()
}

// A CONNECT that arrives after the sweep must not become a live tunnel: it
// would be exactly the connection shutdown just guaranteed does not exist.
func TestConnectAfterCloseTunnelsDoesNotOpenATunnel(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}

	conn, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer conn.Close()

	request := "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n" +
		"Proxy-Authorization: " + proxyCredentials(sessionToken) + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT error = %v", err)
	}

	// The connection is abandoned before the 200 is written, so the client sees
	// the socket close rather than a tunnel that works for a moment.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		t.Fatal("the proxy answered a CONNECT after CloseTunnels")
	}
	if got := h.proxy.forward.tunnels.len(); got != 0 {
		t.Errorf("tracked tunnels = %d after a refused CONNECT, want 0", got)
	}
}

// A tunnel that ends on its own has to unregister itself, or the set is a leak
// with a different name.
func TestFinishedTunnelLeavesTheTrackingSet(t *testing.T) {
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
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read through tunnel error = %v", err)
	}
	conn.Close()

	waitFor(t, "the finished tunnel to unregister", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})
}

// Shutdown must be callable whatever state the proxy is in - twice, with no
// tunnels, with forwarding off, and on a zero value.
func TestCloseTunnelsIsSafeWithNothingToClose(t *testing.T) {
	t.Parallel()

	var zero *Proxy
	if err := zero.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() on a nil proxy error = %v", err)
	}

	noForward := newProxy(t, newUpstream(t, &recordedRequest{}), nil)
	if err := noForward.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() with forwarding off error = %v", err)
	}

	h := newForwardHarness(t, "", nil)
	if err := h.proxy.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() with no tunnels error = %v", err)
	}
	if err := h.proxy.CloseTunnels(); err != nil {
		t.Errorf("second CloseTunnels() error = %v", err)
	}
}

// Tunnels open while the sweep runs. Neither side may deadlock, and every
// tunnel that was accepted has to be gone afterwards.
func TestCloseTunnelsRacesNewTunnels(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(target.Close)
	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), nil)

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", h.addr())
			if err != nil {
				return
			}
			defer conn.Close()
			request := "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n" +
				"Proxy-Authorization: " + proxyCredentials(sessionToken) + "\r\n\r\n"
			// Every outcome here is legitimate - tunnelled then closed, or
			// refused because the sweep won the race - so nothing is asserted
			// about an individual connection. What is asserted is below.
			_, _ = conn.Write([]byte(request))
			_, _ = bufio.NewReader(conn).ReadByte()
		}()
	}

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}
	wg.Wait()

	waitFor(t, "every tunnel to be gone", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})
}
