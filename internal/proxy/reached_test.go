package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostReachedWithoutUsageCallback(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer upstream.Close()
	cfg := testConfig(t, upstream, nil)
	var got string
	cfg.OnHostReached = func(id, host string) {
		if id != sessionID {
			t.Errorf("wrong session %s", id)
		}
		got = host
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", sessionToken)
	p.ServeHTTP(httptest.NewRecorder(), req)
	if got == "" {
		t.Fatal("upstream response was not observed")
	}
}

func TestHostReachedPreservesUpstreamDiagnostics(t *testing.T) {
	t.Parallel()
	const body = `{"error":{"type":"rate_limit_error"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	cfg := testConfig(t, upstream, nil)
	var reached, statuses, diagnostics int
	cfg.OnHostReached = func(id, host string) { reached++ }
	cfg.OnUpstreamStatus = func(id, provider string, status int) {
		if status != http.StatusTooManyRequests {
			t.Errorf("status = %d", status)
		}
		statuses++
	}
	cfg.OnUpstreamError = func(ev UpstreamErrorEvent) {
		if ev.Type != "rate_limit_error" || ev.RetryAfter != "12" {
			t.Errorf("diagnostic = %+v", ev)
		}
		diagnostics++
	}
	cfg.OnUsage = func(UsageEvent) { t.Error("error response counted as usage") }
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", sessionToken)
	resp := httptest.NewRecorder()
	p.ServeHTTP(resp, req)
	if reached != 1 || statuses != 1 || diagnostics != 1 {
		t.Fatalf("callbacks: reached=%d status=%d diagnostics=%d", reached, statuses, diagnostics)
	}
	if resp.Code != http.StatusTooManyRequests || resp.Body.String() != body || resp.Header().Get("Retry-After") != "12" {
		t.Fatalf("response changed: %d %s %v", resp.Code, resp.Body.String(), resp.Header())
	}
}
