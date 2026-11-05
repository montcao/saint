package tui

// The palette.
//
// Three colours carry state and nothing else carries any.
//
// An earlier version of this file spent a single amber on "needs a person" and
// drew every other state on the grey ramp, on the theory that colour is a
// signal only while it is rare. The theory was sound and the result was not:
// running and stopped differed only by a word, and a fleet read as one
// undifferentiated block. Rarity is not the only thing that makes a signal -
// being able to tell two states apart without reading is the other, and it is
// what a readout is for.
//
// So green means running, amber means something wants a person, red means
// stopped or failed. What survives from the one-colour discipline is the half
// that was actually load-bearing:
//
//   - Colour is never the only carrier. Every coloured state is also stated in
//     words, because a terminal theme may render any index as anything.
//   - Colour is spent only on state. Not on selection, not on borders, not on a
//     label that is merely true. Selection is a light grey ground; a focused
//     panel is a brighter grey border. Those were hues once, and taking the hue
//     away from them is what leaves the three below meaning something.
//
// To change any of it, change the constants here. Nothing outside this file
// names an ANSI index.
const (
	// Three state colours, spent only on state: green for healthy and running,
	// amber for something that wants a person, red for something that has
	// stopped or failed.
	//
	// They are read at a glance and across a room, which is what the grey ramp
	// alone could not do. The discipline that survives from the one-colour
	// palette is the important half: colour is never the only carrier, so every
	// coloured state still says what it is in words, and colour is still spent
	// only on state - not on selection, not on borders, not on a label that is
	// merely true.
	colGood      = "78"  // running, healthy, live
	colGoodDim   = "71"  // the trough of a working session's breath
	colAttention = "214" // needs a person: blocked, expiring, near-full
	colBad       = "203" // stopped, expired, failed, unreachable

	// The grey ramp, brightest to darkest.
	colInkBright = "255" // the one strongest thing on a surface
	colInk       = "252" // ordinary values
	colInkSoft   = "248" // values that are present but not the point
	colMuted     = "244" // labels, hints, everything ambient
	colDim       = "240" // collapsed rows: present, not asking
	colRule      = "238" // borders, separators
	colSunken    = "236" // the status bar's ground

	// colOnDark is text drawn on a light background, which is only ever the
	// selected row.
	colOnDark = "16"

	// colSelect is the selected row's ground. Grey rather than indigo: a
	// selection is a statement about where the cursor is, not about whether
	// anything is wrong, and it used to be the loudest thing on screen.
	colSelect = "246"

	// Meter fills. The bar is a measurement rather than decoration, so it is
	// drawn on the grey ramp until it is telling you something: a context
	// window near full is one of the few things that genuinely needs a person.
	barCalmFill = "#8a8a8a"
	barWarnFill = "#ffaf00"
	barTrack    = "#3a3a3a"
)
