package tui

import (
	"testing"
	"time"

	"github.com/montcao/saint/internal/usage"
)

func workingAt(id string, last time.Time) SessionRow {
	r := SessionRow{ID: id, Name: id, Runtime: "claude", State: "running", Activity: "working"}
	if !last.IsZero() {
		r.UsageKnown = true
		r.Usage = usage.Session{Model: "claude-opus-5", Reported: true, LastAt: last}
	}
	return r
}

// The whole discipline in one test: an idle deck schedules nothing.
//
// A permanent animation clock is wasted work on a tool that spends most of its
// life doing nothing, and motion that is always present stops meaning anything
// within a day.
func TestAnIdleDeckSchedulesNoAnimation(t *testing.T) {
	t.Parallel()

	idle := []SessionRow{
		{ID: "a", State: "running", Activity: "idle"},
		{ID: "b", State: "stopped", Activity: "-"},
	}
	if cmd := breathCmd(idle, at(0)); cmd != nil {
		t.Error("a deck with nothing working scheduled an animation frame")
	}
	if anyWorking(idle) {
		t.Error("anyWorking() said yes with nothing working")
	}

	if cmd := breathCmd(append(idle, workingAt("c", at(0))), at(0)); cmd == nil {
		t.Error("a working session did not start the animation")
	}
}

// The rate is a claim about how recently the agent produced tokens, so it may
// only be made when that was measured.
func TestBreathRateIsOnlyClaimedWhenMeasured(t *testing.T) {
	t.Parallel()

	now := at(60)

	cases := []struct {
		name string
		rows []SessionRow
		want time.Duration
	}{
		{
			name: "streaming right now",
			rows: []SessionRow{workingAt("a", now.Add(-1*time.Second))},
			want: breathLively,
		},
		{
			// Still working, just not streaming - a long tool call, say.
			name: "working but quiet",
			rows: []SessionRow{workingAt("a", now.Add(-30*time.Second))},
			want: breathSlow,
		},
		{
			// Known to be working, because a request is in flight; nothing
			// known about its tokens. The rate is not guessed.
			name: "working, never metered",
			rows: []SessionRow{workingAt("a", time.Time{})},
			want: breathSlow,
		},
		{
			name: "the liveliest session sets the rate",
			rows: []SessionRow{
				workingAt("a", now.Add(-5*time.Minute)),
				workingAt("b", now.Add(-1*time.Second)),
			},
			want: breathLively,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := breathRate(tc.rows, now); got != tc.want {
				t.Errorf("breathRate() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A breath, not a blink. A blink reads as an alarm, and there is nothing wrong
// with a session that is working.
func TestBreathIsARampAndReturnsToItsStart(t *testing.T) {
	t.Parallel()

	first := breathBrightness(0)
	if got := breathBrightness(breathFrames); got != first {
		t.Errorf("the cycle does not close: phase 0 = %q, phase %d = %q", first, breathFrames, got)
	}

	distinct := map[string]bool{}
	for phase := 0; phase < breathFrames; phase++ {
		distinct[breathBrightness(phase)] = true
	}
	if len(distinct) < 3 {
		t.Errorf("the cycle uses %d greys; a two-state cycle is a blink, not a breath", len(distinct))
	}
}

// The marker moves only for a session that is working. Everything else must be
// perfectly still, or the motion stops being a signal.
func TestOnlyAWorkingSessionMoves(t *testing.T) {
	t.Parallel()

	m := blockModel(t, nil)
	working := workingAt("a", at(0))
	idle := SessionRow{ID: "b", State: "running", Activity: "idle"}

	var workingSeen, idleSeen []string
	for phase := 0; phase < breathFrames*2; phase++ {
		m.breath = phase
		_, wc := m.activityGlyph(working)
		_, ic := m.activityGlyph(idle)
		workingSeen = append(workingSeen, string(wc))
		idleSeen = append(idleSeen, string(ic))
	}

	if allSame(workingSeen) {
		t.Errorf("a working session never moved across %d frames", len(workingSeen))
	}
	if !allSame(idleSeen) {
		t.Errorf("an idle session moved: %v", idleSeen)
	}
}

func allSame(values []string) bool {
	for _, v := range values {
		if v != values[0] {
			return false
		}
	}
	return true
}
