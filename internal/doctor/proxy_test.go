package doctor

import (
	"net"
	"strings"
	"testing"
)

// A reachable listener is reported healthy.
func TestProxyReachableResultWhenListening(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	got := proxyReachableResult(listener.Addr().String())
	if !got.OK {
		t.Fatalf("result = %+v, want OK", got)
	}
}

// Nothing listening must point at starting the proxy, not at the firewall.
func TestProxyReachableResultWhenNothingListening(t *testing.T) {
	t.Parallel()

	// Port 1 is privileged and nothing listens on it, so the dial is refused
	// immediately and deterministically. Binding an ephemeral port and closing
	// it does not work here: the port goes back to the pool and another test
	// running in parallel can take it, which made this fail intermittently.
	got := proxyReachableResult("127.0.0.1:1")
	if got.OK {
		t.Fatal("result is OK, want a failure")
	}
	if !strings.Contains(got.Fix, "saint proxy") {
		t.Fatalf("fix = %q, want it to suggest starting the proxy", got.Fix)
	}
	if strings.Contains(strings.ToLower(got.Detail), "firewall") {
		t.Fatalf("detail blamed the firewall with nothing listening: %q", got.Detail)
	}
}

// The firewall remediation must name socketfilterfw and a concrete binary path,
// since the allowlist is keyed by path.
func TestFirewallFixNamesSocketfilterfw(t *testing.T) {
	t.Parallel()

	fix := firewallFix()
	for _, want := range []string{"socketfilterfw", "--add", "--unblockapp", "sudo"} {
		if !strings.Contains(fix, want) {
			t.Fatalf("firewallFix() = %q, missing %q", fix, want)
		}
	}
}
