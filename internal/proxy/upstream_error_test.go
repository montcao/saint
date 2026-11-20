package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestUpstreamDiagnosticsForwardBytesAndKeepOnlyAllowedFields(t *testing.T) {
	t.Parallel()
	const secret = "secret-not-for-journal"
	body := []byte(fmt.Sprintf(`{"model":%q,"error":{"type":"rate_limit_error","message":%q}}`, secret, secret))
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", compressed), func(t *testing.T) {
			wire := body
			if compressed {
				wire = gzipFixture(t, body)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				h := w.Header()
				h.Set("Retry-After", "0032")
				h.Set("Set-Cookie", secret)
				h.Set("Anthropic-Ratelimit-Requests-Limit", "500")
				h.Set("Anthropic-Ratelimit-Requests-Remaining", "0")
				h.Set("Anthropic-Ratelimit-Requests-Reset", "2026-11-20T12:05:00-05:00")
				h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
				h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1795194000")
				h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.00")
				h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "NaN")
				h.Set("Anthropic-Ratelimit-Unified-7d-Status", secret)
				h.Set("Anthropic-Ratelimit-Unrecognized", secret)
				h.Set("Content-Type", "application/octet-stream")
				if compressed {
					h.Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(http.StatusTooManyRequests)
				// Flush to leave the error observer without a known length.
				w.(http.Flusher).Flush()
				_, _ = w.Write(wire)
			}))
			defer upstream.Close()
			cfg := testConfig(t, upstream, nil)
			var got []UpstreamErrorEvent
			var status []int
			cfg.OnUpstreamStatus = func(id, provider string, code int) {
				if id != sessionID || provider != "anthropic" {
					t.Errorf("upstream status for %q/%q", id, provider)
				}
				status = append(status, code)
			}
			cfg.OnUpstreamError = func(ev UpstreamErrorEvent) { got = append(got, ev) }
			cfg.OnUsage = func(UsageEvent) { t.Error("an error body must not be treated as a usage envelope") }
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.Header.Set("x-api-key", sessionToken)
			if compressed {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			resp := httptest.NewRecorder()
			p.ServeHTTP(resp, req)
			if resp.Code != http.StatusTooManyRequests || !bytes.Equal(resp.Body.Bytes(), wire) || resp.Header().Get("Retry-After") != "0032" {
				t.Fatalf("upstream response changed: status=%d headers=%v body equality=%t", resp.Code, resp.Header(), bytes.Equal(resp.Body.Bytes(), wire))
			}
			want := UpstreamErrorEvent{
				SessionID: sessionID, Provider: "anthropic", Status: 429, Type: "rate_limit_error", RetryAfter: "32",
				RateLimits: map[string]string{
					"anthropic-ratelimit-requests-limit":         "500",
					"anthropic-ratelimit-requests-remaining":     "0",
					"anthropic-ratelimit-requests-reset":         "2026-11-20T17:05:00Z",
					"anthropic-ratelimit-unified-status":         "rejected",
					"anthropic-ratelimit-unified-5h-reset":       "1795194000",
					"anthropic-ratelimit-unified-5h-utilization": "1",
				},
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], want) || !slices.Equal(status, []int{429}) {
				t.Fatalf("diagnostic=%+v status=%v, want one event %+v", got, status, want)
			}
			data, err := json.Marshal(got)
			if err != nil || bytes.Contains(data, []byte(secret)) {
				t.Fatal("diagnostic contains response content")
			}
		})
	}
}

func TestUpstreamErrorTypeOmitsUnknownTruncatedAndOversized(t *testing.T) {
	t.Parallel()
	large := []byte(`{"error":{"type":"overloaded_error","message":"` + strings.Repeat("x", maxErrorBodyBytes) + `"}}`)
	for _, tc := range []struct {
		name string
		body []byte
		gzip bool
	}{
		{name: "unknown", body: []byte(`{"error":{"type":"secret-not-for-journal"}}`)},
		{name: "truncated", body: []byte(`{"error":{"type":"overloaded_error"}`)},
		{name: "decompression limit", body: gzipFixture(t, large), gzip: true},
		{name: "broken gzip", body: []byte("truncated gzip"), gzip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUpstreamErrorType(tc.body, tc.gzip); got != "" {
				t.Errorf("unsafe diagnostic type %q", got)
			}
		})
	}
}

func gzipFixture(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
