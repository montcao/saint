package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

func railModelAt(t *testing.T, width, height int) Model {
	t.Helper()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	m := newRenderModel(t, width, height)
	m.now = func() time.Time { return now }
	m.proxyStatus = "proxy: 192.168.64.1:8787 · subscription"
	m.auth.states = []CredentialState{
		{
			Kind:      CredentialKind{Name: "anthropic-login", Provider: "anthropic", Short: "plan login", Config: store.CredentialClaudeCode},
			Present:   true,
			ExpiresAt: now.Add(6 * time.Hour),
		},
		{Kind: CredentialKind{Name: "anthropic-token", Provider: "anthropic", Short: "plan token", Config: store.CredentialClaudeSubscription}},
		{Kind: CredentialKind{Name: "openai-key", Provider: "openai", Short: "api key", Config: store.CredentialOpenAI}},
	}
	return m
}

func TestRailTogglesWithS(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(Model)
	if m.mode != modeRail || !m.railOpen {
		t.Fatalf("s did not open the rail: mode=%v open=%v", m.mode, m.railOpen)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(Model)
	if m.mode != modeList || m.railOpen {
		t.Fatalf("s did not close the rail: mode=%v open=%v", m.mode, m.railOpen)
	}
}

// The rail was asked for; the sidebar is ambient. When both cannot fit, the
// sidebar is the one that yields.
func TestRailBeatsTheSidebar(t *testing.T) {
	t.Parallel()

	both := railWidth + minMainWidth + sidebarWidth
	layout := planLayout(both, true)
	if layout.left != railWidth || layout.right != sidebarWidth {
		t.Fatalf("layout at %d = %+v, want both columns", both, layout)
	}

	// One column short of holding both.
	tight := both - 1
	layout = planLayout(tight, true)
	if layout.left != railWidth {
		t.Errorf("layout at %d dropped the rail: %+v", tight, layout)
	}
	if layout.right != 0 {
		t.Errorf("layout at %d kept the sidebar instead of the rail: %+v", tight, layout)
	}
	if layout.main < minMainWidth {
		t.Errorf("layout at %d starved the main column: %+v", tight, layout)
	}
}

// A split pane is narrower than rail plus a usable list. Refusing there would
// make the rail unreachable exactly where saint is most used, so it opens over
// the list instead.
func TestRailOverlaysWhenTooNarrowToSitBeside(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, railWidth+minMainWidth-1, 30)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(Model)

	if m.mode != modeRail {
		t.Fatal("the rail did not open in a narrow terminal")
	}
	if !m.railOverlaid() {
		t.Error("the rail took a column it does not fit in")
	}
	if layout := m.layout(); layout.left != 0 {
		t.Errorf("an overlaid rail still took columns: %+v", layout)
	}

	view := m.View()
	if !strings.Contains(view, "AGENTS") {
		t.Errorf("the overlaid rail is not on screen:\n%s", view)
	}
	for i, line := range strings.Split(view, "\n") {
		if got := lipgloss.Width(line); got > m.width {
			t.Errorf("overlaid rail line %d is %d columns, terminal is %d", i, got, m.width)
		}
	}
}

// Wide enough, and it is a column beside the list rather than over it.
func TestRailIsAColumnWhenItFits(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, railWidth+minMainWidth, 30)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(Model)

	if m.railOverlaid() {
		t.Error("the rail overlaid a terminal wide enough to hold it")
	}
	if layout := m.layout(); layout.left != railWidth {
		t.Errorf("the rail did not take its column: %+v", layout)
	}
	if view := m.View(); !strings.Contains(view, "NAME") {
		t.Error("the list is not visible beside the rail")
	}
}

// While attached nearly every keystroke belongs to the agent, and the few
// exceptions are already reserved. The rail must not take another key or
// another column.
func TestRailIsNotAvailableWhileAttached(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	m.railOpen = true
	m.mode = modeAttached

	if m.railVisible() {
		t.Error("the rail is drawn over an attached session")
	}
	if layout := m.layout(); layout.left != 0 {
		t.Errorf("the rail took columns from an attached pane: %+v", layout)
	}
}

// The rail is what is true of the machine; the sidebar is the selected session.
// An earlier left pane was deleted for blurring that line.
func TestRailDoesNotRestateTheSidebar(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	m.railOpen = true
	m.mode = modeRail

	rail := m.renderRail(28)
	for _, sessionOnly := range []string{"alpha", "beta", "CONTEXT", "128.4k"} {
		if strings.Contains(rail, sessionOnly) {
			t.Errorf("the rail restates the sidebar's %q:\n%s", sessionOnly, rail)
		}
	}
	// It does carry what has no other home.
	for _, want := range []string{"AGENTS", "CREDENTIALS", "anthropic", "billing", "address", "WORKSPACE", "mounts", "SYSTEM"} {
		if !strings.Contains(rail, want) {
			t.Errorf("the rail is missing %q:\n%s", want, rail)
		}
	}
}

func TestRailRowsAct(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	m.railOpen = true
	m.mode = modeRail

	// A credential row goes to the screen that changes it.
	m.rail.selected = indexOfRailLabel(t, m, "plan login")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(Model).mode; got != modeAuth {
		t.Errorf("enter on a credential row = mode %v, want the auth screen", got)
	}

	// A config-backed row names where the value lives rather than pretending it
	// can be edited here.
	m.rail.selected = indexOfRailLabel(t, m, "mounts")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if status := next.(Model).status; !strings.Contains(status, "mount_roots") {
		t.Errorf("status = %q, want it to name the config key", status)
	}
}

func TestRailFilters(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	m.railOpen = true
	m.mode = modeRail

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(Model)
	for _, r := range "proxy" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}

	// "proxy" is a section heading now, so the filter has to match headings too
	// - it is the word a user is most likely to type.
	rows := m.railVisibleRows()
	if len(rows) == 0 {
		t.Fatal("filtering by a section heading returned nothing")
	}
	for _, row := range rows {
		if row.Action != railActionProxy {
			t.Errorf("filter admitted %q, which is not part of the proxy section", row.Label)
		}
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if len(m.railVisibleRows()) <= 1 {
		t.Error("esc did not clear the filter")
	}
	if m.mode != modeRail {
		t.Error("esc closed the rail instead of the filter")
	}
}

func TestRailViewFitsTheTerminal(t *testing.T) {
	t.Parallel()

	for _, width := range []int{railWidth + minMainWidth, 120, 150, 200} {
		m := railModelAt(t, width, 30)
		m.railOpen = true
		m.mode = modeRail

		for i, line := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("rail open at width %d: line %d is %d columns", width, i, got)
			}
		}
	}
}

// indexOfRailLabel indexes railEntries, not railVisibleRows: selection walks
// headings as well as rows, so an index into the rows alone points at the wrong
// line.
func indexOfRailLabel(t *testing.T, m Model, label string) int {
	t.Helper()

	for idx, entry := range m.railEntries() {
		if !entry.Heading && entry.Row.Label == label {
			return idx
		}
	}
	t.Fatalf("no rail row labelled %q", label)
	return 0
}

// An agent and a provider are different things. Listing "claude" beside
// "anthropic" and "openai" read as three vendors when Claude is Anthropic, and
// one of the three was a credential type rather than a vendor at all.
func TestRailGroupsAgentsSeparatelyFromProviders(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 32)
	m.auth.states = []CredentialState{
		{Kind: CredentialKind{Name: "anthropic-login", Provider: "anthropic", Short: "plan login"}, Present: true},
		{Kind: CredentialKind{Name: "anthropic-key", Provider: "anthropic", Short: "api key"}, Present: true},
		{Kind: CredentialKind{Name: "openai-key", Provider: "openai", Short: "api key"}},
	}
	m.railOpen = true
	m.mode = modeRail

	rail := m.renderRail(34)
	for _, want := range []string{"AGENTS", "claude", "codex", "CREDENTIALS", "anthropic", "openai", "plan login", "api key"} {
		if !strings.Contains(rail, want) {
			t.Errorf("rail is missing %q:\n%s", want, rail)
		}
	}

	// A credential row is labelled by what kind of credential it is; the
	// provider is the group it sits under, never repeated in the label.
	for _, row := range m.railCredentialRows() {
		if row.Group == "" {
			t.Errorf("credential row %q names no provider", row.Label)
		}
		if row.Label == row.Group {
			t.Errorf("credential row %q repeats its provider as its label", row.Label)
		}
	}

	// An agent row must not claim to be a credential.
	for _, section := range m.railSections() {
		if section.Title != railSectionAgents {
			continue
		}
		for _, row := range section.Rows {
			if row.Action == railActionAuth {
				t.Errorf("agent row %q acts as a credential", row.Label)
			}
		}
	}
}

// One section per provider did not scale: with two providers the rail already
// overran a 30-row terminal, and a third would push PROXY, WORKSPACE and
// SYSTEM off the bottom - the sections with no other way in.
func TestCredentialsAreOneGroupRegardlessOfProviderCount(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 34)
	m.auth.states = []CredentialState{
		{Kind: CredentialKind{Name: "anthropic-key", Provider: "anthropic", Short: "api key"}, Present: true},
		{Kind: CredentialKind{Name: "openai-key", Provider: "openai", Short: "api key"}},
		{Kind: CredentialKind{Name: "nous-key", Provider: "nous", Short: "api key"}},
		{Kind: CredentialKind{Name: "openrouter-key", Provider: "openrouter", Short: "api key"}},
	}

	// Four providers, still one section, and the section count is unchanged.
	sections := m.railSections()
	credentials := 0
	for _, section := range sections {
		if section.Title == railSectionCredentials {
			credentials++
		}
		if section.Title == "anthropic" || section.Title == "openai" || section.Title == "nous" {
			t.Errorf("provider %q became a section of its own again", section.Title)
		}
	}
	if credentials != 1 {
		t.Fatalf("credentials sections = %d, want exactly 1", credentials)
	}

	// Providers survive as sub-headings on the rows, so each credential still
	// says who issues it.
	providers := map[string]bool{}
	for _, row := range m.railCredentialRows() {
		providers[row.Group] = true
	}
	for _, want := range []string{"anthropic", "openai", "nous", "openrouter"} {
		if !providers[want] {
			t.Errorf("no credential row grouped under %q", want)
		}
	}
}

// Hermes is provider-agnostic by design, so it cannot be pinned to one, and
// saint has no image for it. Both have to be said rather than implied.
func TestHermesIsListedHonestly(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 34)
	var hermes railRow
	for _, row := range m.railAgentRows() {
		if row.Label == "hermes" {
			hermes = row
		}
	}
	if hermes.Label == "" {
		t.Fatal("hermes is not listed as an agent")
	}
	// saint cannot run it yet, and the row says so rather than implying a build
	// away or leaving the agent out entirely.
	if hermes.Value != "planned" {
		t.Errorf("hermes row = %q, want it to say it is not integrated yet", hermes.Value)
	}
	// The consequence of an agent choosing its own provider is that saint may
	// not be in the path at all.
	for _, want := range []string{"unmetered", "uncustodied"} {
		if !strings.Contains(hermes.Detail, want) {
			t.Errorf("hermes detail = %q, want it to mention %q", hermes.Detail, want)
		}
	}
}

func TestRailFoldsAndRemembers(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 34)
	m.railOpen = true
	m.mode = modeRail
	m.rail.selected = indexOfRailHeading(t, m, railSectionFleet)

	before := len(m.railEntries())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if !m.railCollapsed(railSectionFleet) {
		t.Fatal("enter on a heading did not fold it")
	}
	if got := len(m.railEntries()); got >= before {
		t.Errorf("entries = %d after folding, want fewer than %d", got, before)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m := next.(Model); m.railCollapsed(railSectionFleet) {
		t.Error("enter did not unfold it again")
	}
}

// Hiding a match because of a fold the user set earlier would look like the
// search was broken.
func TestFilterReachesIntoFoldedGroups(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 34)
	m.railOpen = true
	m.mode = modeRail
	m.railFolds = []string{railSectionFleet}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(Model)
	for _, r := range "stopped" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}

	found := false
	for _, row := range m.railVisibleRows() {
		if row.Label == "stopped" {
			found = true
		}
	}
	if !found {
		t.Error("a match inside a folded group was hidden by the fold")
	}
}

func indexOfRailHeading(t *testing.T, m Model, section string) int {
	t.Helper()

	for idx, entry := range m.railEntries() {
		if entry.Heading && entry.Section == section {
			return idx
		}
	}
	t.Fatalf("no rail heading for %q", section)
	return 0
}

// The attention colour is the only saturated thing saint draws, and the rail is
// where it is easiest to spend badly: every row here is a true statement about
// the machine, and ranking true statements by hue is what taught the eye to
// read the rail as a wall of status lights. A healthy machine must produce a
// rail with no colour in it at all - that is the state the whole palette is
// built to make recognisable across a room.
func TestRailSpendsNoAttentionOnAHealthyMachine(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	for _, section := range m.railSections() {
		for _, row := range section.Rows {
			if row.State == railNeedsPerson {
				t.Errorf("%s/%s = %q asks for a person on a machine where nothing is wrong",
					section.Title, row.Label, row.Value)
			}
		}
	}
}

// And the mirror: every state that genuinely needs somebody has to reach the
// colour, because these are the ones that fail silently - inside a container,
// hours later, with nobody watching.
func TestRailSpendsAttentionOnEveryStateNeedingAPerson(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	m := railModelAt(t, 150, 30)
	m.proxyStatus = "proxy blocked: 10.0.0.1:8787 (firewall? run saint doctor)"
	m.counts.Missing = 1
	m.orphans = []sandbox.Info{{Name: "saint-deadbeef"}}
	m.auth.states = []CredentialState{
		{
			Kind:      CredentialKind{Name: "anthropic-login", Provider: "anthropic", Short: "plan login", Config: store.CredentialClaudeCode},
			Present:   true,
			ExpiresAt: now.Add(-time.Minute),
		},
		{
			Kind: CredentialKind{Name: "openai-key", Provider: "openai", Short: "api key", Config: store.CredentialOpenAI},
			Err:  errors.New("keychain is locked"),
		},
	}

	byLabel := map[string]railRow{}
	for _, section := range m.railSections() {
		for _, row := range section.Rows {
			byLabel[row.Label] = row
		}
	}

	for _, label := range []string{"address", "missing", "orphans", "plan login", "api key"} {
		row, ok := byLabel[label]
		if !ok {
			t.Fatalf("no rail row labelled %q", label)
		}
		// Amber or red - both ask for a person. Red is the stronger reading,
		// for something that has already stopped working rather than something
		// about to: an expired credential is both broken and a thing to fix,
		// and splitting those into separate colours would leave the reader
		// deciding which of two colours means "do something".
		if row.State != railNeedsPerson && row.State != railBroken {
			t.Errorf("%q = %q is not asking for a person", label, row.Value)
		}
		// Colour is never the only carrier: a terminal theme may render any
		// index as anything, and this row has to survive that.
		if strings.TrimSpace(row.Value) == "" {
			t.Errorf("%q says nothing in words", label)
		}
	}

	// An expired login says "expired" rather than the "expired left" that
	// appending to formatLife used to produce.
	if got := byLabel["plan login"].Value; got != "expired" {
		t.Errorf("expired login row = %q, want %q", got, "expired")
	}
}

// A credential nobody has set is not a fault. saint knows about four kinds and
// most people use one or two, so colouring the unused ones would put permanent
// amber in a rail that is working exactly as intended - and a colour that is
// always on screen is one the reader stops seeing.
func TestUnsetCredentialsAreInformationNotAttention(t *testing.T) {
	t.Parallel()

	m := railModelAt(t, 150, 30)
	for _, row := range m.railCredentialRows() {
		if row.Value != "not set" {
			continue
		}
		if row.State == railNeedsPerson {
			t.Errorf("credential row %q asks for a person just for being unset", row.Label)
		}
		return
	}
	t.Fatal("no unset credential row to check")
}

// Asserting the meaning rather than the index: the palette is allowed to change
// what colour 214 is, and this test should only fail if a rail row stops
// reaching for the attention colour at all.
func TestRailValueStyleMapsStateToThePalette(t *testing.T) {
	t.Parallel()

	if got := railValueStyle(railNeedsPerson).GetForeground(); got != lipgloss.Color(colAttention) {
		t.Errorf("a row needing a person renders %v, want the attention colour", got)
	}
	if got := railValueStyle(railReports).GetForeground(); got != lipgloss.Color(colInk) {
		t.Errorf("a reporting row renders %v, want an ordinary value on the grey ramp", got)
	}
}
