package tui

import (
	"strconv"
	"time"
)

// Change marking exists because saint's whole stated job - spot the one session
// that changed - is the one thing the interface did nothing to help with.
//
// The list redraws in full every few seconds. A full repaint does not merely
// fail to show a change, it destroys the signal: every row is equally new, so
// the reader has to re-read the whole fleet and compare against memory. Change
// blindness is reliable enough that people miss large changes across a repaint
// even while looking for them.
//
// So saint remembers what the fleet was last time it drew, and marks what moved.

// markTTL is how long a change stays marked when nobody touches the keyboard.
//
// It is a bound rather than a schedule. Marks are cleared when the user presses
// a key, because that is the moment saint knows somebody is present and looking;
// the timer is only there so that coming back after lunch does not present an
// hour of accumulated marks as if they were news.
const markTTL = 90 * time.Second

// changeKind is what moved. It is a word rather than a colour because a change
// the reader cannot name is just a flicker.
type changeKind string

const (
	changeAppeared changeKind = "new"
	changeStarted  changeKind = "started"
	changeFinished changeKind = "went idle"
	changeStopped  changeKind = "stopped"
)

// changeMark is one noticed transition.
type changeMark struct {
	Kind changeKind
	At   time.Time
}

// sessionState is the part of a row a change is measured against. Comparing
// whole rows would mark a session every time its age string ticked over.
type sessionState struct {
	Activity string
	State    string
}

func stateOf(row SessionRow) sessionState {
	return sessionState{Activity: row.Activity, State: row.State}
}

// noteChanges compares the incoming fleet against the last one and returns the
// marks to show, keeping any earlier mark the user has not seen yet.
//
// The first snapshot of a session produces no "new" mark. Everything is new
// when saint starts, and a screen where every row is marked is a screen with no
// marks on it.
func noteChanges(previous map[string]sessionState, marks map[string]changeMark, rows []SessionRow, now time.Time) (map[string]sessionState, map[string]changeMark) {
	current := make(map[string]sessionState, len(rows))
	next := make(map[string]changeMark, len(marks))

	// Carry forward marks that are still within their window.
	for id, mark := range marks {
		if now.Sub(mark.At) < markTTL {
			next[id] = mark
		}
	}

	first := len(previous) == 0
	for _, row := range rows {
		state := stateOf(row)
		current[row.ID] = state

		was, known := previous[row.ID]
		switch {
		case first:
			// saint has just started. Nothing has changed yet; it has only
			// been seen for the first time.
		case !known:
			next[row.ID] = changeMark{Kind: changeAppeared, At: now}
		case was.State == "running" && state.State != "running":
			next[row.ID] = changeMark{Kind: changeStopped, At: now}
		case was.Activity != "working" && state.Activity == "working":
			next[row.ID] = changeMark{Kind: changeStarted, At: now}
		case was.Activity == "working" && state.Activity == "idle":
			// The one a person actually waits for: an agent that was thinking
			// and now is not.
			next[row.ID] = changeMark{Kind: changeFinished, At: now}
		}
	}

	// A session that is gone cannot carry a mark into a list it is not in.
	for id := range next {
		if _, ok := current[id]; !ok {
			delete(next, id)
		}
	}
	return current, next
}

// markFor reports a session's unseen change, if it has one.
func (m Model) markFor(id string) (changeMark, bool) {
	mark, ok := m.marks[id]
	return mark, ok
}

// seeChanges clears every mark.
//
// Called on a keypress, which is the only moment saint can honestly infer that
// somebody is in front of the terminal. A mark's job is finished once it has
// been looked at, and marks that outlive the glance become decoration.
func (m *Model) seeChanges() {
	if len(m.marks) > 0 {
		m.marks = map[string]changeMark{}
	}
}

// humanDuration renders an elapsed span without the "ago" that humanAge adds,
// for places where the sentence already supplies the tense - "idle 12m".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return itoaMin(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return itoaMin(int(d.Hours())) + "h"
	default:
		return itoaMin(int(d.Hours()/24)) + "d"
	}
}

func itoaMin(n int) string {
	if n < 0 {
		n = 0
	}
	return strconv.Itoa(n)
}
