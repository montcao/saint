package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// stateCalls are the herdr calls that change what the pane shows, in order.
func stateCalls(fake *fakeHerdr) []string {
	var out []string
	for _, c := range fake.commands() {
		switch {
		case slices.Contains(c, "release-agent"):
			out = append(out, "release")
		case slices.Contains(c, "report-agent"):
			out = append(out, c[slices.Index(c, "--state")+1])
		}
	}
	return out
}

func waitForStates(t *testing.T, fake *fakeHerdr, want []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Equal(stateCalls(fake), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pane states = %q, want %q (calls %q)", stateCalls(fake), want, fake.commands())
}

// Overlapping requests mark the pane working once and release it once, when
// the last one ends; the pane is checked against herdr before the first.
func TestRequestsInFlightShowWorking(t *testing.T) {
	t.Parallel()

	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	end1 := p.Begin("s1")
	end2 := p.Begin("s1")
	waitForStates(t, fake, []string{"working"})
	end1()
	end1() // a second call does nothing
	time.Sleep(50 * time.Millisecond)
	if got := stateCalls(fake); !slices.Equal(got, []string{"working"}) {
		t.Fatalf("released with a request still in flight: %q", got)
	}
	end2()
	waitForStates(t, fake, []string{"working", "release"})

	calls := fake.commands()
	if !slices.Equal(calls[0], []string{"/herdr/one.sock", "pane", "get", "w1:p2"}) {
		t.Errorf("first call = %q, want the pane check", calls[0])
	}
	want := []string{"/herdr/one.sock", "pane", "report-agent", "w1:p2", "--source", "saint", "--agent", "codex", "--state", "working"}
	if !slices.Equal(calls[1], want) {
		t.Errorf("working call = %q, want %q", calls[1], want)
	}
}

// A question waiting for a person wins over working, and once it is
// answered a request still in flight shows working again.
func TestBlockedWinsOverWorking(t *testing.T) {
	t.Parallel()

	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	fake.prompt = func(conn *controlConn) {
		var resp controlResponse
		if err := conn.receive(&resp); err != nil || resp.Question == nil {
			return
		}
		_ = conn.send(controlAnswer{ID: resp.Question.ID, Allow: false})
		_ = conn.receive(&resp)
	}
	end := p.Begin("s1")
	waitForStates(t, fake, []string{"working"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionDeny {
		t.Fatalf("Ask() = %v, want deny", got)
	}
	waitForStates(t, fake, []string{"working", "blocked", "working"})
	end()
	waitForStates(t, fake, []string{"working", "blocked", "working", "release"})
}

// A session with no herdr pane, or one the proxy cannot attribute, shows
// nothing anywhere.
func TestRequestsWithoutAPaneReportNothing(t *testing.T) {
	t.Parallel()

	plain := herdrSession
	plain.Herdr = nil
	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": plain})
	p.Begin("s1")()
	p.Begin("unknown")()
	time.Sleep(50 * time.Millisecond)
	if calls := fake.commands(); len(calls) != 0 {
		t.Errorf("herdr calls for a session with no pane: %q", calls)
	}
}

func TestBillingLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		session store.Session
		want    []string
	}{
		{"plan token", store.Session{ProxyAuth: store.ProxyAuthSubscription, Credential: "keychain", CredentialExpiresAt: now.Add(300 * 24 * time.Hour)},
			[]string{"draws on your Claude plan", "plan token (keychain)", "expires in 10 months", "the agent holds that token"}},
		{"host login", store.Session{ProxyAuth: store.ProxyAuthSubscription, Credential: "claude-code", CredentialExpiresAt: now.Add(5 * time.Hour)},
			[]string{"your Claude Code login", "expires in 5 hours"}},
		{"unknown expiry", store.Session{ProxyAuth: store.ProxyAuthSubscription},
			[]string{"expiry unknown"}},
		{"anthropic key", store.Session{Runtime: sandbox.RuntimeClaude},
			[]string{"bills per token to the Anthropic API key", "never sees it"}},
		{"codex", store.Session{Runtime: sandbox.RuntimeCodex},
			[]string{"OpenAI API key"}},
	} {
		got := billingLine(tc.session, now)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q lacks %q", tc.name, got, want)
			}
		}
	}
	if got := credentialLife(now.Add(-time.Minute), now); got != "already expired" {
		t.Errorf("expired = %q", got)
	}
	if got := credentialLife(now.Add(30*time.Minute), now); got != "expires in 30 minutes" {
		t.Errorf("30m = %q", got)
	}
}
