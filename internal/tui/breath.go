package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Breathing is the only animation in saint, and it is off far more than it is
// on.
//
// saint lives in a split pane, in the periphery, most of its life. Peripheral
// vision is close to colour-blind and extremely motion-sensitive, which is the
// wrong way round from how interfaces usually signal: colour at the edge of
// vision is nearly invisible, and motion at the edge is impossible to ignore.
// So the one channel worth spending on "something is happening" is movement.
//
// The discipline is what makes it work. The deck is completely still when
// nothing is working - no spinner, no blink, no cursor. When a session starts
// thinking, one cell begins to breathe. That is the whole vocabulary. Motion
// that is always present is wallpaper within a day, and then it signals
// nothing.
//
// It is also why this has its own clock rather than riding the refresh tick:
// the refresh shells out to the container CLI to rebuild the fleet, and doing
// that three times a second to animate one cell would cost more than the rest
// of the program put together. And when nothing is working the loop stops
// entirely, so an idle saint schedules no wakeups at all.

// breathMsg is the animation clock, distinct from the refresh tick.
type breathMsg time.Time

const (
	// breathFrames is the length of one cycle. Four steps is enough to read as
	// a breath rather than a blink, and short enough that the phase never
	// drifts far from what the terminal last drew.
	breathFrames = 4

	// A breath's rate says how recently the agent last produced tokens, which
	// is the difference between an agent mid-answer and one that has been
	// waiting on a slow tool call. Both are "working"; only one is moving.
	breathLively = 280 * time.Millisecond
	breathSlow   = 900 * time.Millisecond

	// livelyWithin is how recent the last metered token must be for the fast
	// rate. Past it the session is still working, just not streaming.
	livelyWithin = 5 * time.Second
)

// anyWorking reports whether a session is currently crossing the proxy.
//
// This is the switch for the whole animation: no working session, no clock, no
// wakeups.
func anyWorking(rows []SessionRow) bool {
	for _, row := range rows {
		if row.Activity == "working" {
			return true
		}
	}
	return false
}

// breathRate picks the interval for the next frame from the most recently
// active session.
//
// A rate is only claimed when it was measured. A session the proxy has metered
// carries LastAt; one it has not is still drawn as working - that much is
// known - but breathes at the slow rate rather than a guessed one.
func breathRate(rows []SessionRow, now time.Time) time.Duration {
	rate := breathSlow
	for _, row := range rows {
		if row.Activity != "working" || !row.UsageKnown || !row.Usage.Reported {
			continue
		}
		if row.Usage.LastAt.IsZero() {
			continue
		}
		if now.Sub(row.Usage.LastAt) < livelyWithin {
			rate = breathLively
		}
	}
	return rate
}

// breathCmd schedules the next frame, or nothing at all when the deck is still.
func breathCmd(rows []SessionRow, now time.Time) tea.Cmd {
	if !anyWorking(rows) {
		return nil
	}
	return tea.Tick(breathRate(rows, now), func(t time.Time) tea.Msg {
		return breathMsg(t)
	})
}

// breathBrightness is the colour the working marker is drawn in this frame.
//
// A ramp between two greens rather than an on/off blink. A blink reads as an
// alarm and there is nothing wrong with a session that is working; a breath
// reads as alive, which is the actual claim. It stays green throughout, because
// the colour says which state this is and the movement says it is still going -
// two channels carrying two different facts.
func breathBrightness(phase int) string {
	switch phase % breathFrames {
	case 1, 3:
		return colGood
	case 2:
		return colGoodDim
	default:
		return colInkBright
	}
}
