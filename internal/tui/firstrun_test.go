package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// firstRunText renders the surface and strips styling, so an assertion about
// what a user reads cannot be satisfied by a colour.
//
// It matters that these tests read the plain text. Under `go test` there is no
// TTY, so lipgloss renders no escapes at all and a test that merely searched
// the raw output would pass even if the words had been replaced by colour. The
// strip makes the intent explicit rather than accidental.
func firstRunText(t *testing.T, width, height int, state firstRunState) string {
	t.Helper()
	return stripEscapes(renderFirstRun(width, height, state))
}

func firstRunReadyState() firstRunState {
	return firstRunState{
		Credential: firstRunReady,
		Proxy:      firstRunReady,
		Runtime:    firstRunReady,
	}
}

// The blocking step has to be readable as words. A terminal theme may paint
// every ANSI index the same grey, and a user who cannot see the amber must
// still be told which step is in the way and what ignoring it costs.
func TestFirstRunNamesTheBlockingStepInWords(t *testing.T) {
	t.Parallel()

	state := firstRunReadyState()
	state.Credential = firstRunMissing

	got := firstRunText(t, 90, 24, state)
	for _, want := range []string{
		"no credential stored",
		"none stored",
		"first request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing credential state does not say %q:\n%s", want, got)
		}
	}
}

// The reason the credential is named first, and the failure that ordering
// prevents: a session created without one starts, and only then fails.
func TestFirstRunPutsCredentialBeforeTheOtherBlockers(t *testing.T) {
	t.Parallel()

	// Everything is missing at once, which is what a machine that has just
	// installed saint actually looks like. The surface must still name one
	// step, and it must be the credential - the other two fail immediately and
	// visibly, while this one fails minutes later inside the agent.
	state := firstRunState{
		Credential: firstRunMissing,
		Proxy:      firstRunMissing,
		Runtime:    firstRunMissing,
	}

	step, blocked := state.blocker()
	if !blocked {
		t.Fatal("blocker() found nothing blocking with all three steps missing")
	}
	if step.label != "credential" {
		t.Errorf("blocker() = %q, want the credential to come first", step.label)
	}

	got := firstRunText(t, 90, 24, state)
	if !strings.Contains(got, "no credential stored") {
		t.Errorf("headline does not name the credential:\n%s", got)
	}
	// The other two are still reported in the readout - they are true - but
	// they must not be dressed up as the thing to do next.
	if strings.Contains(got, "proxy not running") {
		t.Errorf("a second step is competing for the headline:\n%s", got)
	}
}

// A missing container runtime is a different failure with a different fix, and
// pressing `a` will not help. The surface has to say so.
func TestFirstRunReportsAMissingContainerRuntime(t *testing.T) {
	t.Parallel()

	state := firstRunReadyState()
	state.Runtime = firstRunMissing

	step, blocked := state.blocker()
	if !blocked || step.label != "container runtime" {
		t.Fatalf("blocker() = %q, %v; want the container runtime", step.label, blocked)
	}

	got := firstRunText(t, 90, 24, state)
	for _, want := range []string{"container runtime", "not found", "microVM"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing runtime state does not say %q:\n%s", want, got)
		}
	}
}

// `unknown` is an answer, and it is not the same answer as "not ready".
//
// The zero value of firstRunState is the not-yet-probed one, which is the case
// this guards: a caller that has not finished looking must never cause the
// surface to assert that a credential is absent or that a runtime is missing.
func TestFirstRunUnknownIsNotAFailure(t *testing.T) {
	t.Parallel()

	var state firstRunState // every field unknown

	if step, blocked := state.blocker(); blocked {
		t.Errorf("blocker() reported %q blocking on a state nobody has checked", step.label)
	}

	got := firstRunText(t, 90, 24, state)
	if !strings.Contains(got, "unknown") {
		t.Errorf("unchecked state does not say unknown:\n%s", got)
	}
	for _, forbidden := range []string{"none stored", "not found", "not running"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("unchecked state claims %q, which saint has not established:\n%s", forbidden, got)
		}
	}

	// And it must not read as a green light either.
	if _, _, needsPerson := state.guidance(); needsPerson {
		t.Error("guidance() treats an unchecked state as blocking")
	}
}

// A ready machine gets a frame with nothing asking for attention.
//
// The escape codes cannot be asserted directly here - there is no TTY under
// `go test`, so lipgloss emits none - so this asserts the decision that gates
// them. needsPerson false is exactly the condition under which colAttention is
// never applied, and a frame with no colour in it is how saint says "nothing
// needs you" from across a room.
func TestFirstRunReadyStateAsksForNothing(t *testing.T) {
	t.Parallel()

	state := firstRunReadyState()

	if step, blocked := state.blocker(); blocked {
		t.Errorf("blocker() reported %q on a fully ready machine", step.label)
	}
	headline, why, needsPerson := state.guidance()
	if needsPerson {
		t.Error("guidance() wants a person on a fully ready machine")
	}
	if headline != "" {
		t.Errorf("guidance() headline = %q, want nothing shouting on a ready machine", headline)
	}
	if why == "" {
		t.Error("guidance() left a ready machine with no next step")
	}

	got := firstRunText(t, 90, 24, state)
	for _, want := range []string{"stored", "found", "running", "n spawn"} {
		if !strings.Contains(got, want) {
			t.Errorf("ready state does not say %q:\n%s", want, got)
		}
	}
}

// Keys, not intents. The reader is looking at a keyboard.
func TestFirstRunNamesKeysNotIntents(t *testing.T) {
	t.Parallel()

	got := firstRunText(t, 90, 24, firstRunReadyState())
	for _, want := range []string{"a auth", "n spawn"} {
		if !strings.Contains(got, want) {
			t.Errorf("key row does not name %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"create a session", "create a new session", "press the"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("key row describes an intent (%q) instead of a key:\n%s", forbidden, got)
		}
	}
}

func firstRunStateCases() []struct {
	name  string
	state firstRunState
} {
	blockedCredential := firstRunReadyState()
	blockedCredential.Credential = firstRunMissing

	blockedRuntime := firstRunReadyState()
	blockedRuntime.Runtime = firstRunMissing

	blockedProxy := firstRunReadyState()
	blockedProxy.Proxy = firstRunMissing

	partial := firstRunReadyState()
	partial.Proxy = firstRunUnknown

	return []struct {
		name  string
		state firstRunState
	}{
		{"ready", firstRunReadyState()},
		{"no credential", blockedCredential},
		{"no runtime", blockedRuntime},
		{"no proxy", blockedProxy},
		{"unknown", firstRunState{}},
		{"partly unknown", partial},
	}
}

// A line wider than the terminal wraps, and a wrapped frame in the alternate
// screen reads as a corrupted one rather than a narrow one. This is the first
// frame a new user ever sees, so it is the worst possible place for it.
func TestFirstRunNeverExceedsWidth(t *testing.T) {
	t.Parallel()

	widths := []int{1, 8, 20, 24, 27, 32, 40, 49, 50, 63, 64, 75, 76, 80, 97, 120, 200}
	heights := []int{0, 1, 3, 6, 10, 24, 50}

	for _, tc := range firstRunStateCases() {
		for _, width := range widths {
			for _, height := range heights {
				out := renderFirstRun(width, height, tc.state)
				for i, line := range strings.Split(out, "\n") {
					if got := lipgloss.Width(line); got > width {
						t.Errorf("%s at %dx%d: line %d is %d columns:\n%q",
							tc.name, width, height, i, got, line)
					}
				}
			}
		}
	}
}

// The surface fills exactly the box it was given, so the frame around it keeps
// its shape. A surface that is one line too tall pushes the status bar off the
// bottom of the terminal.
func TestFirstRunFillsItsBoxExactly(t *testing.T) {
	t.Parallel()

	for _, tc := range firstRunStateCases() {
		for _, height := range []int{1, 2, 4, 7, 12, 24, 60} {
			got := len(strings.Split(renderFirstRun(80, height, tc.state), "\n"))
			if got != height {
				t.Errorf("%s at height %d rendered %d lines", tc.name, height, got)
			}
		}
	}
}

// Content is dropped whole rather than squeezed, and the keys are the last
// thing to go: a frame that tells a new user nothing about how to proceed is
// worse than one missing its explanation.
func TestFirstRunDropsContentBeforeItDropsTheKeys(t *testing.T) {
	t.Parallel()

	state := firstRunReadyState()

	tall := firstRunText(t, 80, 24, state)
	if !strings.Contains(tall, "container runtime") {
		t.Errorf("a tall terminal lost the readiness readout:\n%s", tall)
	}

	short := firstRunText(t, 80, 2, state)
	if strings.Contains(short, "container runtime") {
		t.Errorf("a two-line box kept the readout instead of dropping it:\n%q", short)
	}
	if !strings.Contains(short, "n spawn") {
		t.Errorf("a two-line box dropped the keys:\n%q", short)
	}
}

// A narrow terminal loses features rather than legibility, so the tagline has
// shorter true forms rather than a truncated one. Nothing here may say more
// than the README does.
func TestFirstRunTaglineShortensRatherThanTruncates(t *testing.T) {
	t.Parallel()

	for _, width := range []int{1, 10, 26, 27, 40, 49, 50, 75, 76, 120} {
		tagline := firstRunTagline(width)
		if lipgloss.Width(tagline) > width {
			t.Errorf("tagline at width %d is %d columns: %q", width, lipgloss.Width(tagline), tagline)
		}
		if strings.Contains(tagline, "…") {
			t.Errorf("tagline at width %d was truncated rather than shortened: %q", width, tagline)
		}
	}

	if firstRunTagline(26) != "" {
		t.Error("tagline should be dropped entirely rather than shrink past legibility")
	}
}

// A zero or negative width is a real state during startup, before the first
// WindowSizeMsg arrives. It must not panic and must not emit anything.
func TestFirstRunHandlesNoRoomAtAll(t *testing.T) {
	t.Parallel()

	for _, width := range []int{-5, 0} {
		if got := renderFirstRun(width, 24, firstRunReadyState()); got != "" {
			t.Errorf("width %d rendered %q, want nothing", width, got)
		}
	}
}

// Every state of every step has words attached. This is the mechanical half of
// "colour is never the only carrier": if a branch ever returned an empty
// string, the only remaining difference between two states would be the hue.
func TestFirstRunEveryStepStateHasWords(t *testing.T) {
	t.Parallel()

	for _, at := range []firstRunReadiness{firstRunUnknown, firstRunReady, firstRunMissing} {
		state := firstRunState{Credential: at, Proxy: at, Runtime: at}
		for _, step := range state.steps() {
			if strings.TrimSpace(step.value()) == "" {
				t.Errorf("step %q has no words for readiness %d", step.label, at)
			}
		}
	}
}

// The three readiness values must each come from evidence. "saint has not
// looked" and "saint looked and it is not there" are different answers, and
// rendering them the same sends a user to fix something that may be fine.
func TestFirstRunStateDistinguishesUncheckedFromAbsent(t *testing.T) {
	t.Parallel()

	base := func() Model { return blockModel(t, nil) }

	t.Run("nothing probed yet is unknown, not missing", func(t *testing.T) {
		t.Parallel()
		got := base().firstRun()
		if got.Credential != firstRunUnknown || got.Proxy != firstRunUnknown || got.Runtime != firstRunUnknown {
			t.Errorf("a model that has probed nothing reported %+v, want all unknown", got)
		}
	})

	t.Run("a credential probe that failed is unknown", func(t *testing.T) {
		t.Parallel()
		m := base()
		m.auth.loadErr = errFirstRunProbe
		if got := m.firstRun().Credential; got != firstRunUnknown {
			t.Errorf("Credential = %v, want unknown: saint could not tell", got)
		}
	})

	t.Run("probed and absent is missing", func(t *testing.T) {
		t.Parallel()
		m := base()
		m.auth.states = []CredentialState{{Present: false}}
		if got := m.firstRun().Credential; got != firstRunMissing {
			t.Errorf("Credential = %v, want missing", got)
		}
	})

	t.Run("any present credential is ready", func(t *testing.T) {
		t.Parallel()
		m := base()
		m.auth.states = []CredentialState{{Present: false}, {Present: true}}
		if got := m.firstRun().Credential; got != firstRunReady {
			t.Errorf("Credential = %v, want ready", got)
		}
	})

	t.Run("proxy readiness follows its own status line", func(t *testing.T) {
		t.Parallel()
		for status, want := range map[string]firstRunReadiness{
			"":                                firstRunUnknown,
			"proxy: 192.0.2.1:8787 · key":     firstRunReady,
			"proxy off: no container network": firstRunMissing,
			"proxy blocked: 192.0.2.1:8787":   firstRunMissing,
			"proxy: external 192.0.2.1:8787":  firstRunReady,
		} {
			m := base()
			m.proxyStatus = status
			if got := m.firstRun().Proxy; got != want {
				t.Errorf("proxyStatus %q gave %v, want %v", status, got, want)
			}
		}
	})

	t.Run("a listed fleet is the evidence the runtime exists", func(t *testing.T) {
		t.Parallel()
		m := base()
		m.sawSnapshot = true
		if got := m.firstRun().Runtime; got != firstRunReady {
			t.Errorf("Runtime = %v, want ready after a successful snapshot", got)
		}
	})
}

var errFirstRunProbe = errProbe{}

type errProbe struct{}

func (errProbe) Error() string { return "keychain is locked" }

// The first-run screen replaces the list only when there is genuinely nothing
// to list. A filter that matches nothing is a different situation and must not
// be dressed up as a fresh install.
func TestFirstRunShowsOnlyWhenThereIsNothingToList(t *testing.T) {
	t.Parallel()

	// The tagline is the one line every variant carries, whatever the
	// readiness. Asserting a phrase from one variant would pass or fail on
	// which state the model happened to be in.
	const always = "a control plane for AI coding agents"

	empty := blockModel(t, nil)
	empty.width, empty.height = 120, 40
	if !strings.Contains(stripEscapes(empty.View()), always) {
		t.Errorf("an empty saint did not show the first-run screen:\n%s", stripEscapes(empty.View()))
	}

	withRows := blockModel(t, []SessionRow{{ID: "a", Name: "alpha", State: "running", Activity: "idle"}})
	withRows.width, withRows.height = 120, 40
	if strings.Contains(stripEscapes(withRows.View()), always) {
		t.Error("the first-run screen appeared over a fleet that has sessions")
	}

	filtered := blockModel(t, nil)
	filtered.width, filtered.height = 120, 40
	filtered.filterInput.SetValue("nothing-matches-this")
	if strings.Contains(stripEscapes(filtered.View()), always) {
		t.Error("a filter that matched nothing was rendered as a fresh install")
	}
}

// The screen has to fit the space it was given, or it pushes the status bar off
// the bottom of the terminal - which is where every escape key is documented.
func TestFirstRunDoesNotPushTheStatusBarOffScreen(t *testing.T) {
	t.Parallel()

	for _, height := range []int{18, 24, 40, 60} {
		m := blockModel(t, nil)
		m.width, m.height = 120, height
		if got := lipgloss.Height(m.View()); got > height {
			t.Errorf("height %d: View() rendered %d lines", height, got)
		}
	}
}
