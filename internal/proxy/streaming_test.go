package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamWaitTimeout bounds how long the upstream waits for the client to read
// the first chunk.
//
// Generous on purpose. The assertion is that output is not buffered, and a
// buffered proxy blocks here forever rather than taking a few seconds - so a
// tight bound buys nothing and makes the test flaky under -race with the rest
// of the suite running alongside it.
const streamWaitTimeout = 20 * time.Second

// SSE is the primary traffic shape through this proxy: Claude Code streams every
// response. If the reverse proxy buffered instead of flushing, the agent would
// appear to hang for the whole generation and then dump everything at once - a
// failure that no existing test would catch.
func TestProxyStreamsServerSentEventsIncrementally(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}

		_, _ = w.Write([]byte("event: message_start\ndata: {\"first\":true}\n\n"))
		flusher.Flush()

		// Block until the test has actually read the first chunk. If anything in
		// the chain buffers, the read below times out and this never returns.
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

	p := newProxy(t, upstream, nil)
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
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := readEvent(reader)
	if err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}
	if !strings.Contains(first, "message_start") {
		t.Fatalf("first chunk = %q, want the message_start event", first)
	}

	// Proof the first event arrived before the upstream wrote the second one.
	close(release)

	second, err := readEvent(reader)
	if err != nil {
		t.Fatalf("reading second chunk: %v", err)
	}
	if !strings.Contains(second, "message_stop") {
		t.Fatalf("second chunk = %q, want the message_stop event", second)
	}
}

// readEvent reads up to the blank line that terminates one SSE event.
// Usage metering wraps the response body. If that wrapper batched reads instead
// of passing them straight through, SSE would stop being live and every agent
// would look hung for the length of a generation - the exact failure the test
// above exists to catch, reintroduced through the back door.
func TestMeteredServerSentEventsStayIncremental(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}

		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":1200}}}` + "\n\n"))
		flusher.Flush()

		select {
		case <-release:
		case <-time.After(streamWaitTimeout):
			t.Error("timed out waiting for the client to read the first chunk")
			return
		}

		_, _ = w.Write([]byte("event: message_delta\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":340}}` + "\n\n" +
			"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	var collector usageCollector
	p := newMeteredProxy(t, upstream, collector.record)
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

	reader := bufio.NewReader(resp.Body)
	// This read only returns if the metered body forwarded the first chunk
	// before the upstream sent anything more.
	first, err := readEvent(reader)
	if err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}
	if !strings.Contains(first, "message_start") {
		t.Fatalf("first chunk = %q, want the message_start event", first)
	}

	close(release)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("draining the stream: %v", err)
	}

	ev := collector.waitFor(t, 1)[0]
	want := Usage{InputTokens: 1200, OutputTokens: 340, Reported: true}
	if ev.Usage != want {
		t.Errorf("Usage = %+v, want %+v", ev.Usage, want)
	}
	if ev.Partial {
		t.Error("Partial = true, want false")
	}
}

func readEvent(reader *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			b.WriteString(line)
		}
		if err != nil {
			return b.String(), err
		}
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
	}
}

// Every allowlisted prefix must reach its provider. Only /v1/messages and
// /v1/chat/completions were previously exercised.
func TestRouteCoversEveryAllowlistedPrefix(t *testing.T) {
	t.Parallel()

	p := &Proxy{cfg: Config{
		AnthropicURL: mustParse(t, "https://api.anthropic.com"),
		OpenAIURL:    mustParse(t, "https://api.openai.com"),
		AnthropicKey: func(context.Context) (string, error) { return anthropicKey, nil },
		OpenAIKey:    func(context.Context) (string, error) { return openAIKey, nil },
	}}

	cases := []struct {
		path string
		// mode is the session's auth mode; empty means key mode, where this
		// proxy attaches the user's own credential.
		mode     string
		header   http.Header
		provider string
		wantErr  bool
	}{
		{path: "/v1/messages", provider: "anthropic"},
		{path: "/v1/messages/count_tokens", provider: "anthropic"},
		{path: "/v1/messages/batches", provider: "anthropic"},
		{path: "/v1/complete", provider: "anthropic"},
		{path: "/v1/models", provider: "anthropic"},
		{path: "/v1/models/claude-opus-5", provider: "anthropic"},
		{path: "/v1/chat/completions", provider: "openai"},
		{path: "/v1/responses", provider: "openai"},
		{path: "/v1/embeddings", provider: "openai"},
		{path: "/v1/files", wantErr: true},
		{path: "/v1/oauth/token", wantErr: true},
		{path: "/", wantErr: true},

		// A path this proxy has never heard of belongs upstream when the guest
		// carries its own credential: forwarding it grants nothing the guest
		// could not already do by talking to the provider directly, and Claude
		// Code adds endpoints across releases.
		{
			path:     "/v1/some-future-endpoint",
			mode:     AuthSubscription,
			header:   http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			provider: "anthropic",
		},
		{
			path:     "/v1/another/new/thing",
			mode:     AuthSubscription,
			header:   http.Header{"Anthropic-Beta": []string{"oauth-2025-04-20"}},
			provider: "anthropic",
		},
		{
			path:     "/v1/openai-future",
			mode:     AuthSubscription,
			header:   http.Header{"Openai-Beta": []string{"assistants=v2"}},
			provider: "openai",
		},

		// The same requests in key mode, where this proxy would attach the
		// user's own API key. An unknown path there is the rest of the provider
		// API on the user's credential, so it is refused.
		{
			path:    "/v1/some-future-endpoint",
			header:  http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			wantErr: true,
		},
		{
			path:    "/v1/organizations/me",
			header:  http.Header{"Anthropic-Version": []string{"2023-06-01"}},
			wantErr: true,
		},
		{
			path:    "/v1/openai-future",
			header:  http.Header{"Openai-Beta": []string{"assistants=v2"}},
			wantErr: true,
		},
		// Known paths keep working in both modes; the mode only governs the
		// benefit of the doubt.
		{path: "/v1/messages", mode: AuthSubscription, provider: "anthropic"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			header := tc.header
			if header == nil {
				header = http.Header{}
			}
			provider, upstream, keyFn, err := p.route(tc.path, header, tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("route(%q) error = nil, want an error", tc.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("route(%q) error = %v", tc.path, err)
			}
			if provider != tc.provider {
				t.Fatalf("route(%q) provider = %q, want %q", tc.path, provider, tc.provider)
			}
			if upstream == nil || keyFn == nil {
				t.Fatalf("route(%q) returned a nil upstream or key source", tc.path)
			}
		})
	}
}

// A locked keychain must not surface as a silent 502 - the cause belongs in the
// log even though the caller only sees a generic message.
func TestKeychainFailureIsLoggedAndNotLeaked(t *testing.T) {
	t.Parallel()

	var logged []string
	upstream := newUpstream(t, &recordedRequest{})

	p, err := New(Config{
		AnthropicURL: mustParse(t, "https://api.anthropic.com"),
		OpenAIURL:    mustParse(t, "https://api.openai.com"),
		AnthropicKey: func(context.Context) (string, error) {
			return "", errors.New("keychain is locked: user denied access")
		},
		OpenAIKey:   func(context.Context) (string, error) { return openAIKey, nil },
		TokenLookup: func(token string) (string, bool) { return "abc123", token == sessionToken },
		Transport:   redirectTransport{target: mustParse(t, upstream.URL)},
		Logf:        func(format string, args ...any) { logged = append(logged, sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", sessionToken)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "keychain is locked") {
		t.Fatalf("body leaked the keychain error: %q", rec.Body.String())
	}
	if !strings.Contains(strings.Join(logged, "\n"), "keychain is locked") {
		t.Fatalf("cause never reached the log: %v", logged)
	}
}

// An unrouted path must be logged even without SAINT_DEBUG: inside the guest it
// presents as an unexplained failure with no other signal.
func TestUnroutedPathIsLogged(t *testing.T) {
	t.Parallel()

	var logged []string
	upstream := newUpstream(t, &recordedRequest{})
	p := newProxy(t, upstream, func(format string, args ...any) {
		logged = append(logged, sprintf(format, args...))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", sessionToken)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "unrouted") || !strings.Contains(joined, "/v1/files") {
		t.Fatalf("unrouted path was not logged: %v", logged)
	}
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
