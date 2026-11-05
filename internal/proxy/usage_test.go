package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// usageCollector gathers events from OnUsage, which the proxy invokes on the
// goroutine copying a response - concurrently across requests.
type usageCollector struct {
	mu     sync.Mutex
	events []UsageEvent
}

func (c *usageCollector) record(ev UsageEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *usageCollector) all() []UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]UsageEvent(nil), c.events...)
}

// waitFor polls until at least n events have arrived. The event is emitted when
// the proxy drains the upstream body, which can happen after the client's last
// read returns, so an unsynchronised assertion would be flaky.
func (c *usageCollector) waitFor(t *testing.T, n int) []UsageEvent {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got := c.all()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d usage events; got %d", n, len(got))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// expectNone gives any stray event a chance to arrive before asserting silence.
func (c *usageCollector) expectNone(t *testing.T) {
	t.Helper()

	time.Sleep(50 * time.Millisecond)
	if got := c.all(); len(got) != 0 {
		t.Fatalf("usage events = %+v, want none", got)
	}
}

func newMeteredProxy(t *testing.T, upstream *httptest.Server, onUsage func(UsageEvent)) *Proxy {
	t.Helper()

	cfg := testConfig(t, upstream, nil)
	cfg.OnUsage = onUsage
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

// bodyUpstream serves one fixed body with one fixed content type.
func bodyUpstream(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// get drives one authenticated request through the proxy and returns the body.
func get(t *testing.T, p *Proxy, path string) (*http.Response, string) {
	t.Helper()

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return resp, string(body)
}

const nonStreamingBody = `{
  "id": "msg_1",
  "model": "claude-opus-5",
  "content": [{"type": "text", "text": "hello"}],
  "usage": {
    "input_tokens": 1200,
    "output_tokens": 340,
    "cache_creation_input_tokens": 64,
    "cache_read_input_tokens": 900
  }
}`

func TestUsageFromNonStreamingResponse(t *testing.T) {
	t.Parallel()

	var collector usageCollector
	upstream := bodyUpstream(t, "application/json", nonStreamingBody)
	p := newMeteredProxy(t, upstream, collector.record)

	_, body := get(t, p, "/v1/messages")
	if !strings.Contains(body, "hello") {
		t.Fatalf("body = %q, want the upstream content", body)
	}

	ev := collector.waitFor(t, 1)[0]
	if ev.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, sessionID)
	}
	if ev.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", ev.Provider)
	}
	if ev.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", ev.Model)
	}
	if ev.Stream {
		t.Error("Stream = true, want false for a plain JSON response")
	}
	if ev.Partial {
		t.Error("Partial = true, want false for a complete body")
	}
	want := Usage{InputTokens: 1200, OutputTokens: 340, CacheCreationTokens: 64, CacheReadTokens: 900, Reported: true}
	if ev.Usage != want {
		t.Errorf("Usage = %+v, want %+v", ev.Usage, want)
	}
}

// streamBody is the shape Claude Code actually receives: input tokens arrive in
// message_start, output tokens accumulate across message_delta frames, and the
// final delta carries the real total.
const streamBody = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":1200,"output_tokens":1,"cache_creation_input_tokens":64,"cache_read_input_tokens":900}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":10}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":340}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestUsageAssembledFromStreamedEvents(t *testing.T) {
	t.Parallel()

	var collector usageCollector
	upstream := bodyUpstream(t, "text/event-stream", streamBody)
	p := newMeteredProxy(t, upstream, collector.record)

	_, body := get(t, p, "/v1/messages")
	if body != streamBody {
		t.Fatalf("forwarded body differs from upstream")
	}

	ev := collector.waitFor(t, 1)[0]
	if !ev.Stream {
		t.Error("Stream = false, want true")
	}
	if ev.Partial {
		t.Error("Partial = true, want false: the stream reached message_stop")
	}
	if ev.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", ev.Model)
	}
	// The last message_delta wins for output; message_start's input survives it.
	want := Usage{InputTokens: 1200, OutputTokens: 340, CacheCreationTokens: 64, CacheReadTokens: 900, Reported: true}
	if ev.Usage != want {
		t.Errorf("Usage = %+v, want %+v", ev.Usage, want)
	}
}

// A stream that stops before message_stop still spent tokens. Reporting nothing
// would silently under-count every cancelled generation.
func TestTruncatedStreamReportsPartialUsage(t *testing.T) {
	t.Parallel()

	truncated := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":90}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"

	var collector usageCollector
	upstream := bodyUpstream(t, "text/event-stream", truncated)
	p := newMeteredProxy(t, upstream, collector.record)

	get(t, p, "/v1/messages")

	ev := collector.waitFor(t, 1)[0]
	if !ev.Partial {
		t.Error("Partial = false, want true: the stream never reached message_stop")
	}
	if ev.Usage.InputTokens != 90 || ev.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want input 90 and output 7", ev.Usage)
	}
}

// OpenAI terminates with a sentinel rather than a typed event.
func TestOpenAIStreamSentinelCompletesTheStream(t *testing.T) {
	t.Parallel()

	body := `data: {"model":"gpt-x","usage":{"prompt_tokens":11,"completion_tokens":22}}` + "\n\n" +
		"data: [DONE]\n\n"

	var collector usageCollector
	upstream := bodyUpstream(t, "text/event-stream", body)
	p := newMeteredProxy(t, upstream, collector.record)

	get(t, p, "/v1/chat/completions")

	ev := collector.waitFor(t, 1)[0]
	if ev.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", ev.Provider)
	}
	if ev.Partial {
		t.Error("Partial = true, want false: the stream reached [DONE]")
	}
	if ev.Usage.InputTokens != 11 || ev.Usage.OutputTokens != 22 {
		t.Errorf("Usage = %+v, want input 11 and output 22", ev.Usage)
	}
}

// A response with no usage block must not be reported as a measured zero: the
// UI has to be able to say "unknown" instead of a confident 0.
func TestUsageAbsentIsNotReportedAsZero(t *testing.T) {
	t.Parallel()

	var collector usageCollector
	upstream := bodyUpstream(t, "application/json", `{"model":"claude-opus-5","content":[]}`)
	p := newMeteredProxy(t, upstream, collector.record)

	get(t, p, "/v1/messages")

	ev := collector.waitFor(t, 1)[0]
	if ev.Usage.Reported {
		t.Errorf("Reported = true, want false: the upstream sent no usage block")
	}
	if ev.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", ev.Model)
	}
}

func TestMalformedBodyIsNotMetered(t *testing.T) {
	t.Parallel()

	const broken = `{"model":"claude-opus-5","usage":{`

	var collector usageCollector
	upstream := bodyUpstream(t, "application/json", broken)
	p := newMeteredProxy(t, upstream, collector.record)

	resp, body := get(t, p, "/v1/messages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: metering must not change the response", resp.StatusCode)
	}
	if body != broken {
		t.Fatalf("body = %q, want the upstream body forwarded unchanged", body)
	}
	collector.expectNone(t)
}

func TestOversizeBodyIsNotMetered(t *testing.T) {
	t.Parallel()

	// Valid JSON with real usage, but past the metering cap.
	filler := strings.Repeat("a", maxJSONMeterBytes+1024)
	big := fmt.Sprintf(`{"model":"claude-opus-5","usage":{"input_tokens":5},"text":%q}`, filler)

	var collector usageCollector
	upstream := bodyUpstream(t, "application/json", big)
	p := newMeteredProxy(t, upstream, collector.record)

	resp, body := get(t, p, "/v1/messages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(body) != len(big) {
		t.Fatalf("body length = %d, want %d: the whole body must still reach the guest", len(body), len(big))
	}
	collector.expectNone(t)
}

func TestUnmeteredContentTypePassesThrough(t *testing.T) {
	t.Parallel()

	var collector usageCollector
	upstream := bodyUpstream(t, "text/plain", "not json")
	p := newMeteredProxy(t, upstream, collector.record)

	_, body := get(t, p, "/v1/messages")
	if body != "not json" {
		t.Fatalf("body = %q, want it forwarded unchanged", body)
	}
	collector.expectNone(t)
}

// Metering is observation. What the guest receives must be byte-identical to
// what the upstream sent, in every mode.
func TestMeteringDoesNotAlterForwardedBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "json", contentType: "application/json", body: nonStreamingBody},
		{name: "sse", contentType: "text/event-stream", body: streamBody},
		{name: "plain", contentType: "text/plain", body: "plain body\nwith newlines\r\nand \x00 bytes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var collector usageCollector
			upstream := bodyUpstream(t, tc.contentType, tc.body)

			metered := newMeteredProxy(t, upstream, collector.record)
			_, withTap := get(t, metered, "/v1/messages")

			plain := newProxy(t, upstream, nil)
			_, withoutTap := get(t, plain, "/v1/messages")

			if sum(withTap) != sum(tc.body) {
				t.Errorf("metered body checksum differs from upstream")
			}
			if withTap != withoutTap {
				t.Errorf("metered body differs from unmetered body")
			}
		})
	}
}

func sum(s string) string {
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
}

// The callback is host code from another package. A panic there must not take
// down an agent's in-flight request.
func TestUsageCallbackPanicDoesNotBreakTheRequest(t *testing.T) {
	t.Parallel()

	var logged []string
	var mu sync.Mutex
	upstream := bodyUpstream(t, "application/json", nonStreamingBody)

	cfg := testConfig(t, upstream, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	cfg.OnUsage = func(UsageEvent) { panic("callback exploded") }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	resp, body := get(t, p, "/v1/messages")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != nonStreamingBody {
		t.Fatalf("body was truncated by the panicking callback")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		found := strings.Contains(strings.Join(logged, "\n"), "usage callback panicked")
		mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("panic was swallowed without a log line")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A guest that hangs up mid-generation must not panic the proxy or leave the
// response goroutine wedged.
func TestClientDisconnectMidStreamIsSafe(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":42}}}`+"\n\n")
		flusher.Flush()

		select {
		case <-released:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			t.Error("upstream was never released")
		}
	}))
	t.Cleanup(func() {
		close(released)
		upstream.Close()
	})

	var collector usageCollector
	p := newMeteredProxy(t, upstream, collector.record)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("Do() error = %v", err)
	}

	buf := make([]byte, 32)
	if _, err := resp.Body.Read(buf); err != nil {
		cancel()
		t.Fatalf("reading the first bytes: %v", err)
	}
	cancel()
	resp.Body.Close()

	// Whatever the proxy reports, it must be marked partial - the generation
	// never finished.
	for _, ev := range collector.waitFor(t, 1) {
		if !ev.Partial {
			t.Errorf("Partial = false, want true after a client disconnect: %+v", ev)
		}
	}
}

// OnUsage is called from per-request goroutines. Run under -race.
func TestUsageMeteringUnderConcurrentRequests(t *testing.T) {
	t.Parallel()

	const requests = 20

	var collector usageCollector
	upstream := bodyUpstream(t, "application/json", nonStreamingBody)
	p := newMeteredProxy(t, upstream, collector.record)

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("x-api-key", sessionToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	wg.Wait()

	events := collector.waitFor(t, requests)
	for _, ev := range events {
		if ev.Usage.InputTokens != 1200 {
			t.Fatalf("Usage = %+v, want input 1200 on every request", ev.Usage)
		}
	}
}
