package main

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/proxy"
)

func activityTestConnection(t *testing.T, j *journal, req controlRequest) (*controlConn, activityMessage, <-chan struct{}) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		j.serveActivity(req, newControlConn(server))
	}()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	c := newControlConn(client)
	var ack activityMessage
	if err := c.receive(&ack); err != nil {
		t.Fatal(err)
	}
	return c, ack, done
}

func waitActivityDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("activity handler did not release its connection")
	}
}

func TestActivityLeaseScopesEventsAndClosesWithTheOwner(t *testing.T) {
	t.Parallel()
	j := newJournalWriter("", 0, 0)
	const first = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const second = "BCDEFGHIJKLMNOPQRSTUVWXYZA"

	req := controlRequest{Op: controlOpActivityOwner, Session: "abc", Stream: first}
	owner, ack, ownerDone := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	// This arrives before herdr starts the sidecar.
	j.activity.publish(journalRecord{Kind: journalKindUpstreamStatus, SessionID: "xyz", Status: 200})
	j.activity.publish(journalRecord{Kind: journalKindUpstreamStatus, SessionID: "abc", Status: 429})

	req.Op = controlOpActivity
	view, ack, viewDone := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	var event activityMessage
	if err := view.receive(&event); err != nil || event.Record == nil || event.Record.SessionID != "abc" || event.Record.Status != 429 {
		t.Fatalf("scoped startup replay: %+v (%v)", event, err)
	}

	// A new attach replaces the old lease, and closing the old owner cannot
	// tear down the replacement. A sidecar from the old pane cannot join it.
	newOwner, ack, newOwnerDone := activityTestConnection(t, j, controlRequest{Op: controlOpActivityOwner, Session: "abc", Stream: second})
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	_ = owner.conn.Close()
	waitActivityDone(t, ownerDone)
	waitActivityDone(t, viewDone)
	_, ack, staleDone := activityTestConnection(t, j, req)
	if ack.OK {
		t.Fatal("stale viewer joined a replacement lease")
	}
	waitActivityDone(t, staleDone)

	req.Stream = second
	newView, ack, newViewDone := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	_ = newOwner.conn.Close()
	waitActivityDone(t, newOwnerDone)
	waitActivityDone(t, newViewDone)
	if err := newView.receive(&event); !errors.Is(err, io.EOF) {
		t.Fatalf("idle sidecar after agent exit: %v, want EOF", err)
	}
	_, ack, lateDone := activityTestConnection(t, j, req)
	if ack.OK || j.activity.active.Load() {
		t.Fatal("idle or late viewer survived the owner")
	}
	waitActivityDone(t, lateDone)
}

func TestActivitySlowOrClosedSidecarReleasesItsLease(t *testing.T) {
	t.Parallel()
	j := newJournalWriter("", 0, 0)
	const stream = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	req := controlRequest{Op: controlOpActivityOwner, Session: "abc", Stream: stream}
	_, ack, done := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	for i := 0; i <= activityQueueSize; i++ {
		j.activity.publish(journalRecord{Kind: journalKindUsage, SessionID: "abc"})
	}
	waitActivityDone(t, done)
	if j.activity.active.Load() {
		t.Fatal("overflowed lease remains queued")
	}
	_, ack, ownerDone := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	req.Op = controlOpActivity
	view, ack, viewDone := activityTestConnection(t, j, req)
	if !ack.OK {
		t.Fatal(ack.Error)
	}
	_ = view.conn.Close()
	waitActivityDone(t, viewDone)
	waitActivityDone(t, ownerDone)
	if j.activity.active.Load() {
		t.Fatal("closed sidecar left an active lease")
	}
}

func TestActivityRendersChangesAndCountsRepeatedDecisions(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	view := activityView{out: &out}
	ev := journalRecord{Kind: journalKindEgress, Path: "intake.datadoghq.com", Decision: proxy.DecisionAllow.String(), Reason: proxy.ReasonDefault}
	for _, rec := range []journalRecord{
		ev, ev, ev,
		{Kind: journalKindUpstreamStatus, Provider: "anthropic", Status: 429},
		{Kind: journalKindUsage, Provider: "anthropic", Model: "claude\u202e\u009b", Usage: &journalUsage{InputTokens: 41}},
		{Kind: journalKindEgress, Path: "example.com", Decision: proxy.DecisionDeny.String(), Reason: proxy.ReasonUnanswered},
	} {
		if err := view.render(rec); err != nil {
			t.Fatal(err)
		}
	}
	want := "intake.datadoghq.com [telemetry] allowed (default)\n" +
		"intake.datadoghq.com [telemetry] allowed (default) (2 more)\n" +
		"anthropic HTTP 429\n" +
		"anthropic usage claude single in=41 out=0 cache_read=0 cache_write=0\n" +
		"example.com refused (no answer)\n"
	if got := out.String(); got != want {
		t.Fatalf("activity output:\n%s\nwant:\n%s", got, want)
	}
}
