package proxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A compressed response must still be metered.
//
// Go's transport decompresses only when it added Accept-Encoding itself. An
// agent that sets the header explicitly - Claude Code does - gets the gzip
// stream passed through untouched, and a meter reading raw bytes then finds no
// SSE frames at all: no model, no tokens, and every response marked partial
// because message_stop never arrives.
//
// Observed in production before this test existed: 177 consecutive streamed
// responses from a real session, all attributed, all reporting nothing.
func TestUsageIsMeteredThroughACompressedStream(t *testing.T) {
	t.Parallel()

	frames := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":120,"cache_read_input_tokens":4000}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":45}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(frames))
		_ = zw.Close()
	}))
	defer upstream.Close()

	got := make(chan UsageEvent, 4)
	cfg := testConfig(t, upstream, nil)
	cfg.OnUsage = func(ev UsageEvent) { got <- ev }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	front := httptest.NewServer(p)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)
	// The header that causes it: set by the client, so Go's transport leaves
	// the response compressed.
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("draining the response: %v", err)
	}

	select {
	case ev := <-got:
		if !ev.Usage.Reported {
			t.Fatal("a compressed stream was metered as reporting nothing")
		}
		if ev.Model != "claude-opus-5" {
			t.Errorf("model = %q, want it read from the compressed frames", ev.Model)
		}
		if ev.Usage.InputTokens != 120 || ev.Usage.OutputTokens != 45 {
			t.Errorf("tokens in/out = %d/%d, want 120/45", ev.Usage.InputTokens, ev.Usage.OutputTokens)
		}
		if ev.Usage.CacheReadTokens != 4000 {
			t.Errorf("cache read = %d, want 4000", ev.Usage.CacheReadTokens)
		}
		if ev.Partial {
			t.Error("a stream that reached message_stop was marked partial")
		}
	default:
		t.Fatal("no usage event was emitted at all")
	}
}

// A body the meter cannot decode reports nothing rather than guessing, and must
// still reach the guest untouched.
func TestAnUndecodableBodyReportsNothingAndStillReachesTheGuest(t *testing.T) {
	t.Parallel()

	const payload = "not-really-brotli-bytes"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// An encoding saint does not read.
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	got := make(chan UsageEvent, 4)
	cfg := testConfig(t, upstream, nil)
	cfg.OnUsage = func(ev UsageEvent) { got <- ev }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", sessionToken)
	req.Header.Set("Accept-Encoding", "br")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	// The guest's bytes are never altered: saint does not re-encode a response
	// on the agent's behalf.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if string(body) != payload {
		t.Errorf("the guest received %q, want the upstream bytes untouched", body)
	}

	select {
	case ev := <-got:
		if ev.Usage.Reported {
			t.Errorf("an undecodable body reported usage: %+v", ev.Usage)
		}
	default:
	}
}

// A gzip stream larger than the cap reports nothing rather than half an answer.
func TestAnOversizeEncodedBodyReportsNothing(t *testing.T) {
	t.Parallel()

	b := &meteredBody{scan: &sseScanner{}, encoding: "gzip"}
	chunk := make([]byte, 1<<20)
	for i := 0; i <= maxEncodedMeterBytes/len(chunk); i++ {
		b.observe(chunk)
	}
	if !b.overrun {
		t.Fatal("an oversize encoded body was still being held")
	}
	if b.held != nil {
		t.Error("an oversize encoded body kept its bytes")
	}
}

// Plaintext still scans straight through, with nothing held.
func TestAPlaintextBodyIsNotHeld(t *testing.T) {
	t.Parallel()

	b := &meteredBody{scan: &sseScanner{}}
	b.observe([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	if b.held != nil {
		t.Error("a plaintext body was held instead of scanned as it passed")
	}
}
