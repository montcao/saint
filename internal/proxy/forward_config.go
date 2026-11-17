package proxy

import (
	"context"
	"net"
	"slices"
	"strings"
	"time"
)

// ForwardConfig turns the forward-proxy path on.
//
// Nil - the default - leaves it off entirely: a CONNECT or an absolute-form
// request is refused with 405 and nothing is dialled, which is what this proxy
// did before this file existed.
type ForwardConfig struct {
	// AllowAnonymous accepts forward-proxied traffic that carries no session
	// credential.
	//
	// Off by default, and spelled as an opt-in for the reason the rest of the
	// limits are: this proxy binds the container bridge gateway, so every
	// container on that bridge can reach the port, not only the ones saint
	// started. With this on, any of them gets a tunnel to any host the policy
	// allows, recorded under the empty session ID because saint genuinely does
	// not know who it was.
	//
	// With it off, a guest authenticates the way every HTTP client already knows
	// how to: credentials in the proxy URL, HTTP_PROXY=http://saint:TOKEN@host:port,
	// which clients turn into Proxy-Authorization on the CONNECT and on
	// absolute-form requests. TOKEN is the session's existing proxy token, so
	// nothing new is issued and the user is still never asked for anything. A
	// client that cannot send proxy credentials gets 407 and falls back to
	// talking to the network directly - it is refused by saint, not cut off.
	AllowAnonymous bool

	// Policy optionally restricts which hosts may be reached. Nil allows every
	// host, which is the default.
	Policy *HostPolicy

	// LivePolicy, when set, is called for each forward-proxied request and
	// replaces Policy, so rules saved while the proxy runs - an "Allow always"
	// answer, a `saint allow` - apply from the next request on. It runs on the
	// request path, so it must be cheap (a cached read, not a parse per call).
	LivePolicy func() *HostPolicy

	// OnDecision, when set, receives every ruling on a forward-proxied host:
	// allowed or refused, and why. It is observation and must not block.
	OnDecision func(DecisionEvent)

	// Enforce turns the host policy from a record into a control.
	//
	// Off, the proxy observes: every host a session reaches is recorded and
	// none is refused, which is the behaviour this feature shipped with. On,
	// anything neither ruled on nor in the default allowlist is put to a
	// person through Ask, and refused if nobody answers.
	//
	// It is separate from Policy because the two say different things. A policy
	// with an empty allowlist means "no opinion"; enforcement is the decision
	// that no opinion should mean no.
	Enforce bool

	// Ask is consulted for a host nothing has ruled on. Nil means there is
	// nobody to ask, and with Enforce on, an unruled host is then denied - a
	// headless saint does not quietly become a permissive one.
	Ask func(ctx context.Context, sessionID, host string) Decision

	// AllowLoopbackTargets permits forward-proxying to loopback and link-local
	// addresses. Off by default, and the one denial that applies even with no
	// policy configured.
	//
	// The asymmetry is deliberate. A container cannot reach the host's
	// 127.0.0.1 on its own, so tunnelling there would hand a guest something it
	// did not otherwise have: every unauthenticated service the developer runs
	// locally, reached through saint. Link-local goes with it because
	// 169.254.169.254 is the cloud metadata address. Ordinary private ranges are
	// not blocked - the container already routes to those directly, so refusing
	// them would cost a real workload and buy nothing.
	AllowLoopbackTargets bool

	// MaxTrackedHosts caps how many (session, host) pairs the activity table
	// holds. Zero takes DefaultMaxTrackedHosts. There is no way to remove the
	// cap: an unbounded table is a leak, not a feature.
	MaxTrackedHosts int

	// DialTimeout bounds connecting to a target. Zero takes
	// DefaultForwardDialTimeout.
	DialTimeout time.Duration

	// DialContext overrides how target connections are made. Production leaves
	// it nil; tests point it at a local listener so no test reaches the network.
	// Overriding it also replaces the loopback guard above, which lives in the
	// default dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// HostPolicy is an optional allow/deny list for forward-proxied hosts.
//
// Matching is on the hostname, lowercased, with the port ignored: a rule about
// example.com is about the host, not one of its ports. A rule is either an
// exact hostname or a wildcard written "*.example.com" or ".example.com", which
// matches example.com and any subdomain of it.
//
// Deny wins over Allow. An empty Allow allows everything not denied - the
// default, and what "off unless asked for" means here. A non-empty Allow
// refuses every host it does not name.
//
// Remember what a denial is worth: it refuses traffic that came to saint. A
// guest on its own internal network has no other way to the host; a guest on
// a network with internet access can reach it by not using the proxy.
type HostPolicy struct {
	Allow []string
	Deny  []string
}

// permits reports whether the policy lets a hostname through. A nil policy
// permits everything.
func (pol *HostPolicy) permits(hostname string) bool {
	if pol == nil {
		return true
	}
	for _, rule := range pol.Deny {
		if matchHostRule(rule, hostname) {
			return false
		}
	}
	if len(pol.Allow) == 0 {
		return true
	}
	for _, rule := range pol.Allow {
		if matchHostRule(rule, hostname) {
			return true
		}
	}
	return false
}

func matchHostRule(rule, hostname string) bool {
	rule = strings.ToLower(strings.TrimSpace(rule))
	rule = strings.TrimPrefix(rule, "*")
	if rule == "" || rule == "." {
		return false
	}
	if domain, wildcard := strings.CutPrefix(rule, "."); wildcard {
		return hostname == domain || strings.HasSuffix(hostname, "."+domain)
	}
	return hostname == rule
}

// ForwardSettings is the forward-proxy posture, as values a caller can render
// honestly. Like Settings, it formats nothing.
type ForwardSettings struct {
	// Enabled reports whether the forward-proxy path is on at all. Everything
	// below is meaningless when it is false.
	Enabled bool

	// AnonymousAllowed reports whether traffic with no session credential is
	// accepted. True means any container on the bridge can use this proxy.
	AnonymousAllowed bool

	// LoopbackTargetsAllowed reports whether the host's own loopback and
	// link-local addresses may be reached through this proxy.
	LoopbackTargetsAllowed bool

	// AllowHosts and DenyHosts are the policy rules in force, sorted. An empty
	// AllowHosts means every host not denied, which is not the same as no host.
	AllowHosts []string
	DenyHosts  []string

	// MaxTrackedHosts is the ceiling on (session, host) rows.
	MaxTrackedHosts int
}

func (p *Proxy) forwardSettings() ForwardSettings {
	if p.forward == nil {
		return ForwardSettings{}
	}
	cfg := p.forward.cfg
	settings := ForwardSettings{
		Enabled:                true,
		AnonymousAllowed:       cfg.AllowAnonymous,
		LoopbackTargetsAllowed: cfg.AllowLoopbackTargets,
		MaxTrackedHosts:        p.forward.table.max,
	}
	if cfg.Policy != nil {
		settings.AllowHosts = slices.Sorted(slices.Values(cfg.Policy.Allow))
		settings.DenyHosts = slices.Sorted(slices.Values(cfg.Policy.Deny))
	}
	return settings
}

// allows is the single place a forward-proxied host is ruled on.
//
// Both request shapes - a CONNECT and an absolute-form HTTP request - come
// through here, because a policy that applied to one and not the other would be
// a policy with a documented way around it.
func (f *forwarder) allows(ctx context.Context, sessionID, host string) bool {
	policy := f.cfg.Policy
	if f.cfg.LivePolicy != nil {
		policy = f.cfg.LivePolicy()
	}

	// Observation mode keeps the original behaviour exactly: explicit rules are
	// honoured, and anything else passes. Turning enforcement on is what makes
	// "not ruled on" mean no.
	var decision Decision
	var reason string
	if !f.cfg.Enforce {
		decision, reason = DecisionAllow, ReasonObserved
		if !policy.permits(host) {
			decision, reason = DecisionDeny, ReasonRule
		}
	} else {
		decider := &policyDecider{
			policy:    policy,
			allowlist: defaultAllowedHosts,
			ask:       f.cfg.Ask,
		}
		decision, reason = decider.decide(ctx, sessionID, host)
	}
	if f.cfg.OnDecision != nil {
		f.cfg.OnDecision(DecisionEvent{SessionID: sessionID, Host: host, Decision: decision, Reason: reason})
	}
	return decision == DecisionAllow
}

// DefaultAllowedHosts returns a copy of the shipped allowlist, so a caller can
// show it beside the saved one.
func DefaultAllowedHosts() []string {
	return slices.Clone(defaultAllowedHosts)
}
