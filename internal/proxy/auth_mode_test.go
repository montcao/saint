package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const guestOAuth = "oauth-subscription-token-value"

// forwarded records what the upstream actually received.
type forwarded struct {
	authorization string
	apiKey        string
	beta          string
	version       string
	saintSession  string
}

func newForwardingUpstream(t *testing.T, got *forwarded) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.authorization = r.Header.Get("Authorization")
		got.apiKey = r.Header.Get("x-api-key")
		got.beta = r.Header.Get("anthropic-beta")
		got.version = r.Header.Get("anthropic-version")
		got.saintSession = r.Header.Get(SaintSessionHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func newModeProxy(t *testing.T, upstream *httptest.Server, mode string) *Proxy {
	t.Helper()

	cfg := testConfig(t, upstream, nil)
	cfg.SessionAuth = func(string) string { return mode }
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

// subscriptionRequest is what Claude Code sends when it holds a claude.ai login
// and only ANTHROPIC_BASE_URL points at a gateway.
func subscriptionRequest(t *testing.T, frontURL string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, frontURL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+guestOAuth)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14")
	req.Header.Set(SaintSessionHeader, sessionToken)
	return req
}

func do(t *testing.T, p *Proxy, req func(string) *http.Request) *http.Response {
	t.Helper()

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	resp, err := http.DefaultClient.Do(req(front.URL))
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

// The whole point of subscription mode: the guest's own credential reaches
// Anthropic untouched, so the subscription stays the active credential and the
// plan is what gets billed.
func TestSubscriptionModeForwardsTheGuestCredential(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthSubscription)

	resp := do(t, p, func(url string) *http.Request { return subscriptionRequest(t, url) })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got.authorization != "Bearer "+guestOAuth {
		t.Errorf("Authorization = %q, want the guest's own credential", got.authorization)
	}
	// Injecting the host's key would replace the subscription and move the
	// session onto per-token billing.
	if strings.Contains(got.apiKey, anthropicKey) || strings.Contains(got.authorization, anthropicKey) {
		t.Error("the host's API key was injected in subscription mode")
	}
	// Stripping anthropic-beta drops the OAuth capability the upstream requires
	// and fails every subscription request with a 401.
	if !strings.Contains(got.beta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want it forwarded verbatim", got.beta)
	}
	if got.version != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want it forwarded verbatim", got.version)
	}
	// saint's own header is saint's business, not the provider's.
	if got.saintSession != "" {
		t.Errorf("%s reached the upstream: %q", SaintSessionHeader, got.saintSession)
	}
}

// Authentication is not lost in subscription mode - it moves to saint's header.
func TestSubscriptionModeStillAuthenticates(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthSubscription)

	resp := do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		// A valid subscription credential, but no saint session token.
		req.Header.Set("Authorization", "Bearer "+guestOAuth)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a saint session token", resp.StatusCode)
	}
	if got.authorization != "" {
		t.Error("an unauthenticated request was forwarded upstream")
	}
}

// Mode comes from the session record, never from the request, so a guest cannot
// change which header it sends to talk the proxy into injecting the real key.
func TestGuestCannotDowngradeToKeyMode(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthSubscription)

	do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		// Presenting the session token the way key mode expects.
		req.Header.Set("x-api-key", sessionToken)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req
	})

	if got.apiKey == anthropicKey {
		t.Fatal("the real API key was injected for a session recorded as subscription")
	}
}

// Key mode is unchanged: the session token is stripped and the real key added.
func TestKeyModeStillInjects(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthKey)

	do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("x-api-key", sessionToken)
		req.Header.Set("anthropic-beta", "fine-grained-tool-streaming-2025-05-14")
		return req
	})

	if got.apiKey != anthropicKey {
		t.Errorf("x-api-key = %q, want the real upstream key", got.apiKey)
	}
	if strings.Contains(got.apiKey, sessionToken) {
		t.Error("the session token was forwarded upstream")
	}
	// anthropic-beta must survive stripping in both modes: stripAuthHeaders
	// matches on anthropic- prefixes and is one edit away from eating it.
	if got.beta != "fine-grained-tool-streaming-2025-05-14" {
		t.Errorf("anthropic-beta = %q, want it forwarded in key mode too", got.beta)
	}
}

// A nil SessionAuth is the pre-subscription behaviour.
func TestNilSessionAuthMeansKeyMode(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newProxy(t, upstream, nil)

	do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("x-api-key", sessionToken)
		return req
	})

	if got.apiKey != anthropicKey {
		t.Errorf("x-api-key = %q, want the real key with no SessionAuth configured", got.apiKey)
	}
}

// The session token may also arrive in saint's header in key mode; a guest that
// sets ANTHROPIC_CUSTOM_HEADERS is still the same session.
func TestSaintHeaderAuthenticatesInKeyModeToo(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthKey)

	resp := do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set(SaintSessionHeader, sessionToken)
		return req
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.saintSession != "" {
		t.Errorf("%s reached the upstream in key mode: %q", SaintSessionHeader, got.saintSession)
	}
}

// The hole this closes: an unknown path used to be forwarded on the strength of
// a protocol header alone. In key mode that attaches the user's own API key, so
// a compromised guest could reach the rest of the provider's API rather than
// just inference.
func TestKeyModeRefusesUnknownPathsRatherThanAttachingTheKey(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthKey)

	resp := do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/organizations/me", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("x-api-key", sessionToken)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req
	})

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown path in key mode", resp.StatusCode)
	}
	if got.apiKey != "" || got.authorization != "" {
		t.Error("an unknown path reached the upstream with a credential attached")
	}
}

// Subscription mode keeps the benefit of the doubt: the guest already holds its
// own credential, so forwarding an unknown path grants it nothing new, and
// Claude Code gains endpoints across releases.
func TestSubscriptionModeStillForwardsUnknownPaths(t *testing.T) {
	t.Parallel()

	var got forwarded
	upstream := newForwardingUpstream(t, &got)
	p := newModeProxy(t, upstream, AuthSubscription)

	resp := do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/some-future-endpoint", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+guestOAuth)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set(SaintSessionHeader, sessionToken)
		return req
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the request forwarded", resp.StatusCode)
	}
	if got.authorization != "Bearer "+guestOAuth {
		t.Errorf("Authorization = %q, want the guest's own credential", got.authorization)
	}
}

// An unauthenticated caller learns nothing - not whether a path routes, and not
// by way of a log line describing it.
func TestUnauthenticatedRequestsAreRefusedBeforeRouting(t *testing.T) {
	t.Parallel()

	var logged []string
	var mu sync.Mutex
	var got forwarded
	upstream := newForwardingUpstream(t, &got)

	cfg := testConfig(t, upstream, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	resp := do(t, p, func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/nonsense", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		return req
	})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before any routing decision", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(strings.Join(logged, "\n"), "unrouted") {
		t.Error("an unauthenticated caller's path was logged")
	}
}
