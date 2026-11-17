package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

func TestAllowCommand(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runAllow(st, args, &out)
		return out.String(), err
	}

	if out, err := run("Paste.Example"); err != nil || !strings.Contains(out, "allowed paste.example") {
		t.Fatalf("allow = %q, %v", out, err)
	}
	if out, err := run("paste.example"); err != nil || !strings.Contains(out, "already") {
		t.Fatalf("allow again = %q, %v", out, err)
	}
	list, err := run()
	if err != nil {
		t.Fatal(err)
	}
	saved, shipped, _ := strings.Cut(list, "Shipped with saint:")
	if !strings.Contains(saved, "paste.example") || !strings.Contains(shipped, "registry.npmjs.org") {
		t.Errorf("list does not separate saved from shipped hosts:\n%s", list)
	}
	if out, err := run("-rm", "paste.example"); err != nil || !strings.Contains(out, "removed") {
		t.Fatalf("rm = %q, %v", out, err)
	}
	if _, err := run("-rm", "paste.example"); err == nil {
		t.Fatal("removing a host that is not saved succeeded")
	}
	if _, err := run("https://paste.example/x"); err == nil {
		t.Fatal("a URL was accepted as a host")
	}
	if _, err := run("-x"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("bad flag: err = %v, want usage", err)
	}
}

// The journal says what was decided about a host and why, in words.
func TestEgressRecordsInTheJournal(t *testing.T) {
	t.Parallel()

	rec := egressRecord(proxy.DecisionEvent{SessionID: "s1", Host: "paste.example\x1b[2J", Decision: proxy.DecisionDeny, Reason: proxy.ReasonUnanswered}, time.Now()).sanitized()
	what, result := journalSummary(rec)
	if what != "egress paste.example[2J" {
		t.Errorf("what = %q", what)
	}
	if result != "deny (no answer)" {
		t.Errorf("result = %q", result)
	}
	_, result = journalSummary(egressRecord(proxy.DecisionEvent{Host: "github.com", Decision: proxy.DecisionAllow, Reason: proxy.ReasonDefault}, time.Now()))
	if result != "allow (shipped allowlist)" {
		t.Errorf("result = %q", result)
	}
}
