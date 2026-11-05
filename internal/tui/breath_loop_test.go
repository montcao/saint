package tui

import (
	"testing"
	"time"
)

// Proves the loop actually stops: a working deck schedules frames, and the
// frame after the work ends schedules nothing.
func TestBreathLoopStopsWhenWorkEnds(t *testing.T) {
	m := blockModel(t, []SessionRow{workingAt("a", at(0))})
	m.now = func() time.Time { return at(0) }

	next, cmd := m.Update(breathMsg(at(0)))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a working deck did not schedule the next frame")
	}
	if !m.breathing {
		t.Error("breathing flag is false while a frame is scheduled")
	}
	startPhase := m.breath

	m.rows[0].Activity = "idle"
	next, cmd = m.Update(breathMsg(at(1)))
	m = next.(Model)
	if cmd != nil {
		t.Error("the deck kept animating after the work ended")
	}
	if m.breathing {
		t.Error("breathing flag stayed true with no frame scheduled")
	}
	if m.breath == startPhase {
		t.Error("the phase did not advance")
	}
}
