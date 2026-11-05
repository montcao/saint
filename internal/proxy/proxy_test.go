package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	sessionToken = "session-token-0123456789abcdef"
	sessionID    = "abc123"
	anthropicKey = "sk-ant-real-upstream-key"
	openAIKey    = "sk-oai-real-upstream-key"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return parsed
}

// redirectTransport sends requests aimed at the pinned upstream hosts to a
// local test server instead. It exists so tests never need loopback on the
// upstream host allowlist.
type redirectTransport struct{ target *url.URL }

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

type recordedRequest struct {
	apiKey        string
	authorization string
	body          string
	count         int
}

// testConfig is the shared base for proxy tests. Metering tests take this and
// add an OnUsage callback so the authenticated, redirected upstream wiring is
// defined in exactly one place.
func testConfig(t *testing.T, upstream *httptest.Server, logf func(string, ...any)) Config {
	t.Helper()

	return Config{
		AnthropicURL: mustParse(t, "https://api.anthropic.com"),
		OpenAIURL:    mustParse(t, "https://api.openai.com"),
		AnthropicKey: func(context.Context) (string, error) { return anthropicKey, nil },
		OpenAIKey:    func(context.Context) (string, error) { return openAIKey, nil },
		TokenLookup: func(token string) (string, bool) {
			if token == sessionToken {
				return sessionID, true
			}
			return "", false
		},
		Transport: redirectTransport{target: mustParse(t, upstream.URL)},
		Logf:      logf,
	}
}

func newProxy(t *testing.T, upstream *httptest.Server, logf func(string, ...any)) *Proxy {
	t.Helper()

	p, err := New(testConfig(t, upstream, logf))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

func newUpstream(t *testing.T, got *recordedRequest) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.count++
		got.apiKey = r.Header.Get("x-api-key")
		got.authorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()

	base := func() Config {
		return Config{
			AnthropicURL: mustParse(t, "https://api.anthropic.com"),
			OpenAIURL:    mustParse(t, "https://api.openai.com"),
			AnthropicKey: func(context.Context) (string, error) { return "a", nil },
			OpenAIKey:    func(context.Context) (string, error) { return "b", nil },
			TokenLookup:  func(string) (string, bool) { return "", false },
		}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing anthropic url", mutate: func(c *Config) { c.AnthropicURL = nil }},
		{name: "missing openai url", mutate: func(c *Config) { c.OpenAIURL = nil }},
		{name: "missing token lookup", mutate: func(c *Config) { c.TokenLookup = nil }},
		{name: "missing key source", mutate: func(c *Config) { c.AnthropicKey = nil }},
		{
			name:   "non allowlisted upstream",
			mutate: func(c *Config) { c.AnthropicURL = mustParse(t, "https://example.org") },
		},
		{
			name:   "loopback upstream is not allowlisted",
			mutate: func(c *Config) { c.OpenAIURL = mustParse(t, "https://127.0.0.1:8787") },
		},
		{
			name:   "localhost upstream is not allowlisted",
			mutate: func(c *Config) { c.OpenAIURL = mustParse(t, "https://localhost:8787") },
		},
		{
			name:   "plaintext upstream",
			mutate: func(c *Config) { c.AnthropicURL = mustParse(t, "http://api.anthropic.com") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New() error = nil, want non-nil")
			}
		})
	}
}

func TestProxyRejectsUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no credential", headers: nil},
		{name: "wrong x-api-key", headers: map[string]string{"x-api-key": "not-the-token"}},
		{name: "wrong bearer", headers: map[string]string{"Authorization": "Bearer not-the-token"}},
		{name: "empty bearer", headers: map[string]string{"Authorization": "Bearer "}},
		{name: "the real upstream key is not a valid caller credential", headers: map[string]string{"x-api-key": anthropicKey}},
		{name: "token prefix", headers: map[string]string{"x-api-key": sessionToken[:8]}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := &recordedRequest{}
			p := newProxy(t, newUpstream(t, got), nil)

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload"))
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got.count != 0 {
				t.Fatal("unauthenticated request reached the upstream")
			}
		})
	}
}

func TestProxyForwardsAuthenticatedAnthropicRequest(t *testing.T) {
	t.Parallel()

	for _, header := range []string{"x-api-key", "Authorization"} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()

			got := &recordedRequest{}
			p := newProxy(t, newUpstream(t, got), nil)

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("payload-123"))
			if header == "x-api-key" {
				req.Header.Set("x-api-key", sessionToken)
			} else {
				req.Header.Set("Authorization", "Bearer "+sessionToken)
			}
			req.Header.Set("anthropic-api-key", "another")
			rec := httptest.NewRecorder()

			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got.apiKey != anthropicKey {
				t.Fatalf("upstream x-api-key = %q, want the real key", got.apiKey)
			}
			if got.authorization != "" {
				t.Fatalf("upstream Authorization = %q, want empty", got.authorization)
			}
			if strings.Contains(got.apiKey, sessionToken) || strings.Contains(got.authorization, sessionToken) {
				t.Fatal("session token leaked to the upstream")
			}
			if got.body != "payload-123" {
				t.Fatalf("body = %q", got.body)
			}
		})
	}
}

func TestProxyForwardsAuthenticatedOpenAIRequest(t *testing.T) {
	t.Parallel()

	got := &recordedRequest{}
	p := newProxy(t, newUpstream(t, got), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got.authorization != "Bearer "+openAIKey {
		t.Fatalf("upstream Authorization = %q, want the real key", got.authorization)
	}
	if strings.Contains(got.authorization, sessionToken) {
		t.Fatal("session token leaked to the upstream")
	}
}

func TestProxyLogsSessionButNoSecrets(t *testing.T) {
	t.Parallel()

	var logs []string
	got := &recordedRequest{}
	p := newProxy(t, newUpstream(t, got), func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("secret-body"))
	req.Header.Set("x-api-key", sessionToken)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %#v", logs)
	}
	line := logs[0]
	if !strings.Contains(line, "session=abc123") {
		t.Fatalf("log line = %q, want the resolved session id", line)
	}
	for _, secret := range []string{sessionToken, anthropicKey, openAIKey, "secret-body"} {
		if strings.Contains(line, secret) {
			t.Fatalf("log leaked %q: %s", secret, line)
		}
	}
}

func TestProxyRejectsUnknownPathBeforeAuth(t *testing.T) {
	t.Parallel()

	got := &recordedRequest{}
	p := newProxy(t, newUpstream(t, got), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	req.Header.Set("x-api-key", sessionToken)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got.count != 0 {
		t.Fatal("request reached the upstream")
	}
}

func TestInboundTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
		want    []string
	}{
		{name: "none", headers: nil, want: nil},
		{name: "api key", headers: map[string]string{"x-api-key": "tok"}, want: []string{"tok"}},
		{name: "bearer", headers: map[string]string{"Authorization": "Bearer tok"}, want: []string{"tok"}},
		{name: "raw authorization", headers: map[string]string{"Authorization": "tok"}, want: []string{"tok"}},
		{
			name:    "both are candidates",
			headers: map[string]string{"x-api-key": "one", "Authorization": "Bearer two"},
			want:    []string{"one", "two"},
		},
		{name: "whitespace trimmed", headers: map[string]string{"x-api-key": "  tok  "}, want: []string{"tok"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			for name, value := range tc.headers {
				h.Set(name, value)
			}
			got := inboundTokens(h)
			if len(got) != len(tc.want) {
				t.Fatalf("inboundTokens() = %#v, want %#v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("inboundTokens()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
