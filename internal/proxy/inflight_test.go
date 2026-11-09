package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// inFlightRecorder is the host side of Config.OnRequestStart, kept the way the
// usage meter keeps it: a live count that has to come back to zero, and a
// high-water mark so a test can tell "never counted" from "counted and
// balanced".
type inFlightRecorder struct {
	mu     sync.Mutex
	live   int
	peak   int
	starts int
	ends   int
	ids    []string
}

func (r *inFlightRecorder) hook(sessionID string) func() {
	r.mu.Lock()
	r.starts++
	r.live++
	if r.live > r.peak {
		r.peak = r.live
	}
	r.ids = append(r.ids, sessionID)
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ends++
		r.live--
	}
}

func (r *inFlightRecorder) read() (live, peak, starts, ends int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live, r.peak, r.starts, r.ends
}

func (r *inFlightRecorder) sessions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// The hook has to bracket the request: counted while the upstream is still
// answering, and back to zero once the response is done. Observing it from the
// upstream handler is the only way to be sure the count is live rather than
// something that happens to balance at the end.
func TestOnRequestStartBracketsAnAttributedRequest(t *testing.T) {
	t.Parallel()

	rec := &inFlightRecorder{}
	var duringUpstream int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duringUpstream, _, _, _ = rec.read()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := testConfig(t, upstream, nil)
	cfg.OnRequestStart = rec.hook
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
	req.Header.Set("x-api-key", sessionToken)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if duringUpstream != 1 {
		t.Errorf("in flight while the upstream answered = %d, want 1", duringUpstream)
	}

	live, _, starts, ends := rec.read()
	if starts != 1 || ends != 1 {
		t.Errorf("starts/ends = %d/%d, want 1/1", starts, ends)
	}
	if live != 0 {
		t.Errorf("in flight after the request = %d, want 0", live)
	}
	if got := rec.sessions(); len(got) != 1 || got[0] != sessionID {
		t.Errorf("attributed to %v, want [%s]", got, sessionID)
	}
}

// An unauthenticated request belongs to nobody. Counting it would attribute a
// stranger's traffic to a real session, which is exactly the invented number
// this codebase refuses to show.
func TestUnauthenticatedRequestIsNeverAttributed(t *testing.T) {
	t.Parallel()

	rec := &inFlightRecorder{}
	cfg := testConfig(t, newUpstream(t, &recordedRequest{}), nil)
	cfg.OnRequestStart = rec.hook
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
	req.Header.Set("x-api-key", "not-the-token")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, req)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if _, _, starts, _ := rec.read(); starts != 0 {
		t.Errorf("starts = %d, want 0: an unattributed request was counted against a session", starts)
	}
}

// Every exit after authentication has to decrement, not only the happy one. A
// leaked counter is a session that reads "working" for the rest of the process,
// and no later request corrects it.
func TestEveryExitPathEndsTheRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*Config)
		path       string
		wantStatus int
	}{
		{
			name:       "unrouted path",
			path:       "/v1/nothing-here",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "key lookup failure",
			mutate: func(c *Config) {
				c.AnthropicKey = func(context.Context) (string, error) { return "", errors.New("keychain is locked") }
			},
			path:       "/v1/messages",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "upstream transport failure",
			mutate:     func(c *Config) { c.Transport = failingTransport{} },
			path:       "/v1/messages",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "refused by the rate limit",
			mutate:     func(c *Config) { c.RequestsPerMinute = 1 },
			path:       "/v1/messages",
			wantStatus: http.StatusTooManyRequests,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := &inFlightRecorder{}
			cfg := testConfig(t, newUpstream(t, &recordedRequest{}), nil)
			cfg.OnRequestStart = rec.hook
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			p, err := New(cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			// The rate-limit case needs a request through first, so the one
			// under test is the one that is refused.
			attempts := 1
			if tc.wantStatus == http.StatusTooManyRequests {
				attempts = 2
			}

			var response *httptest.ResponseRecorder
			for range attempts {
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("payload"))
				req.Header.Set("x-api-key", sessionToken)
				response = httptest.NewRecorder()
				p.ServeHTTP(response, req)
			}

			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tc.wantStatus)
			}

			live, peak, starts, ends := rec.read()
			if starts != attempts {
				t.Errorf("starts = %d, want %d", starts, attempts)
			}
			if peak < 1 {
				t.Error("peak in flight = 0, want at least 1: the request was never counted")
			}
			if live != 0 || ends != starts {
				t.Errorf("in flight = %d and ends = %d after %d requests, want 0 and %d", live, ends, starts, starts)
			}
		})
	}
}

// The hook belongs to another package. A panic in it must not take down the
// guest's request, and must not be able to leave the counter raised either.
func TestPanickingRequestHookDoesNotBreakTheRequest(t *testing.T) {
	t.Parallel()

	var logged []string
	cfg := testConfig(t, newUpstream(t, &recordedRequest{}), func(format string, args ...any) {
		logged = append(logged, format)
	})
	cfg.OnRequestStart = func(string) func() { panic("host callback is broken") }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
	req.Header.Set("x-api-key", sessionToken)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a broken host hook failed a good request", response.Code)
	}
	if len(logged) == 0 {
		t.Error("nothing was logged about the panicking callback")
	}
}

// A hook that hands back nothing is a no-op, not a nil dereference on the
// deferred call.
func TestRequestHookReturningNilDoneIsSafe(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t, newUpstream(t, &recordedRequest{}), nil)
	cfg.OnRequestStart = func(string) func() { return nil }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
	req.Header.Set("x-api-key", sessionToken)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

// Requests from several sessions overlap on a real proxy. The count has to come
// back to exactly zero, and the peak has to show they really did overlap -
// otherwise this test would pass on a proxy that never counted anything.
func TestInFlightCountBalancesUnderConcurrentRequests(t *testing.T) {
	t.Parallel()

	rec := &inFlightRecorder{}
	release := make(chan struct{})
	arrived := make(chan struct{}, 16)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	cfg := testConfig(t, upstream, nil)
	cfg.OnRequestStart = rec.hook
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const concurrent = 16
	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
			req.Header.Set("x-api-key", sessionToken)
			p.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}

	// Every request is now parked inside the upstream handler, so they are all
	// genuinely in flight at the same moment.
	for range concurrent {
		<-arrived
	}
	if live, _, _, _ := rec.read(); live != concurrent {
		t.Errorf("in flight with %d parked requests = %d, want %d", concurrent, live, concurrent)
	}

	close(release)
	wg.Wait()

	live, peak, starts, ends := rec.read()
	if starts != concurrent || ends != concurrent {
		t.Errorf("starts/ends = %d/%d, want %d/%d", starts, ends, concurrent, concurrent)
	}
	if peak != concurrent {
		t.Errorf("peak in flight = %d, want %d", peak, concurrent)
	}
	if live != 0 {
		t.Errorf("in flight after every request finished = %d, want 0", live)
	}
}

// A CONNECT tunnel stays open for its whole lifetime and says nothing about
// whether the agent is thinking, so it is deliberately not counted. Counting it
// would pin a session at "working" until it exits.
func TestForwardTunnelIsNotCountedAsInFlight(t *testing.T) {
	t.Parallel()

	rec := &inFlightRecorder{}
	echo := echoServer(t)

	cfg := testConfig(t, newUpstream(t, &recordedRequest{}), nil)
	cfg.OnRequestStart = rec.hook
	cfg.Forward = &ForwardConfig{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	conn, reader, resp := sendConnect(t, strings.TrimPrefix(front.URL, "http://"), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through tunnel error = %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read through tunnel error = %v", err)
	}

	if _, _, starts, _ := rec.read(); starts != 0 {
		t.Errorf("starts = %d, want 0: an open tunnel was counted as a working session", starts)
	}
	conn.Close()
}

// failingTransport stands in for an upstream that cannot be reached, which
// sends the reverse proxy down its ErrorHandler.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("upstream is unreachable")
}
