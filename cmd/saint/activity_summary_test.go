package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/montcao/saint/internal/store"
)

func TestSummaryChangesOnlyAndPreservesCustody(t *testing.T) {
	t.Parallel()
	st := store.New(t.TempDir())
	pane := store.HerdrPane{PaneID: "w:p", TerminalID: "terminal"}
	if err := st.AddSession(store.Session{ID: "abc", Herdr: &pane}); err != nil {
		t.Fatal(err)
	}
	reports := make(chan []string, 100)
	s := newActivitySummary(st)
	s.terminal = func(context.Context, store.HerdrPane) (string, error) { return "terminal", nil }
	s.run = func(_ context.Context, _ store.HerdrPane, args ...string) error { reports <- args; return nil }
	go s.loop()
	defer s.close()
	take := func(want string) {
		t.Helper()
		select {
		case args := <-reports:
			if len(args) != 5 || args[1] != "report-metadata" || args[3] != "--title" || !strings.Contains(args[4], want) {
				t.Fatalf("unexpected report %q, want %s", args, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no report containing %s", want)
		}
	}
	s.refresh("abc")
	take("proxy placeholder only")
	s.reached("abc", "example.com")
	take("hosts reached 1")
	for range 100 {
		s.reached("abc", "example.com")
	}
	select {
	case args := <-reports:
		t.Fatalf("duplicate report %q", args)
	case <-time.After(30 * time.Millisecond):
	}
	s.refused("abc")
	take("refusals 1")
	if err := st.UpdateSession("abc", func(v *store.Session) { v.ProxyAuth = store.ProxyAuthSubscription }); err != nil {
		t.Fatal(err)
	}
	s.refresh("abc")
	take("real subscription credential")
}

func TestSummaryConcurrentUpdatesAndBounds(t *testing.T) {
	t.Parallel()
	s := newActivitySummary(store.New(t.TempDir()))
	defer s.cancel()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for i := range 500 {
				s.reached("abc", fmt.Sprint(i))
				s.refused("abc")
			}
		})
	}
	wg.Wait()
	v := s.states["abc"]
	if len(v.hosts) != 500 || v.refusals != 5000 {
		t.Fatalf("hosts=%d refusals=%d", len(v.hosts), v.refusals)
	}
	for i := range summaryHostLimit + 10 {
		s.reached("abc", fmt.Sprint(i))
	}
	if len(v.hosts) != summaryHostLimit || !v.overflow {
		t.Fatal("unbounded host tracking")
	}
	for i := range summarySessionLimit + 10 {
		s.refresh(fmt.Sprintf("s%d", i))
	}
	if len(s.states) != summarySessionLimit {
		t.Fatal("unbounded session tracking")
	}
}

func TestSummaryRejectsReusedPane(t *testing.T) {
	t.Parallel()
	st := store.New(t.TempDir())
	pane := store.HerdrPane{PaneID: "w:p", TerminalID: "old"}
	if err := st.AddSession(store.Session{ID: "abc", Herdr: &pane}); err != nil {
		t.Fatal(err)
	}
	s := newActivitySummary(st)
	checked := make(chan struct{})
	s.terminal = func(context.Context, store.HerdrPane) (string, error) { close(checked); return "new", nil }
	s.run = func(context.Context, store.HerdrPane, ...string) error {
		t.Error("reported into reused pane")
		return nil
	}
	go s.loop()
	s.refresh("abc")
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("no pane check")
	}
	s.close()
}
