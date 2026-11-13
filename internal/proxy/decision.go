package proxy

import (
	"context"
	"strings"
	"time"
)

// Deciding whether a guest may reach a host.
//
// The forward proxy began as visibility: it recorded where a session went and
// refused nothing. That is a useful thing to have and a poor thing to claim,
// because a record of an exfiltration is not a defence against one.
//
// Turning it into a control needs a third answer. Allow and deny alone force a
// choice between a list nobody can keep current - which fails closed on the
// next dependency a build pulls in - and a list that permits everything, which
// is where this started. The third answer is to ask, and the cost of asking is
// what the rest of this file is about.

// Decision is what a policy says about one host.
type Decision int

const (
	// DecisionAsk means saint does not know and a person should say. It is the
	// default for a host nothing has ruled on, which is what makes the policy
	// deny-by-default without making it unusable.
	DecisionAsk Decision = iota
	// DecisionAllow permits the connection.
	DecisionAllow
	// DecisionDeny refuses it.
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	default:
		return "ask"
	}
}

// Decider answers whether a session may reach a host.
//
// It is given a context that is cancelled when the guest gives up waiting, so
// an implementation that blocks on a human can stop blocking when there is no
// longer anyone to answer for.
//
// Returning DecisionAsk from a Decider is not an answer and is treated as a
// denial by the caller: the question was already asked.
type Decider interface {
	Decide(ctx context.Context, sessionID, host string) Decision
}

// askTimeout bounds how long a guest waits for a person.
//
// A prompt that blocks forever is how an agent running unattended at three in
// the morning becomes an agent that is simply stuck, with no error, no output
// and nothing in the log to explain it. Past this the request is refused and
// the refusal is recorded, so the host can be allowed in the morning and the
// agent gets a clean network error in the meantime rather than a hang.
const askTimeout = 20 * time.Second

// policyDecider is the built-in policy: explicit rules first, then the default
// allowlist, then ask.
type policyDecider struct {
	policy *HostPolicy
	// allowlist is the shipped default: hosts an agent needs to do its job at
	// all. It is deliberately incomplete - see defaultAllowedHosts.
	allowlist []string
	// ask is consulted for anything not ruled on. Nil means there is nobody to
	// ask, and an unruled host is then denied rather than silently permitted.
	ask func(ctx context.Context, sessionID, host string) Decision
	// wait bounds how long a guest waits for a person. Zero takes askTimeout;
	// it is a field so a test can exercise the timeout without spending it.
	wait time.Duration
}

func (d *policyDecider) Decide(ctx context.Context, sessionID, host string) Decision {
	decision, _ := d.decide(ctx, sessionID, host)
	return decision
}

// decide is Decide with the reason, for the record of what was decided.
func (d *policyDecider) decide(ctx context.Context, sessionID, host string) (Decision, string) {
	host = strings.ToLower(strings.TrimSpace(host))

	// An explicit denial is final and is never asked about.
	if d.policy != nil {
		for _, rule := range d.policy.Deny {
			if matchHostRule(rule, host) {
				return DecisionDeny, ReasonRule
			}
		}
		for _, rule := range d.policy.Allow {
			if matchHostRule(rule, host) {
				return DecisionAllow, ReasonRule
			}
		}
	}

	for _, rule := range d.allowlist {
		if matchHostRule(rule, host) {
			return DecisionAllow, ReasonDefault
		}
	}

	if d.ask == nil {
		return DecisionDeny, ReasonUnanswered
	}

	wait := d.wait
	if wait <= 0 {
		wait = askTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if answer := d.ask(ctx, sessionID, host); answer != DecisionAsk {
		return answer, ReasonAnswer
	}
	// Nobody answered. A question that timed out is a refusal, not a
	// permission: the safe direction when there is no person is the one that
	// does not let traffic out.
	return DecisionDeny, ReasonUnanswered
}

// Reasons a forward-proxied host was allowed or refused, as DecisionEvent
// reports them.
const (
	// ReasonRule: a saved allow or deny rule (HostPolicy) matched.
	ReasonRule = "rule"
	// ReasonDefault: the host is on the shipped allowlist.
	ReasonDefault = "default"
	// ReasonAnswer: a person was asked and answered.
	ReasonAnswer = "answer"
	// ReasonUnanswered: nobody answered in time, or there was nobody to ask.
	ReasonUnanswered = "unanswered"
	// ReasonObserved: enforcement is off, so nothing was ruled on.
	ReasonObserved = "observed"
)

// DecisionEvent is one ruling on a forward-proxied host, for a caller to
// record. It names the host, never a path, header or payload.
type DecisionEvent struct {
	SessionID string
	Host      string
	Decision  Decision
	Reason    string
}

// defaultAllowedHosts are the hosts an agent needs before it can do anything at
// all, so that turning enforcement on does not present a wall of prompts before
// the first useful minute.
//
// It is deliberately incomplete and saying so is the point. These are the hosts
// observed to be needed, not a survey of everything a build might reach: a
// registry mirror, a private index, a company proxy, the next language's
// toolchain - none of them are here and all of them are legitimate. The prompt
// is how this list grows, which is why the prompt has to be good.
//
// Nothing here is a wildcard over a whole domain that a third party can create
// subdomains under. `github.com` is the site; `*.github.io` is anybody's.
var defaultAllowedHosts = []string{
	// The providers saint itself proxies. A guest reaching these directly
	// rather than through the reverse-proxy path is unusual but not hostile.
	"api.anthropic.com",
	"api.openai.com",

	// Source. codeload and objects are where the archives and LFS blobs come
	// from, and a clone fails without them in a way that looks like a network
	// fault rather than a policy.
	"github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",

	// Package registries, one per ecosystem the shipped images can build.
	"registry.npmjs.org",
	"pypi.org",
	"files.pythonhosted.org",
	"proxy.golang.org",
	"sum.golang.org",
	"crates.io",
	"static.crates.io",

	// Claude Code reaches these on its first start: downloads for its own
	// updater, and its third-party logging intake for metrics and error
	// reports. Without them every new session is asked about both before the
	// person has typed anything. DISABLE_TELEMETRY, DISABLE_ERROR_REPORTING
	// and DISABLE_AUTOUPDATER in the guest turn the traffic off instead.
	"downloads.claude.ai",
	"http-intake.logs.us5.datadoghq.com",
}
