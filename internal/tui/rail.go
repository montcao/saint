package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// railWidth is fixed. A proportional rail would reflow its values on every
// resize, and these are read by position rather than by reading the label.
const railWidth = 30

// railInner is the text width inside the rail's border and padding.
const railInner = railWidth - 4

// railState is what a row is asking of the person reading it.
//
// Two readings, not the four this had. Three of the old four - good, warn, bad -
// were all rankings of things that are merely true: a credential that is set, an
// agent that is running, a proxy that answers. Ranking those by hue taught the
// eye to sort the rail by colour, and then the rows that actually needed
// somebody arrived in the same channel as the rows that did not. A row either
// wants a person or it is information, and only the first kind gets a colour.
type railState int

const (
	// railReports is a value that is simply true. Most of the rail is this.
	railReports railState = iota
	// railNeedsPerson is a value the user has to do something about: a
	// credential that has lapsed, a proxy no guest can reach, debris to clear.
	// Every one of them also says so in words, because a terminal theme may
	// render any colour as anything.
	railNeedsPerson
	// railHealthy is a value that is true and worth seeing at a glance: a live
	// credential, a reachable proxy, a running agent. It reads green.
	//
	// It is separate from railReports because most of the rail is neither -
	// a port number and a network name are facts with no health to report, and
	// colouring them would make the colours mean "this row exists".
	railHealthy
	// railBroken is a value that has already stopped working. It reads red.
	//
	// It is the stronger reading of railNeedsPerson rather than a different
	// axis: an expired credential is both broken and a thing to fix, and
	// splitting those apart would leave the reader deciding which of two
	// colours means "do something". Red is "this has failed", amber is "this
	// will fail", and both want a person.
	railBroken
)

// railAction is what enter does on a row.
//
// The rail summarises and sends you somewhere; it is never a second editor for
// something another screen already owns.
type railAction int

const (
	railActionNone railAction = iota
	// railActionAuth opens the credentials screen.
	railActionAuth
	// railActionRefresh reloads live state.
	railActionRefresh
	// railActionNote puts the row's detail in the status bar - used for values
	// that live in config.json and have no editor yet.
	railActionNote
	// railActionProxy opens the proxy screen.
	railActionProxy
)

type railRow struct {
	Label string

	// Group is a sub-heading this row sits under inside its section, used to
	// gather credentials by provider without giving each provider a section of
	// its own.
	Group string

	Value  string
	State  railState
	Action railAction
	// Detail is what railActionNote shows, typically where the value lives.
	Detail string
}

type railSection struct {
	Title string
	Rows  []railRow
	// Collapsible sections can be folded away, and the fold is remembered.
	Collapsible bool
}

// railModel is the control rail: a state index, not a menu of verbs.
//
// The header already lists the keys for the current mode, and the sidebar
// already describes the selected session. This surface exists for everything
// that is true of the whole machine and had nowhere to be said - which
// credential is in use, what the proxy is doing, where sessions may be mounted
// from. Every row states what is currently true; acting on it goes to where
// that truth is changed.
type railModel struct {
	selected  int
	filter    textinput.Model
	filtering bool
}

func newRailModel() railModel {
	filter := textinput.New()
	filter.Prompt = "/ "
	filter.Placeholder = "filter"
	filter.CharLimit = 40
	filter.Width = railWidth - 7
	return railModel{filter: filter}
}

// railVisible reports whether the rail should take columns.
//
// Never while attached: nearly every keystroke belongs to the agent then, and
// the few exceptions (detach, force quit, scrollback) are already reserved
// and already on screen. Another chord to open a launcher over a running
// agent would cost the agent a key permanently.
func (m Model) railVisible() bool {
	return m.railOpen && m.mode != modeAttached && m.railFits()
}

// railOverlaid reports whether the rail is showing over the list rather than
// beside it.
//
// A narrow terminal - a split pane, most often - cannot hold the rail and a
// usable list side by side. Refusing to open would make the rail unreachable
// exactly where saint is most used, so it opens over the list instead, the way
// the other full-screen surfaces already do. The information is the same; only
// the geometry changes.
func (m Model) railOverlaid() bool {
	return m.mode == modeRail && !m.railFits()
}

// railFits reports whether the terminal is wide enough to open the rail.
func (m Model) railFits() bool {
	return m.width >= railWidth+minMainWidth
}

// railSections builds the index from live state.
//
// Nothing here may restate the sidebar. The rail is what is true of the
// machine; the sidebar is what is true of the selected session. An earlier left
// pane was removed for blurring exactly that line.
func (m Model) railSections() []railSection {
	sections := []railSection{
		{Title: railSectionAgents, Rows: m.railAgentRows()},
		{Title: railSectionCredentials, Rows: m.railCredentialRows(), Collapsible: true},
		{Title: railSectionProxy, Rows: m.railProxyRows(), Collapsible: true},
		{Title: railSectionWorkspace, Rows: m.railWorkspaceRows(), Collapsible: true},
		{Title: railSectionFleet, Rows: m.railFleetRows(), Collapsible: true},
		{Title: railSectionSystem, Rows: m.railSystemRows(), Collapsible: true},
	}
	return sections
}

// Section titles are constants because the collapsed set is persisted by title.
const (
	railSectionAgents      = "agents"
	railSectionCredentials = "credentials"
	railSectionProxy       = "proxy"
	railSectionWorkspace   = "workspace"
	railSectionFleet       = "fleet"
	railSectionSystem      = "system"
)

// providerAny marks an agent that is not tied to one provider.
const providerAny = "any"

// railAgent is a runtime saint can run in a session.
//
// An agent and a provider are different things, and conflating them is what
// made the first version of this list confusing: Claude Code is an agent,
// Anthropic is who it authenticates to. Adding a runtime is one entry here.
type railAgent struct {
	Name string
	// Runtime is empty for an agent saint knows about but cannot yet run.
	Runtime sandbox.Runtime
	// Provider is who it authenticates to, or providerAny when the agent
	// chooses at runtime.
	Provider string
}

var railAgents = []railAgent{
	{Name: "claude", Runtime: sandbox.RuntimeClaude, Provider: "anthropic"},
	{Name: "codex", Runtime: sandbox.RuntimeCodex, Provider: "openai"},
	// Hermes is provider-agnostic by design - it switches model providers at
	// runtime with no code change - so it cannot be pinned to one. It is listed
	// as a planned runtime rather than left out entirely: an agent saint means
	// to support is worth stating, and the row says plainly that it cannot run
	// one yet rather than implying a build away.
	{Name: "hermes", Provider: providerAny},
}

// railAgentRows report each runtime, how many sessions use it, and who it pays.
func (m Model) railAgentRows() []railRow {
	counts := map[string]int{}
	for _, row := range m.rows {
		if !row.Orphan {
			counts[row.Runtime]++
		}
	}

	rows := make([]railRow, 0, len(railAgents))
	for _, agent := range railAgents {
		row := railRow{Label: agent.Name, Action: railActionNote}

		switch {
		case agent.Runtime == "":
			row.Value, row.State = "planned", railReports
			row.Detail = agent.Name + " is not integrated yet; saint cannot run it"
		case counts[string(agent.Runtime)] > 0:
			// Running is healthy, and worth seeing without reading. Nobody has
			// to act on it, which is why it is green rather than amber.
			row.Value, row.State = fmt.Sprintf("%d running", counts[string(agent.Runtime)]), railHealthy
			row.Detail = agent.Name + " authenticates to " + agent.Provider
		default:
			row.Value = "idle"
			row.Detail = agent.Name + " authenticates to " + agent.Provider
		}

		// An agent that picks its own provider can reach one this proxy does
		// not forward, and saint is then not in the path: no credential
		// custody and no metering. Say so on the row rather than letting the
		// header's billing chip imply coverage saint does not have.
		if agent.Provider == providerAny {
			row.Detail = agent.Name + " chooses its own provider; traffic to one saint does not proxy is direct, unmetered and uncustodied"
		}
		rows = append(rows, row)
	}
	return rows
}

// railCredentialRows are every credential, grouped by the provider that issues
// it.
//
// One group rather than one section per provider: the row count then stops
// tracking how many providers exist, which is what stopped the earlier shape
// scaling. Each credential still appears exactly once, so one shared between
// two agents is not listed twice.
func (m Model) railCredentialRows() []railRow {
	rows := make([]railRow, 0, len(m.auth.states))
	for _, provider := range m.railProviders() {
		for _, state := range m.auth.states {
			if state.Kind.Provider != provider {
				continue
			}
			row := railRow{Label: state.Kind.ShortLabel(), Group: provider, Action: railActionAuth}
			switch {
			case state.Err != nil:
				// saint tried to read this and could not. That is a fault in the
				// path between saint and the secret, and it will not fix itself.
				row.Value, row.State = statusLine(state.Err.Error()), railBroken
			case !state.Present:
				// Not set is information, not a request. saint knows about four
				// credential kinds and most people use one or two, so colouring
				// every unused one would leave a permanent coloured row in a rail
				// that is working exactly as intended - which is how a colour
				// stops being believed. The words say it, and the section heading
				// counts how many are set.
				row.Value = "not set"
			case !state.ExpiresAt.IsZero():
				// Remaining life, not "set": a session takes whatever is left.
				left := state.ExpiresAt.Sub(m.clock())
				row.Value = formatLife(left) + " left"
				if left <= 0 {
					// formatLife already renders this as "expired"; "expired
					// left" is not a phrase.
					row.Value = "expired"
				}
				// The one credential state that has to be acted on before it
				// bites: a login with minutes on it fails inside a container,
				// hours from now, with nobody watching, and it cannot be
				// refreshed from in there.
				switch {
				case left <= 0:
					row.State = railBroken
				case left <= credentialWarnWithin:
					row.State = railNeedsPerson
				default:
					row.State = railHealthy
				}
			default:
				row.Value = "set"
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// railProviders are the providers that actually have a credential, in first-seen
// order, so the list grows with what exists rather than a hardcoded set.
func (m Model) railProviders() []string {
	seen := map[string]bool{}
	providers := make([]string, 0, 2)
	for _, state := range m.auth.states {
		if p := state.Kind.Provider; p != "" && !seen[p] {
			seen[p] = true
			providers = append(providers, p)
		}
	}
	return providers
}

// railCredentialSummary is the count shown on the collapsed heading, so the
// group says something useful without being opened.
func (m Model) railCredentialSummary() string {
	set := 0
	for _, state := range m.auth.states {
		if state.Present {
			set++
		}
	}
	if len(m.auth.states) == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d set", set, len(m.auth.states))
}

// railProxyRows describe the hop every proxied session's traffic crosses.
func (m Model) railProxyRows() []railRow {
	mode, why := resolvedAuthMode(m.authMode, m.auth.states)
	// Which account pays is information. Both readings are correct states of a
	// working proxy, and the expensive one says "api key" in words; as a colour
	// it sat permanently beside rows that meant something was wrong.
	billing := railRow{Label: "billing", Value: mode, Action: railActionProxy, Detail: why}

	// No proxy is the rail's loudest row. Every agent then falls back to
	// whatever credentials it can find for itself: saint holds nothing, meters
	// nothing, and the billing row above is describing a path the traffic is no
	// longer taking.
	address := railRow{Label: "address", Value: "off", State: railNeedsPerson, Action: railActionProxy}
	if m.proxyStatus != "" {
		address.Value = strings.TrimPrefix(statusLine(m.proxyStatus), "proxy: ")
		if cut, _, found := strings.Cut(address.Value, " · "); found {
			address.Value = cut
		}
		switch {
		case strings.HasPrefix(m.proxyStatus, "proxy off"):
			address.State = railNeedsPerson
		case strings.Contains(m.proxyStatus, "blocked"):
			// Bound but unreachable. Nothing about this looks broken from the
			// host - the listener exists - and no guest request will ever arrive,
			// so it stays invisible until someone reads this row.
			address.State = railNeedsPerson
		default:
			address.State = railHealthy
		}
	}

	return []railRow{
		address,
		billing,
		{Label: "settings", Value: "open", Action: railActionProxy},
	}
}

func (m Model) railWorkspaceRows() []railRow {
	cfg := m.config()

	mounts := "home"
	if roots := cfg.MountRoots; len(roots) > 0 {
		mounts = fmt.Sprintf("%s", roots[0])
		if len(roots) > 1 {
			mounts = fmt.Sprintf("%d roots", len(roots))
		}
	}

	credential := cfg.DefaultCredential
	if credential == "" {
		credential = "live login"
	}
	runtime := string(cfg.DefaultRuntime)
	if runtime == "" {
		runtime = "claude"
	}

	// These live in config.json and have no editor yet, so the row says where
	// the value is rather than pretending it can be changed here.
	return []railRow{
		{Label: "mounts", Value: mounts, Action: railActionNote, Detail: `"mount_roots" in ~/.saint/config.json`},
		{Label: "runtime", Value: runtime, Action: railActionNote, Detail: `"default_runtime" in ~/.saint/config.json`},
		{Label: "credential", Value: credential, Action: railActionNote, Detail: `"default_credential" in ~/.saint/config.json`},
	}
}

func (m Model) railFleetRows() []railRow {
	// Running and stopped are the two ordinary readings of a fleet. Neither is
	// better than the other and neither wants anything, so the labels carry them.
	rows := []railRow{
		{Label: "running", Value: fmt.Sprintf("%d", m.counts.Running)},
		{Label: "stopped", Value: fmt.Sprintf("%d", m.counts.Stopped)},
	}
	// A session saint has a record for and the sandbox has no container for. It
	// will never start again; the record has to be removed or the session
	// recreated, and only a person can decide which.
	if m.counts.Missing > 0 {
		rows = append(rows, railRow{Label: "missing", Value: fmt.Sprintf("%d", m.counts.Missing), State: railNeedsPerson})
	}
	// The mirror image: a container with no session behind it, holding whatever
	// credential it was created with, that nothing in saint will ever stop.
	if len(m.orphans) > 0 {
		rows = append(rows, railRow{Label: "orphans", Value: fmt.Sprintf("%d", len(m.orphans)), State: railNeedsPerson})
	}

	if totals := m.usageTotals(); totals.Requests > 0 {
		rows = append(rows,
			railRow{Label: "tokens in", Value: formatTokens(totals.InputTokens)},
			railRow{Label: "tokens out", Value: formatTokens(totals.OutputTokens)},
		)
	}
	return rows
}

func (m Model) railSystemRows() []railRow {
	paths := m.paths()
	return []railRow{
		{Label: "check", Value: "saint doctor", Action: railActionNote, Detail: "run `saint doctor` in a shell"},
		{Label: "config", Value: "~/.saint", Action: railActionNote, Detail: paths.ConfigPath},
		{Label: "log", Value: "saint.log", Action: railActionNote, Detail: paths.LogPath + " (SAINT_DEBUG=1)"},
	}
}

// railEntry is one selectable line: a section heading, or a row inside one.
//
// Headings are selectable because folding a group is an action, and a fold you
// cannot reach with the same keys as everything else is a second interaction to
// learn.
type railEntry struct {
	Section   string
	Heading   bool
	Collapsed bool
	Summary   string
	Row       railRow
}

// railEntries flattens the sections into what is on screen and selectable.
func (m Model) railEntries() []railEntry {
	query := strings.ToLower(strings.TrimSpace(m.rail.filter.Value()))
	entries := make([]railEntry, 0, 24)

	for _, section := range m.railSections() {
		shown := make([]railRow, 0, len(section.Rows))
		for _, row := range section.Rows {
			if railMatches(query, section.Title, row) {
				shown = append(shown, row)
			}
		}
		if len(shown) == 0 {
			continue
		}

		// A filter reaches into folded groups: hiding a match because of a fold
		// the user set earlier would look like the search was broken.
		collapsed := section.Collapsible && m.railCollapsed(section.Title) && query == ""
		if section.Collapsible {
			summary := ""
			if section.Title == railSectionCredentials {
				summary = m.railCredentialSummary()
			}
			entries = append(entries, railEntry{
				Section: section.Title, Heading: true, Collapsed: collapsed, Summary: summary,
			})
			if collapsed {
				continue
			}
		} else {
			entries = append(entries, railEntry{Section: section.Title, Heading: true})
		}

		for _, row := range shown {
			entries = append(entries, railEntry{Section: section.Title, Row: row})
		}
	}
	return entries
}

// railCollapsed reports whether a section is folded.
func (m Model) railCollapsed(title string) bool {
	for _, name := range m.railFolds {
		if name == title {
			return true
		}
	}
	return false
}

// toggleRailFold folds or unfolds a section and remembers it.
func (m *Model) toggleRailFold(title string) {
	folds := make([]string, 0, len(m.railFolds)+1)
	found := false
	for _, name := range m.railFolds {
		if name == title {
			found = true
			continue
		}
		folds = append(folds, name)
	}
	if !found {
		folds = append(folds, title)
	}
	m.railFolds = folds

	if m.loader == nil || m.loader.Store == nil {
		return
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil {
		return
	}
	cfg.RailCollapsed = folds
	_ = m.loader.Store.SaveConfig(cfg)
}

// railMatches reports whether a row survives the filter.
//
// The section title counts. Now that rows are grouped, the word a user reaches
// for is often the heading - "proxy", "anthropic" - and matching only the row
// label would return nothing for exactly the search they were most likely to
// type.
func railMatches(query, title string, row railRow) bool {
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(title+" "+row.Label+" "+row.Value), query)
}

// railVisibleRows is the rows on screen, without the headings.
func (m Model) railVisibleRows() []railRow {
	rows := make([]railRow, 0, 16)
	for _, entry := range m.railEntries() {
		if !entry.Heading {
			rows = append(rows, entry.Row)
		}
	}
	return rows
}

func (m Model) railCurrent() (railEntry, bool) {
	entries := m.railEntries()
	if m.rail.selected < 0 || m.rail.selected >= len(entries) {
		return railEntry{}, false
	}
	return entries[m.rail.selected], true
}

func (m *Model) railMove(delta int) {
	count := len(m.railEntries())
	if count == 0 {
		m.rail.selected = 0
		return
	}
	m.rail.selected = (m.rail.selected + delta + count) % count
}

func (m Model) updateRail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.rail.filtering {
		switch msg.String() {
		case "esc":
			m.rail.filtering = false
			m.rail.filter.Reset()
			m.rail.filter.Blur()
			m.rail.selected = 0
			return m, nil
		case "enter":
			m.rail.filtering = false
			m.rail.filter.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.rail.filter, cmd = m.rail.filter.Update(msg)
		m.rail.selected = 0
		return m, cmd
	}

	switch msg.String() {
	case "esc", "s":
		return m.closeRail()
	case forceQuitKey, "ctrl+c", "q":
		m.closePane()
		return m, tea.Quit
	case "up", "k":
		m.railMove(-1)
		return m, nil
	case "down", "j":
		m.railMove(1)
		return m, nil
	case "/":
		m.rail.filtering = true
		m.rail.filter.Focus()
		return m, textinput.Blink
	case "R":
		return m, tea.Batch(m.loadCmd(), m.loadCredentialsCmd())
	case "enter", "left", "right":
		entry, ok := m.railCurrent()
		if !ok {
			return m, nil
		}
		if entry.Heading {
			m.toggleRailFold(entry.Section)
			return m, nil
		}
		row := entry.Row
		switch row.Action {
		case railActionAuth:
			return m.openAuth()
		case railActionRefresh:
			m.status = "refreshing..."
			return m, tea.Batch(m.loadCmd(), m.loadCredentialsCmd())
		case railActionProxy:
			return m.openProxy()
		case railActionNote:
			m.status = row.Detail
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

// openRail shows the control rail and refreshes what it reports.
func (m Model) openRail() (tea.Model, tea.Cmd) {
	m.railOpen = true
	m.mode = modeRail
	m.rail.selected = 0
	m.rememberRail(true)
	return m, m.loadCredentialsCmd()
}

func (m Model) closeRail() (tea.Model, tea.Cmd) {
	m.railOpen = false
	m.mode = modeList
	m.rail.filtering = false
	m.rail.filter.Reset()
	m.rail.filter.Blur()
	m.rememberRail(false)
	return m, nil
}

// rememberRail persists the toggle so the rail is where you left it.
func (m Model) rememberRail(open bool) {
	if m.loader == nil || m.loader.Store == nil {
		return
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil || cfg.RailOpen == open {
		return
	}
	cfg.RailOpen = open
	_ = m.loader.Store.SaveConfig(cfg)
}

func (m Model) config() store.Config {
	if m.loader == nil || m.loader.Store == nil {
		return store.Config{}
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil {
		return store.Config{}
	}
	return cfg
}

func (m Model) paths() store.Paths {
	if m.loader == nil || m.loader.Store == nil {
		return store.Paths{}
	}
	return m.loader.Store.Paths()
}

func (m Model) renderRail(height int) string {
	// Overlaid, the rail is not squeezed into a column and can use the width it
	// has - the values are the same, they simply have room to breathe.
	width := railWidth
	if m.railOverlaid() {
		width = min(max(railWidth, m.width-6), 52)
	}
	inner := width - 4

	// A brighter grey than the sidebar's border, and that difference is the
	// whole message: this surface has the keyboard. It used to be indigo, which
	// said the same thing in a hue that then had to be kept out of every other
	// meaning.
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colFocusRule)).
		Padding(0, 1).
		Width(width - 2)
	if height > 2 && !m.railOverlaid() {
		box = box.Height(height - 2)
	}

	// Section headings are bold and ambient; a provider sub-heading is a step
	// brighter but not bold, so the two ranks stay apart without either of them
	// competing with the values they label.
	headingStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colMuted))
	groupStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkSoft))
	selectedStyle := selectionStyle()

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright)).Render("CONTROL"))
	b.WriteString("\n")
	if m.rail.filtering || strings.TrimSpace(m.rail.filter.Value()) != "" {
		b.WriteString(m.rail.filter.View())
		b.WriteString("\n")
	}

	entries := m.railEntries()
	group := ""
	for idx, entry := range entries {
		if entry.Heading {
			group = ""
			b.WriteString("\n")

			// The marker says which way pressing enter goes, so a folded group
			// does not read as an empty one.
			label := strings.ToUpper(entry.Section)
			if m.railSectionIsCollapsible(entry.Section) {
				marker := "▾ "
				if entry.Collapsed {
					marker = "▸ "
				}
				label = marker + label
			}
			line := label
			if entry.Summary != "" {
				gap := max(1, inner-lipgloss.Width(label)-lipgloss.Width(entry.Summary))
				line = label + strings.Repeat(" ", gap) + entry.Summary
			}
			if idx == m.rail.selected {
				b.WriteString(selectedStyle.Render(truncateString(line, inner)))
			} else {
				b.WriteString(headingStyle.Render(truncateString(line, inner)))
			}
			b.WriteString("\n")
			continue
		}

		// A provider sub-heading inside a group, emitted when it changes. Not
		// selectable: it labels rows, it is not something to act on.
		if entry.Row.Group != "" && entry.Row.Group != group {
			group = entry.Row.Group
			b.WriteString(groupStyle.Render(truncateString("  "+group, inner)))
			b.WriteString("\n")
		}

		indent := ""
		if entry.Row.Group != "" {
			indent = "  "
		}
		line := railLine(entry.Row, inner-len(indent))
		if idx == m.rail.selected {
			line = selectedStyle.Render(truncateString(railPlain(entry.Row, inner-len(indent)), inner-len(indent)))
		}
		b.WriteString(indent + line)
		b.WriteString("\n")
	}

	if len(entries) == 0 {
		b.WriteString("\n")
		b.WriteString(sidebarMuted("nothing matches"))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(sidebarMuted("↑↓ move · enter go/fold"))
	b.WriteString("\n")
	b.WriteString(sidebarMuted("/ filter · s close"))

	return box.Render(b.String())
}

// railSectionIsCollapsible reports whether a section can be folded.
func (m Model) railSectionIsCollapsible(title string) bool {
	for _, section := range m.railSections() {
		if section.Title == title {
			return section.Collapsible
		}
	}
	return false
}

// railLine renders one row: label left, value right, coloured by state.
func railLine(row railRow, inner int) string {
	value := truncateString(row.Value, max(1, inner-len(row.Label)-1))
	gap := max(1, inner-lipgloss.Width(row.Label)-lipgloss.Width(value))
	return sidebarMuted(row.Label) + strings.Repeat(" ", gap) + railValueStyle(row.State).Render(value)
}

// railPlain is the same row without colour, for the selected highlight.
func railPlain(row railRow, inner int) string {
	value := truncateString(row.Value, max(1, inner-len(row.Label)-1))
	gap := max(1, inner-lipgloss.Width(row.Label)-lipgloss.Width(value))
	return row.Label + strings.Repeat(" ", gap) + value
}

func railValueStyle(state railState) lipgloss.Style {
	switch state {
	case railNeedsPerson:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))
	case railHealthy:
		// Live credentials, a reachable proxy, a running agent. Green so the
		// rail can be read as a column of shapes rather than a column of words.
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colGood))
	case railBroken:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colBad))
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colInk))
	}
}
