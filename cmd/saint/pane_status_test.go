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

// saint reports one state to herdr, blocked, and only while a question waits
// for a person; answering it hands the pane back to herdr's own reading.
// Working and idle are herdr's to show, so nothing here may report them.
func TestPaneReportsOnlyBlockedAndItsRelease(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionDeny {
		t.Fatalf("Ask() = %v, want deny", got)
	}
	waitForStates(t, fake, []string{"blocked", "release"})

	for _, c := range fake.commands() {
		if i := slices.Index(c, "--state"); i >= 0 && c[i+1] != "blocked" {
			t.Errorf("reported state %q, want only blocked: %q", c[i+1], c)
		}
	}
}

func TestCustodyLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		session store.Session
		want    []string
	}{
		{"plan token", store.Session{ProxyAuth: store.ProxyAuthSubscription, Credential: "keychain", CredentialExpiresAt: now.Add(300 * 24 * time.Hour)},
			[]string{"the agent holds your Claude plan token (keychain)", "expires in 10 months"}},
		{"host login", store.Session{ProxyAuth: store.ProxyAuthSubscription, Credential: "claude-code", CredentialExpiresAt: now.Add(5 * time.Hour)},
			[]string{"your Claude Code login", "expires in 5 hours"}},
		{"unknown expiry", store.Session{ProxyAuth: store.ProxyAuthSubscription},
			[]string{"expiry unknown"}},
		{"anthropic key", store.Session{Runtime: sandbox.RuntimeClaude},
			[]string{"the proxy holds the Anthropic API key", "the agent sees only a per-session token"}},
		{"codex", store.Session{Runtime: sandbox.RuntimeCodex},
			[]string{"OpenAI API key"}},
	} {
		got := custodyLine(tc.session, now)
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
