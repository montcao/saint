package usage

import (
	"sync"
	"testing"
	"time"

	"github.com/montcao/saint/internal/proxy"
)

func testMeter() *Meter {
	m := NewMeter()
	stamp := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return stamp }
	return m
}

func reported(input, output, cacheRead, cacheWrite int) proxy.Usage {
	return proxy.Usage{
		InputTokens:         input,
		OutputTokens:        output,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		Reported:            true,
	}
}

func TestMeterAccumulatesTotalsButNotContext(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{
		SessionID: "abc123", Provider: "anthropic", Model: "claude-opus-5",
		Usage: reported(1000, 100, 500, 40),
	})
	m.Record(proxy.UsageEvent{
		SessionID: "abc123", Provider: "anthropic", Model: "claude-opus-5",
		Usage: reported(1500, 250, 900, 0),
	})

	got, ok := m.Session("abc123")
	if !ok {
		t.Fatal("Session() reported no data for a metered session")
	}
	if got.Requests != 2 {
		t.Errorf("Requests = %d, want 2", got.Requests)
	}
	if got.InputTokens != 2500 || got.OutputTokens != 350 {
		t.Errorf("token totals = in %d out %d, want in 2500 out 350", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 1400 || got.CacheWriteTokens != 40 {
		t.Errorf("cache totals = r%d w%d, want r1400 w40", got.CacheReadTokens, got.CacheWriteTokens)
	}
	// Context is the latest prompt's occupancy - 1500 + 900 + 0 - not the sum
	// of every prompt so far. Summing it would climb past any real window.
	if got.Context != 2400 {
		t.Errorf("Context = %d, want 2400 (the most recent request only)", got.Context)
	}
	if !got.Reported {
		t.Error("Reported = false, want true")
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", got.Model)
	}
}

// A request the provider did not account for still happened. It must count as a
// request without moving the totals or erasing what was previously known.
func TestUnreportedEventDoesNotDisturbTotals(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{
		SessionID: "abc123", Provider: "anthropic", Model: "claude-opus-5",
		Usage: reported(1000, 100, 0, 0),
	})
	m.Record(proxy.UsageEvent{SessionID: "abc123", Provider: "anthropic"})

	got, _ := m.Session("abc123")
	if got.Requests != 2 {
		t.Errorf("Requests = %d, want 2", got.Requests)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 100 {
		t.Errorf("totals moved on an unreported event: %+v", got)
	}
	if got.Context != 1000 {
		t.Errorf("Context = %d, want the last measured value 1000", got.Context)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the last known model to survive", got.Model)
	}
}

func TestUnreportedOnlySessionStaysUnreported(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Provider: "openai"})

	got, ok := m.Session("abc123")
	if !ok {
		t.Fatal("Session() reported no data after a request")
	}
	if got.Reported {
		t.Error("Reported = true, want false: no provider ever sent counters")
	}
	if got.Requests != 1 {
		t.Errorf("Requests = %d, want 1", got.Requests)
	}
}

func TestMeterCountsPartialRequests(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Partial: true, Usage: reported(10, 2, 0, 0)})
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(10, 2, 0, 0)})

	got, _ := m.Session("abc123")
	if got.Partial != 1 {
		t.Errorf("Partial = %d, want 1", got.Partial)
	}
}

func TestMeterKeepsSessionsSeparate(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "aaa", Usage: reported(10, 1, 0, 0)})
	m.Record(proxy.UsageEvent{SessionID: "bbb", Usage: reported(20, 2, 0, 0)})

	a, _ := m.Session("aaa")
	b, _ := m.Session("bbb")
	if a.InputTokens != 10 || b.InputTokens != 20 {
		t.Errorf("sessions bled into each other: a=%+v b=%+v", a, b)
	}
}

func TestForgetDropsASession(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(10, 1, 0, 0)})
	m.Forget("abc123")

	if _, ok := m.Session("abc123"); ok {
		t.Error("Session() still reports data after Forget()")
	}
}

// The TUI renders from a snapshot without holding the lock, so the snapshot has
// to be a copy.
func TestSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(10, 1, 0, 0)})

	snap := m.Snapshot()
	entry := snap["abc123"]
	entry.InputTokens = 999
	snap["abc123"] = entry
	snap["injected"] = Session{}

	got, _ := m.Session("abc123")
	if got.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10: the snapshot aliased meter state", got.InputTokens)
	}
	if _, ok := m.Session("injected"); ok {
		t.Error("writing to the snapshot added a session to the meter")
	}
}

func TestMeterIgnoresEmptySessionID(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{Usage: reported(10, 1, 0, 0)})

	if len(m.Snapshot()) != 0 {
		t.Errorf("Snapshot() = %+v, want empty", m.Snapshot())
	}
}

// The loader holds this field optionally, so every method has to survive nil.
func TestNilMeterIsSafe(t *testing.T) {
	t.Parallel()

	var m *Meter
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(1, 1, 0, 0)})
	m.Forget("abc123")
	if _, ok := m.Session("abc123"); ok {
		t.Error("nil meter reported a session")
	}
	if snap := m.Snapshot(); snap != nil {
		t.Errorf("Snapshot() = %+v, want nil", snap)
	}
}

// Record runs on the proxy's response goroutines while the TUI snapshots.
func TestMeterUnderConcurrentUse(t *testing.T) {
	t.Parallel()

	m := testMeter()
	const writers, perWriter = 8, 50

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + w))
			for range perWriter {
				m.Record(proxy.UsageEvent{SessionID: id, Usage: reported(1, 1, 0, 0)})
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = m.Snapshot()
		}
	}()

	wg.Wait()
	<-done

	snap := m.Snapshot()
	if len(snap) != writers {
		t.Fatalf("sessions = %d, want %d", len(snap), writers)
	}
	for id, s := range snap {
		if s.Requests != perWriter {
			t.Errorf("session %q Requests = %d, want %d", id, s.Requests, perWriter)
		}
	}
}

// The only honest "working" signal saint has is a request crossing the proxy,
// so Begin has to be visible in the snapshot the TUI renders from - and has to
// come back to zero when the request ends.
func TestBeginTracksInFlightThroughSnapshot(t *testing.T) {
	t.Parallel()

	m := testMeter()
	end := m.Begin("abc123")

	if got := m.Snapshot()["abc123"].InFlight; got != 1 {
		t.Errorf("InFlight during a request = %d, want 1", got)
	}
	// A session can be working before any provider has reported a token. That
	// must not be mistaken for measured usage.
	if got := m.Snapshot()["abc123"]; got.Reported {
		t.Error("Reported = true after Begin alone, want false: Begin observes traffic, not tokens")
	}

	second := m.Begin("abc123")
	if got := m.Snapshot()["abc123"].InFlight; got != 2 {
		t.Errorf("InFlight with two requests = %d, want 2", got)
	}

	end()
	second()
	if got := m.Snapshot()["abc123"].InFlight; got != 0 {
		t.Errorf("InFlight after both ended = %d, want 0", got)
	}
}

// A metered session with nothing outstanding reports zero, and that zero is a
// measurement rather than a gap: it is what the UI reads as idle.
func TestMeteredSessionWithNoRequestReportsZeroInFlight(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(10, 1, 0, 0)})

	got, ok := m.Session("abc123")
	if !ok {
		t.Fatal("Session() reported no data for a metered session")
	}
	if got.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", got.InFlight)
	}
}

// A second end must not push the counter below zero. An under-count reports a
// working session as idle, and nothing later in the process corrects it.
func TestEndCalledTwiceDoesNotUnderflow(t *testing.T) {
	t.Parallel()

	m := testMeter()
	first := m.Begin("abc123")
	second := m.Begin("abc123")

	first()
	first()
	first()

	if got := m.Snapshot()["abc123"].InFlight; got != 1 {
		t.Errorf("InFlight after three calls to one end = %d, want 1", got)
	}

	second()
	second()
	if got := m.Snapshot()["abc123"].InFlight; got != 0 {
		t.Errorf("InFlight after every end ran = %d, want 0", got)
	}
}

// A session can be deleted while one of its requests is still crossing the
// proxy. The end call must not put an empty row back.
func TestEndAfterForgetDoesNotResurrectTheSession(t *testing.T) {
	t.Parallel()

	m := testMeter()
	m.Record(proxy.UsageEvent{SessionID: "abc123", Usage: reported(10, 1, 0, 0)})
	end := m.Begin("abc123")

	m.Forget("abc123")
	end()

	if _, ok := m.Session("abc123"); ok {
		t.Error("Session() reported a forgotten session: the end call recreated its row")
	}
}

func TestBeginIgnoresEmptySessionID(t *testing.T) {
	t.Parallel()

	m := testMeter()
	end := m.Begin("")
	end()

	if len(m.Snapshot()) != 0 {
		t.Errorf("Snapshot() = %+v, want empty", m.Snapshot())
	}
}

func TestNilMeterBeginIsSafe(t *testing.T) {
	t.Parallel()

	var m *Meter
	end := m.Begin("abc123")
	if end == nil {
		t.Fatal("Begin() on a nil meter returned a nil end func")
	}
	end()
}

// Begin runs on the proxy's serving goroutines, several sessions at a time,
// while the TUI snapshots. The count has to land back on exactly zero: this is
// the test that would catch a lost decrement, which presents as a session that
// says working forever.
func TestInFlightUnderConcurrentUse(t *testing.T) {
	t.Parallel()

	m := testMeter()
	const workers, perWorker = 8, 100

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + w))
			for range perWorker {
				end := m.Begin(id)
				// Calling end twice is what a careless caller does; the counter
				// has to survive it under load, not only in isolation.
				end()
				end()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			for _, session := range m.Snapshot() {
				if session.InFlight < 0 {
					t.Errorf("InFlight = %d, want never negative", session.InFlight)
				}
			}
		}
	}()

	wg.Wait()
	<-done

	for id, session := range m.Snapshot() {
		if session.InFlight != 0 {
			t.Errorf("session %q InFlight = %d after every request ended, want 0", id, session.InFlight)
		}
	}
}
