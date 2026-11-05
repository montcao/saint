package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

func TestUsageSummary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  SessionRow
		want string
	}{
		{
			// Nothing has gone through the proxy. That is not the same as a
			// session that has spent nothing, so it must not render as zeros.
			name: "no proxied request yet",
			row:  SessionRow{ID: "abc123"},
			want: "-",
		},
		{
			name: "provider reported nothing",
			row: SessionRow{
				ID:         "abc123",
				UsageKnown: true,
				Usage:      usage.Session{Requests: 3},
			},
			want: "unreported (3 requests)",
		},
		{
			name: "measured",
			row: SessionRow{
				ID:         "abc123",
				UsageKnown: true,
				Usage: usage.Session{
					Model: "claude-opus-5", Requests: 1,
					InputTokens: 1200, OutputTokens: 340,
					CacheReadTokens: 900, CacheWriteTokens: 64,
					Context: 2164, Reported: true,
				},
			},
			want: "claude-opus-5 • ctx 2164 • in 1200 • out 340 • cache r900/w64 • 1 request",
		},
		{
			name: "measured with a cancelled generation",
			row: SessionRow{
				ID:         "abc123",
				UsageKnown: true,
				Usage: usage.Session{
					Model: "claude-opus-5", Requests: 4, Partial: 2,
					InputTokens: 12000, OutputTokens: 340, Context: 12000, Reported: true,
				},
			},
			want: "claude-opus-5 • ctx 12.0k • in 12.0k • out 340 • cache r0/w0 • 4 requests • 2 partial",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := usageSummary(tc.row); got != tc.want {
				t.Errorf("usageSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0"},
		{in: 999, want: "999"},
		// Small counts stay exact: rounding hides the difference between a
		// fresh context and a warm one.
		{in: 9_999, want: "9999"},
		{in: 10_000, want: "10.0k"},
		{in: 128_400, want: "128.4k"},
		{in: 999_999, want: "1000.0k"},
		{in: 1_000_000, want: "1.00M"},
		{in: 2_345_678, want: "2.35M"},
		{in: -5, want: "0"},
	}

	for _, tc := range cases {
		if got := formatTokens(tc.in); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The detail panel is the only place usage is visible before the sidebar lands,
// so it has to actually render there.
func TestDetailPanelShowsUsage(t *testing.T) {
	t.Parallel()

	m := Model{
		width:  100,
		height: 40,
		rows: []SessionRow{{
			ID: "abc123", Name: "demo", Runtime: "claude", State: "running",
			UsageKnown: true,
			Usage: usage.Session{
				Model: "claude-opus-5", Requests: 2,
				InputTokens: 1200, OutputTokens: 340, Context: 1200, Reported: true,
			},
		}},
	}

	// Narrow: no sidebar, so the detail panel has to carry the numbers itself.
	m.width = sidebarWidth + minMainWidth - 1
	panel := m.renderDetailPanel()
	for _, want := range []string{"tokens", "claude-opus-5", "ctx 1200", "out 340"} {
		if !strings.Contains(panel, want) {
			t.Errorf("detail panel is missing %q:\n%s", want, panel)
		}
	}

	// Wide: the sidebar breaks the same numbers out line by line, so repeating
	// them here as one dense run is strictly worse than not repeating them.
	m.width = sidebarWidth + minMainWidth + 40
	if panel := m.renderDetailPanel(); strings.Contains(panel, "tokens") {
		t.Errorf("detail panel repeats the sidebar's numbers:\n%s", panel)
	}
}

// The meter is wired into the loader, not the Model, so the snapshot is where
// usage has to arrive.
func TestLoadAttachesUsageToRows(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if err := st.SaveSessions([]store.Session{
		{ID: "metered1", Name: "metered", Runtime: sandbox.RuntimeClaude},
		{ID: "unmeterd", Name: "quiet", Runtime: sandbox.RuntimeClaude},
	}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	meter := usage.NewMeter()
	meter.Record(proxy.UsageEvent{
		SessionID: "metered1",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		Usage: proxy.Usage{
			InputTokens: 1200, OutputTokens: 340, Reported: true,
		},
	})

	loader := &SnapshotLoader{
		Store:     st,
		Sandbox:   &fakeSandbox{},
		Inspector: NewRepoInspector("/usr/bin/git", nil),
		Usage:     meter,
	}
	snapshot, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	byID := make(map[string]SessionRow, len(snapshot.Rows))
	for _, row := range snapshot.Rows {
		byID[row.ID] = row
	}

	metered, ok := byID["metered1"]
	if !ok {
		t.Fatal("the metered session is missing from the snapshot")
	}
	if !metered.UsageKnown {
		t.Fatal("UsageKnown = false for a session the meter has data for")
	}
	if metered.Usage.InputTokens != 1200 || metered.Usage.Model != "claude-opus-5" {
		t.Errorf("Usage = %+v, want the metered values", metered.Usage)
	}

	quiet, ok := byID["unmeterd"]
	if !ok {
		t.Fatal("the unmetered session is missing from the snapshot")
	}
	if quiet.UsageKnown {
		t.Errorf("UsageKnown = true for a session that made no proxied request")
	}
}

// `saint ls` and the plain-text path run without a proxy at all.
func TestLoadWithoutAMeterIsFine(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if err := st.SaveSessions([]store.Session{
		{ID: "abc12345", Name: "demo", Runtime: sandbox.RuntimeClaude},
	}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	loader := &SnapshotLoader{
		Store:     st,
		Sandbox:   &fakeSandbox{},
		Inspector: NewRepoInspector("/usr/bin/git", nil),
	}
	snapshot, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(snapshot.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(snapshot.Rows))
	}
	if snapshot.Rows[0].UsageKnown {
		t.Error("UsageKnown = true without a meter")
	}
}
