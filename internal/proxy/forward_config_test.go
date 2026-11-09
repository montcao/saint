package proxy

import (
	"net/http"
	"testing"
)

// A denied host is refused before anything is dialled, and the attempt is
// recorded - a host with a denial and no connection is exactly the thing the
// table exists to show.
func TestHostPolicyDeniesAndRecordsWithoutDialling(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.Policy = &HostPolicy{Deny: []string{"*.blocked.test"}}
	})

	_, _, resp := sendConnect(t, h.addr(), "api.blocked.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to a denied host: status = %d, want 403", resp.StatusCode)
	}
	if got := h.dials.Load(); got != 0 {
		t.Fatalf("dials = %d, want 0: a denied host must never be connected to", got)
	}

	row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "api.blocked.test")
	if row.Denials != 1 || row.Tunnels != 0 {
		t.Errorf("Denials/Tunnels = %d/%d, want 1/0", row.Denials, row.Tunnels)
	}

	// An allow list that does not name the host refuses it too, and the host it
	// does name still goes through.
	h2 := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.Policy = &HostPolicy{Allow: []string{"allowed.test"}}
	})
	_, _, denied := sendConnect(t, h2.addr(), "other.test:443", proxyCredentials(sessionToken))
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("host outside the allow list: status = %d, want 403", denied.StatusCode)
	}
	_, _, allowed := sendConnect(t, h2.addr(), "allowed.test:443", proxyCredentials(sessionToken))
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("host on the allow list: status = %d, want 200", allowed.StatusCode)
	}
}

func TestHostPolicyMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy *HostPolicy
		host   string
		want   bool
	}{
		{name: "nil policy allows everything", policy: nil, host: "anything.test", want: true},
		{name: "empty policy allows everything", policy: &HostPolicy{}, host: "anything.test", want: true},
		{name: "exact deny", policy: &HostPolicy{Deny: []string{"bad.test"}}, host: "bad.test", want: false},
		{name: "exact deny does not match a subdomain", policy: &HostPolicy{Deny: []string{"bad.test"}}, host: "sub.bad.test", want: true},
		{name: "wildcard deny matches a subdomain", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "sub.bad.test", want: false},
		{name: "wildcard deny matches the domain itself", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "bad.test", want: false},
		{name: "dot form is the same wildcard", policy: &HostPolicy{Deny: []string{".bad.test"}}, host: "sub.bad.test", want: false},
		{name: "wildcard does not match a lookalike suffix", policy: &HostPolicy{Deny: []string{"*.bad.test"}}, host: "notbad.test", want: true},
		{name: "allow list refuses what it does not name", policy: &HostPolicy{Allow: []string{"good.test"}}, host: "other.test", want: false},
		{name: "allow list permits what it names", policy: &HostPolicy{Allow: []string{"good.test"}}, host: "good.test", want: true},
		{name: "deny wins over allow", policy: &HostPolicy{Allow: []string{"good.test"}, Deny: []string{"good.test"}}, host: "good.test", want: false},
		{name: "rules are case insensitive", policy: &HostPolicy{Deny: []string{"BAD.test"}}, host: "bad.test", want: false},
		{name: "an empty rule matches nothing", policy: &HostPolicy{Deny: []string{"", "*"}}, host: "bad.test", want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.policy.permits(tc.host); got != tc.want {
				t.Errorf("permits(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// Settings has to describe the forward posture as values, so a renderer can say
// what is on without this package deciding how.
func TestForwardSettingsReportThePosture(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(c *ForwardConfig) {
		c.AllowAnonymous = true
		c.MaxTrackedHosts = 7
		c.Policy = &HostPolicy{Deny: []string{"zeta.test", "alpha.test"}}
	})

	forward := h.proxy.Settings().Forward
	if !forward.Enabled || !forward.AnonymousAllowed || forward.LoopbackTargetsAllowed {
		t.Errorf("Forward = %+v, want enabled, anonymous allowed, loopback refused", forward)
	}
	if forward.MaxTrackedHosts != 7 {
		t.Errorf("MaxTrackedHosts = %d, want 7", forward.MaxTrackedHosts)
	}
	want := []string{"alpha.test", "zeta.test"}
	if len(forward.DenyHosts) != len(want) {
		t.Fatalf("DenyHosts = %v, want %v", forward.DenyHosts, want)
	}
	for i := range want {
		if forward.DenyHosts[i] != want[i] {
			t.Fatalf("DenyHosts = %v, want %v sorted", forward.DenyHosts, want)
		}
	}
	if len(forward.AllowHosts) != 0 {
		t.Errorf("AllowHosts = %v, want empty, which means every host not denied", forward.AllowHosts)
	}
}
