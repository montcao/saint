package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock drives the rate limiter without sleeping. A test that waited out a
// real refill window would take a minute and still be timing-dependent.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	// A fixed instant rather than time.Now(): the limiter treats a zero-valued
	// bucket timestamp as an enormous elapsed interval, and starting from a real
	// wall clock would hide that.
	return &fakeClock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countingUpstream records what actually arrived, safely. The shared
// recordedRequest in proxy_test.go is written from the upstream's goroutine
// with no lock, which is fine for a request the test has watched complete but
// not for one this file deliberately aborts mid-flight.
type countingUpstream struct {
	mu       sync.Mutex
	requests int
	lastBody string
}

func (u *countingUpstream) serve(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests++
		u.lastBody = string(body)
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server
}

func (u *countingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

func (u *countingUpstream) body() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastBody
}

// newLimitedProxy builds a proxy on the shared test wiring with the limit
// fields set by the caller.
func newLimitedProxy(t *testing.T, upstream *httptest.Server, mutate func(*Config)) *Proxy {
	t.Helper()

	cfg := testConfig(t, upstream, nil)
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

func postBody(path string, size int) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", size)))
	req.Header.Set("x-api-key", sessionToken)
	return req
}

// The cap has to hold at the exact boundary in both directions. A body one byte
// under it is ordinary agent traffic; a body one byte over it must come back as
// a 413 the guest can read rather than a truncated request the upstream answers
// as though it were whole.
func TestRequestBodySizeLimit(t *testing.T) {
	t.Parallel()

	const limit = 1024

	tests := []struct {
		name         string
		configured   int64
		size         int
		wantStatus   int
		wantUpstream int
	}{
		{name: "well under the cap", configured: limit, size: 1, wantStatus: http.StatusOK, wantUpstream: 1},
		{name: "one byte under the cap", configured: limit, size: limit - 1, wantStatus: http.StatusOK, wantUpstream: 1},
		{name: "exactly at the cap", configured: limit, size: limit, wantStatus: http.StatusOK, wantUpstream: 1},
		{name: "one byte over the cap", configured: limit, size: limit + 1, wantStatus: http.StatusRequestEntityTooLarge, wantUpstream: 0},
		{name: "far over the cap", configured: limit, size: limit * 64, wantStatus: http.StatusRequestEntityTooLarge, wantUpstream: 0},

		// Zero is the default, not a cap of zero bytes. An upgrade must not
		// start refusing bodies nobody said anything about.
		{name: "unset config takes the default", configured: 0, size: 2 << 20, wantStatus: http.StatusOK, wantUpstream: 1},

		// The opt-out has to actually opt out.
		{name: "unlimited passes a body over the default", configured: UnlimitedRequestBytes, size: 1 << 10, wantStatus: http.StatusOK, wantUpstream: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := &countingUpstream{}
			var keyFetched atomic.Bool
			p := newLimitedProxy(t, upstream.serve(t), func(c *Config) {
				c.MaxRequestBytes = tc.configured
				c.AnthropicKey = func(context.Context) (string, error) {
					keyFetched.Store(true)
					return anthropicKey, nil
				}
			})

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, postBody("/v1/messages", tc.size))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := upstream.count(); got != tc.wantUpstream {
				t.Fatalf("upstream requests = %d, want %d", got, tc.wantUpstream)
			}
			if tc.wantStatus == http.StatusOK {
				if got := len(upstream.body()); got != tc.size {
					t.Fatalf("upstream body = %d bytes, want %d", got, tc.size)
				}
				return
			}
			// A refused request must not have reached the credential. The whole
			// point of the cap is that an unauthorised or runaway guest gets no
			// use of the user's key.
			if keyFetched.Load() {
				t.Fatal("a refused request still fetched the provider key")
			}
		})
	}
}

// A chunked request declares no length, so the cap can only be enforced while
// the bytes are already moving. That path has to end in the same clear 413 and
// must not leave a truncated body upstream to be answered as a whole one.
func TestOversizedChunkedBodyIsRefusedMidRequest(t *testing.T) {
	t.Parallel()

	const limit = 1024

	upstream := &countingUpstream{}
	var logged []string
	p := newLimitedProxy(t, upstream.serve(t), func(c *Config) {
		c.MaxRequestBytes = limit
		c.Logf = func(format string, args ...any) { logged = append(logged, sprintf(format, args...)) }
	})

	req := postBody("/v1/messages", limit*8)
	// What a client sending without a known length looks like to the server.
	req.ContentLength = -1

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if got := upstream.body(); len(got) > limit {
		t.Fatalf("upstream received %d bytes, more than the %d byte cap", len(got), limit)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "mid-request") {
		t.Fatalf("the mid-request refusal was not logged: %v", logged)
	}
}

// limitedBody is where the boundary is actually decided, so it is worth testing
// away from the transport: a reader that hands over one byte at a time must
// reach the same verdict as one that hands over the whole body at once.
func TestLimitedBodyBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cap     int64
		size    int
		bufSize int
		wantErr bool
	}{
		{name: "under", cap: 8, size: 4, bufSize: 64},
		{name: "exact", cap: 8, size: 8, bufSize: 64},
		{name: "over by one", cap: 8, size: 9, bufSize: 64, wantErr: true},
		{name: "exact byte at a time", cap: 8, size: 8, bufSize: 1},
		{name: "over by one byte at a time", cap: 8, size: 9, bufSize: 1, wantErr: true},
		{name: "empty body", cap: 0, size: 0, bufSize: 64},
		{name: "any byte over a zero cap", cap: 0, size: 1, bufSize: 64, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := &limitedBody{
				src:       io.NopCloser(strings.NewReader(strings.Repeat("x", tc.size))),
				remaining: tc.cap,
			}
			var read int
			var err error
			buf := make([]byte, tc.bufSize)
			for {
				var n int
				n, err = body.Read(buf)
				read += n
				if err != nil {
					break
				}
			}

			if tc.wantErr {
				if err != errRequestTooLarge {
					t.Fatalf("error = %v, want errRequestTooLarge", err)
				}
				if !body.tooLarge() {
					t.Fatal("tooLarge() = false after the cap was passed")
				}
				return
			}
			if err != io.EOF {
				t.Fatalf("error = %v, want io.EOF", err)
			}
			if read != tc.size {
				t.Fatalf("read %d bytes, want %d", read, tc.size)
			}
			if body.tooLarge() {
				t.Fatal("tooLarge() = true for a body inside the cap")
			}
		})
	}
}

// The ceiling is a count per minute, so the request that matters is the one
// after the last allowed one. Zero has to stay unlimited: it is what every
// existing install has configured.
func TestRateLimitBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		perMinute   int
		requests    int
		wantAllowed int
	}{
		{name: "off by default", perMinute: 0, requests: 50, wantAllowed: 50},
		{name: "negative is also off", perMinute: -1, requests: 20, wantAllowed: 20},
		{name: "one per minute", perMinute: 1, requests: 3, wantAllowed: 1},
		{name: "at the ceiling", perMinute: 3, requests: 3, wantAllowed: 3},
		{name: "one over the ceiling", perMinute: 3, requests: 4, wantAllowed: 3},
		{name: "well over the ceiling", perMinute: 5, requests: 40, wantAllowed: 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := &countingUpstream{}
			p := newLimitedProxy(t, upstream.serve(t), func(c *Config) {
				c.RequestsPerMinute = tc.perMinute
			})
			clock := newFakeClock()
			if p.limiter != nil {
				p.limiter.now = clock.now
			}

			allowed := 0
			for i := range tc.requests {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, postBody("/v1/messages", 8))
				switch rec.Code {
				case http.StatusOK:
					allowed++
				case http.StatusTooManyRequests:
					retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
					if err != nil {
						t.Fatalf("request %d: Retry-After = %q, want whole seconds", i, rec.Header().Get("Retry-After"))
					}
					if retry < 1 {
						t.Fatalf("request %d: Retry-After = %d, want at least 1", i, retry)
					}
				default:
					t.Fatalf("request %d: status = %d, want 200 or 429", i, rec.Code)
				}
			}

			if allowed != tc.wantAllowed {
				t.Fatalf("allowed = %d, want %d", allowed, tc.wantAllowed)
			}
			if got := upstream.count(); got != tc.wantAllowed {
				t.Fatalf("upstream requests = %d, want %d", got, tc.wantAllowed)
			}
		})
	}
}

// A refusal has to expire on its own. A limiter that never refills would take a
// session out for the rest of its life over one burst.
func TestRateLimitRefillsOverTime(t *testing.T) {
	t.Parallel()

	upstream := &countingUpstream{}
	p := newLimitedProxy(t, upstream.serve(t), func(c *Config) { c.RequestsPerMinute = 2 })
	clock := newFakeClock()
	p.limiter.now = clock.now

	serve := func() int {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, postBody("/v1/messages", 8))
		return rec.Code
	}

	for i := range 2 {
		if code := serve(); code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}
	if code := serve(); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the budget is spent", code)
	}

	// Half a window at two per minute is one token: exactly one more request.
	clock.advance(30 * time.Second)
	if code := serve(); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a partial refill", code)
	}
	if code := serve(); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 with the refill spent", code)
	}

	clock.advance(time.Minute)
	for i := range 2 {
		if code := serve(); code != http.StatusOK {
			t.Fatalf("request %d after a full window: status = %d, want 200", i, code)
		}
	}
}

// The ceiling is per session, so one noisy guest must not be able to refuse
// service to another. Both are on the same bridge and neither trusts the other.
func TestRateLimitIsPerSession(t *testing.T) {
	t.Parallel()

	const otherToken = "session-token-fedcba9876543210"

	upstream := &countingUpstream{}
	p := newLimitedProxy(t, upstream.serve(t), func(c *Config) {
		c.RequestsPerMinute = 1
		c.TokenLookup = func(token string) (string, bool) {
			switch token {
			case sessionToken:
				return sessionID, true
			case otherToken:
				return "other-session", true
			}
			return "", false
		}
	})
	clock := newFakeClock()
	p.limiter.now = clock.now

	serve := func(token string) int {
		req := postBody("/v1/messages", 8)
		req.Header.Set("x-api-key", token)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := serve(sessionToken); code != http.StatusOK {
		t.Fatalf("first session: status = %d, want 200", code)
	}
	if code := serve(sessionToken); code != http.StatusTooManyRequests {
		t.Fatalf("first session: status = %d, want 429", code)
	}
	if code := serve(otherToken); code != http.StatusOK {
		t.Fatalf("second session: status = %d, want 200 - it has spent nothing", code)
	}
}

// ServeHTTP runs on one goroutine per request and the limiter is shared state.
// Under -race this catches a limiter that is merely usually right; the exact
// counts catch one that double-spends or leaks tokens under contention.
func TestRateLimitedProxyIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	const (
		perMinute   = 50
		goroutines  = 8
		perRoutine  = 25
		totalCalls  = goroutines * perRoutine
		wantRefused = totalCalls - perMinute
	)

	upstream := &countingUpstream{}
	p := newLimitedProxy(t, upstream.serve(t), func(c *Config) {
		c.RequestsPerMinute = perMinute
		c.MaxRequestBytes = 4096
	})
	// Frozen: with no refill mid-test the allowed count is exact rather than a
	// range, so a lost or duplicated token cannot pass as timing.
	clock := newFakeClock()
	p.limiter.now = clock.now

	var allowed, refused atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perRoutine {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, postBody("/v1/messages", 64))
				switch rec.Code {
				case http.StatusOK:
					allowed.Add(1)
				case http.StatusTooManyRequests:
					refused.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != perMinute {
		t.Fatalf("allowed = %d, want exactly %d", got, perMinute)
	}
	if got := refused.Load(); got != wantRefused {
		t.Fatalf("refused = %d, want %d", got, wantRefused)
	}
	if got := upstream.count(); got != perMinute {
		t.Fatalf("upstream requests = %d, want %d", got, perMinute)
	}
}

// The limiter keys on session IDs it does not own, so its map has to give
// memory back. Only buckets that have refilled completely are dropped, which is
// why a dropped one cannot hand anybody free budget.
func TestRateLimiterForgetsIdleSessions(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	limiter := newRateLimiter(60, clock.now)

	if _, ok := limiter.allow("gone-quiet"); !ok {
		t.Fatal("allow() = false on an unseen session")
	}
	clock.advance(idleBucketTTL + sweepInterval)
	if _, ok := limiter.allow("still-here"); !ok {
		t.Fatal("allow() = false on a second unseen session")
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, found := limiter.buckets["gone-quiet"]; found {
		t.Fatal("an idle session's bucket was never reclaimed")
	}
	if _, found := limiter.buckets["still-here"]; !found {
		t.Fatal("the active session's bucket was swept")
	}
}

// The limits wrap the request body, and nothing in this file may touch the
// response. If a limit ever buffered a response, every agent would look hung
// for the length of a generation - the failure streaming_test.go exists to
// catch, reachable again through a control added later.
func TestLimitedProxyStreamsServerSentEventsIncrementally(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}

		_, _ = w.Write([]byte("event: message_start\ndata: {\"first\":true}\n\n"))
		flusher.Flush()

		select {
		case <-release:
		case <-time.After(streamWaitTimeout):
			t.Error("timed out waiting for the client to read the first chunk")
			return
		}

		_, _ = w.Write([]byte("event: message_stop\ndata: {\"last\":true}\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	p := newLimitedProxy(t, upstream, func(c *Config) {
		c.MaxRequestBytes = 4096
		c.RequestsPerMinute = 60
	})
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := readEvent(reader)
	if err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}
	if !strings.Contains(first, "message_start") {
		t.Fatalf("first chunk = %q, want the message_start event", first)
	}

	// Proof the first event reached the client before the upstream wrote the
	// second one, with both limits in force.
	close(release)

	second, err := readEvent(reader)
	if err != nil {
		t.Fatalf("reading second chunk: %v", err)
	}
	if !strings.Contains(second, "message_stop") {
		t.Fatalf("second chunk = %q, want the message_stop event", second)
	}
}

// Settings has to report what the guest will actually meet, not what the config
// literally said: an unset field is a default in force, not an absent control.
func TestSettingsReportsEffectiveLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*Config)
		wantBody Limit
		wantRate Limit
	}{
		{
			name:     "defaults",
			wantBody: Limit{Value: DefaultMaxRequestBytes, Enforced: true, Default: true},
			wantRate: Limit{Default: true},
		},
		{
			name:     "configured limits",
			mutate:   func(c *Config) { c.MaxRequestBytes = 4096; c.RequestsPerMinute = 120 },
			wantBody: Limit{Value: 4096, Enforced: true},
			wantRate: Limit{Value: 120, Enforced: true},
		},
		{
			name:     "body cap switched off",
			mutate:   func(c *Config) { c.MaxRequestBytes = UnlimitedRequestBytes },
			wantBody: Limit{},
			wantRate: Limit{Default: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := &countingUpstream{}
			p := newLimitedProxy(t, upstream.serve(t), tc.mutate)

			got := p.Settings()
			if got.MaxRequestBytes != tc.wantBody {
				t.Errorf("MaxRequestBytes = %+v, want %+v", got.MaxRequestBytes, tc.wantBody)
			}
			if got.RequestsPerMinute != tc.wantRate {
				t.Errorf("RequestsPerMinute = %+v, want %+v", got.RequestsPerMinute, tc.wantRate)
			}
			want := []string{"api.anthropic.com", "api.openai.com"}
			if len(got.UpstreamHosts) != len(want) {
				t.Fatalf("UpstreamHosts = %v, want %v", got.UpstreamHosts, want)
			}
			for i := range want {
				if got.UpstreamHosts[i] != want[i] {
					t.Fatalf("UpstreamHosts = %v, want %v", got.UpstreamHosts, want)
				}
			}
		})
	}
}

// A Proxy that was never handed limit configuration still has to behave. The
// zero value must mean the default cap and no ceiling, never a cap of zero
// bytes that refuses every request with a body.
func TestZeroValueProxyEnforcesDefaultsNotDenial(t *testing.T) {
	t.Parallel()

	p := &Proxy{}
	if got := p.maxRequestBytes(); got != DefaultMaxRequestBytes {
		t.Fatalf("maxRequestBytes() = %d, want the default %d", got, DefaultMaxRequestBytes)
	}
	if _, ok := p.limiter.allow(sessionID); !ok {
		t.Fatal("a nil limiter refused a request")
	}

	rec := httptest.NewRecorder()
	limited, ok := p.enforceLimits(rec, postBody("/v1/messages", 4096), sessionID)
	if !ok {
		t.Fatalf("enforceLimits() refused an ordinary request: status = %d", rec.Code)
	}
	if limited == nil {
		t.Fatal("the default cap left the body unwrapped")
	}
}
