package store

import "testing"

func TestProxyBindResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		bind       string
		wantHost   string
		isAuto     bool
		isLoopback bool
	}{
		{name: "unset defaults to auto", bind: "", wantHost: ProxyBindAuto, isAuto: true},
		{name: "auto", bind: "auto", wantHost: "auto", isAuto: true},
		{name: "auto is case insensitive", bind: "AUTO", wantHost: "AUTO", isAuto: true},
		{name: "whitespace trimmed", bind: "  auto  ", wantHost: "auto", isAuto: true},
		{name: "loopback flagged", bind: "127.0.0.1", wantHost: "127.0.0.1", isLoopback: true},
		{name: "localhost flagged", bind: "localhost", wantHost: "localhost", isLoopback: true},
		{name: "ipv6 loopback flagged", bind: "::1", wantHost: "::1", isLoopback: true},
		{name: "gateway pinned", bind: "192.168.64.1", wantHost: "192.168.64.1"},
		{name: "wildcard", bind: "0.0.0.0", wantHost: "0.0.0.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{ProxyBind: tc.bind}
			if got := cfg.ProxyBindHost(); got != tc.wantHost {
				t.Fatalf("ProxyBindHost() = %q, want %q", got, tc.wantHost)
			}
			if got := cfg.ProxyBindIsAuto(); got != tc.isAuto {
				t.Fatalf("ProxyBindIsAuto() = %v, want %v", got, tc.isAuto)
			}
			if got := cfg.ProxyBindIsLoopback(); got != tc.isLoopback {
				t.Fatalf("ProxyBindIsLoopback() = %v, want %v", got, tc.isLoopback)
			}
		})
	}
}

// A fresh config must default to the discovered gateway. Defaulting to loopback
// would bind fine and look healthy while being unreachable from every guest.
func TestDefaultConfigIsGuestReachable(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	if !cfg.ProxyBindIsAuto() {
		t.Fatalf("default ProxyBind = %q, want the auto sentinel", cfg.ProxyBind)
	}
	if cfg.ProxyBindIsLoopback() {
		t.Fatal("default bind must not be loopback; guests cannot reach it")
	}
	if cfg.ProxyNetworkOrDefault() == "" {
		t.Fatal("default proxy network is empty")
	}
}
