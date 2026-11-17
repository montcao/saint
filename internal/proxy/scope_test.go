package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// scopedServer serves p with every connection confined by allowed, the way the
// proxy process confines each session network's listener.
func scopedServer(t *testing.T, p *Proxy, allowed func(string) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(p)
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return WithSessionScope(ctx, allowed)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// A token is accepted only on the listener for its own session's network, on
// both the API path and the forward path.
func TestATokenIsRefusedOnAnotherSessionsListener(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)
	other := scopedServer(t, h.proxy, func(id string) bool { return id == "someone-else" })
	own := scopedServer(t, h.proxy, func(id string) bool { return id == sessionID })

	req, err := http.NewRequest(http.MethodPost, other.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("API call on another session's listener: status = %d, want 401", resp.StatusCode)
	}

	_, _, connect := sendConnect(t, other.Listener.Addr().String(), "example.test:443", proxyCredentials(sessionToken))
	if connect.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("CONNECT on another session's listener: status = %d, want 407", connect.StatusCode)
	}

	_, _, ok := sendConnect(t, own.Listener.Addr().String(), "example.test:443", proxyCredentials(sessionToken))
	if ok.StatusCode != http.StatusOK {
		t.Errorf("CONNECT on the session's own listener: status = %d, want 200", ok.StatusCode)
	}
}

// Rules saved while the proxy runs apply to the next request, and every ruling
// is reported with its reason.
func TestLivePolicyAndDecisionEvents(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	saved := &HostPolicy{}
	var events []DecisionEvent

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(cfg *ForwardConfig) {
		cfg.Enforce = true
		cfg.LivePolicy = func() *HostPolicy {
			mu.Lock()
			defer mu.Unlock()
			return &HostPolicy{Allow: append([]string(nil), saved.Allow...)}
		}
		cfg.OnDecision = func(ev DecisionEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}
		// Nobody is there to ask.
	})

	_, _, first := sendConnect(t, h.addr(), "paste.example:443", proxyCredentials(sessionToken))
	if first.StatusCode != http.StatusForbidden {
		t.Fatalf("unruled host with nobody to ask: status = %d, want 403", first.StatusCode)
	}

	mu.Lock()
	saved.Allow = []string{"paste.example"}
	mu.Unlock()
	_, _, second := sendConnect(t, h.addr(), "paste.example:443", proxyCredentials(sessionToken))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("host saved while running: status = %d, want 200", second.StatusCode)
	}
	_, _, third := sendConnect(t, h.addr(), "github.com:443", proxyCredentials(sessionToken))
	if third.StatusCode != http.StatusOK {
		t.Fatalf("shipped host: status = %d, want 200", third.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []DecisionEvent{
		{SessionID: sessionID, Host: "paste.example", Decision: DecisionDeny, Reason: ReasonUnanswered},
		{SessionID: sessionID, Host: "paste.example", Decision: DecisionAllow, Reason: ReasonRule},
		{SessionID: sessionID, Host: "github.com", Decision: DecisionAllow, Reason: ReasonDefault},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

// An answer from a person is reported as one.
func TestAnAnswerIsReportedAsAnAnswer(t *testing.T) {
	t.Parallel()

	d := &policyDecider{allowlist: defaultAllowedHosts, ask: func(context.Context, string, string) Decision { return DecisionAllow }}
	if decision, reason := d.decide(context.Background(), "s1", "paste.example"); decision != DecisionAllow || reason != ReasonAnswer {
		t.Errorf("decide = %v, %q; want allow, %q", decision, reason, ReasonAnswer)
	}
	d.ask = func(context.Context, string, string) Decision { return DecisionAsk }
	if decision, reason := d.decide(context.Background(), "s1", "paste.example"); decision != DecisionDeny || reason != ReasonUnanswered {
		t.Errorf("decide = %v, %q; want deny, %q", decision, reason, ReasonUnanswered)
	}
}
