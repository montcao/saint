package proxy

import (
	"context"
	"testing"
	"time"
)

func deciderFor(policy *HostPolicy, ask func(context.Context, string, string) Decision) *policyDecider {
	// A short wait: the behaviour under test is that an unanswered question is
	// refused, not how many seconds it waits first.
	return &policyDecider{policy: policy, allowlist: defaultAllowedHosts, ask: ask, wait: 50 * time.Millisecond}
}

func never(t *testing.T) func(context.Context, string, string) Decision {
	t.Helper()
	return func(_ context.Context, _, host string) Decision {
		t.Errorf("a person was asked about %q, which should have been decided without them", host)
		return DecisionDeny
	}
}

// Order matters more than any single rule: a denial the user has already given
// must not be re-asked, and must outrank both the allowlist and an allow rule.
func TestAnExplicitDenialIsFinal(t *testing.T) {
	t.Parallel()

	d := deciderFor(&HostPolicy{
		Deny:  []string{"github.com", "evil.example"},
		Allow: []string{"github.com"},
	}, never(t))

	for _, host := range []string{"github.com", "evil.example"} {
		if got := d.Decide(context.Background(), "s1", host); got != DecisionDeny {
			t.Errorf("Decide(%q) = %v, want deny", host, got)
		}
	}
}

// The shipped allowlist exists so that turning enforcement on does not present
// a wall of prompts before the first useful minute.
func TestTheDefaultAllowlistAnswersWithoutAPerson(t *testing.T) {
	t.Parallel()

	d := deciderFor(nil, never(t))
	for _, host := range []string{
		"api.anthropic.com", "github.com", "registry.npmjs.org",
		"proxy.golang.org", "files.pythonhosted.org",
	} {
		if got := d.Decide(context.Background(), "s1", host); got != DecisionAllow {
			t.Errorf("Decide(%q) = %v, want allow", host, got)
		}
	}
}

// Anything nobody has ruled on is put to a person. This is the whole difference
// between a policy that is a control and a list nobody can keep current.
func TestAnUnknownHostIsAsked(t *testing.T) {
	t.Parallel()

	asked := ""
	d := deciderFor(nil, func(_ context.Context, session, host string) Decision {
		asked = session + " " + host
		return DecisionAllow
	})

	if got := d.Decide(context.Background(), "s1", "paste.example"); got != DecisionAllow {
		t.Errorf("Decide() = %v, want the answer the person gave", got)
	}
	if asked != "s1 paste.example" {
		t.Errorf("asked %q, want the session and host", asked)
	}
}

// A prompt that blocks forever is how an agent running unattended becomes an
// agent that is simply stuck, with no error and nothing in the log.
func TestAnUnansweredQuestionIsARefusal(t *testing.T) {
	t.Parallel()

	d := deciderFor(nil, func(ctx context.Context, _, _ string) Decision {
		// Nobody is at the keyboard: wait for the deadline the decider set.
		<-ctx.Done()
		return DecisionAsk
	})

	start := time.Now()
	got := d.Decide(context.Background(), "s1", "unknown.example")
	if got != DecisionDeny {
		t.Errorf("Decide() = %v, want deny when nobody answered", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for an answer; the wait is supposed to be bounded", elapsed)
	}
}

// A guest that gave up waiting takes the question with it: there is no longer
// anyone the answer could help.
func TestAGuestGivingUpEndsTheQuestion(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	d := deciderFor(nil, func(ctx context.Context, _, _ string) Decision {
		<-ctx.Done()
		return DecisionAsk
	})

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if got := d.Decide(ctx, "s1", "unknown.example"); got != DecisionDeny {
		t.Errorf("Decide() = %v, want deny", got)
	}
}

// A headless saint must not quietly become a permissive one.
func TestWithNobodyToAskAnUnknownHostIsDenied(t *testing.T) {
	t.Parallel()

	d := deciderFor(nil, nil)
	if got := d.Decide(context.Background(), "s1", "unknown.example"); got != DecisionDeny {
		t.Errorf("Decide() = %v, want deny with no way to ask", got)
	}
	// The allowlist still answers, so a headless run is usable rather than
	// merely safe.
	if got := d.Decide(context.Background(), "s1", "github.com"); got != DecisionAllow {
		t.Errorf("Decide(github.com) = %v, want allow", got)
	}
}

// The default allowlist must not contain a wildcard over a domain a third party
// can create names under. `github.com` is a site; `*.github.io` is anybody's.
func TestTheDefaultAllowlistHasNoThirdPartyWildcards(t *testing.T) {
	t.Parallel()

	for _, host := range defaultAllowedHosts {
		if len(host) > 0 && host[0] == '*' {
			t.Errorf("default allowlist entry %q is a wildcard", host)
		}
		for _, risky := range []string{".github.io", ".pages.dev", ".vercel.app", ".herokuapp.com"} {
			if len(host) >= len(risky) && host[len(host)-len(risky):] == risky {
				t.Errorf("default allowlist entry %q is on a domain anybody can take a name under", host)
			}
		}
	}
}
