package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/montcao/saint/internal/proxy"
)

type okTransport struct{}

func (okTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

// testListenerSet serves a real proxy whose tokens "tok-a" and "tok-b" belong
// to sessions a and b, on networks net-a and net-b. Port 0 lets the kernel
// choose, so nothing here depends on a free port on this machine.
func testListenerSet(t *testing.T) *listenerSet {
	t.Helper()
	upstream := func(raw string) *url.URL { u, _ := url.Parse(raw); return u }
	key := func(context.Context) (string, error) { return "real-key", nil }
	p, err := proxy.New(proxy.Config{
		AnthropicURL: upstream("https://api.anthropic.com"),
		OpenAIURL:    upstream("https://api.openai.com"),
		AnthropicKey: key,
		OpenAIKey:    key,
		TokenLookup: func(token string) (string, bool) {
			switch token {
			case "tok-a":
				return "a", true
			case "tok-b":
				return "b", true
			}
			return "", false
		},
		Transport: okTransport{},
	})
	if err != nil {
		t.Fatal(err)
	}
	networks := map[string]string{"a": "net-a", "b": "net-b"}
	ls := newListenerSet(0, p, func(id string) string { return networks[id] }, t.Logf)
	t.Cleanup(func() { _ = ls.shutdown(context.Background()) })
	return ls
}

func apiStatus(t *testing.T, addr, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Each network's listener accepts only the sessions on that network.
func TestAListenerServesOnlyItsOwnNetwork(t *testing.T) {
	t.Parallel()

	ls := testListenerSet(t)
	addr, err := ls.ensure(context.Background(), "net-a", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := apiStatus(t, addr, "tok-a"); got != http.StatusOK {
		t.Errorf("own session on its network: status %d, want 200", got)
	}
	if got := apiStatus(t, addr, "tok-b"); got != http.StatusUnauthorized {
		t.Errorf("another session's token on this network: status %d, want 401", got)
	}
}

// Binding is idempotent per host, and a host reused by a new network (the
// runtime reuses a deleted network's subnet) now answers for that network.
func TestEnsureReusesAndRemapsAHost(t *testing.T) {
	t.Parallel()

	ls := testListenerSet(t)
	first, err := ls.ensure(context.Background(), "net-a", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ls.ensure(context.Background(), "net-b", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("second ensure bound %s, want the existing %s", second, first)
	}
	if got := len(ls.addrs()); got != 1 {
		t.Errorf("listening on %d addresses, want 1", got)
	}
	if got := apiStatus(t, first, "tok-b"); got != http.StatusOK {
		t.Errorf("after remap, net-b's session: status %d, want 200", got)
	}
	if got := apiStatus(t, first, "tok-a"); got != http.StatusUnauthorized {
		t.Errorf("after remap, net-a's session: status %d, want 401", got)
	}
}

// A gateway whose address is not up yet - its container is still attaching -
// is retried rather than failed.
func TestEnsureWaitsForTheGatewayAddress(t *testing.T) {
	t.Parallel()

	ls := testListenerSet(t)
	var attempts atomic.Int32
	ls.listenTCP = func(addr string) (net.Listener, error) {
		if attempts.Add(1) < 3 {
			return nil, &net.OpError{Op: "listen", Err: syscall.EADDRNOTAVAIL}
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	if _, err := ls.ensure(context.Background(), "net-a", "192.0.2.1"); err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}

	ls.listenTCP = func(string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Err: syscall.EADDRINUSE}
	}
	if _, err := ls.ensure(context.Background(), "net-b", "192.0.2.2"); err == nil {
		t.Fatal("a port in use was retried as if the bridge were coming up")
	}
}
