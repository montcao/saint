package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

type loadedMsg struct {
	snapshot Snapshot
	err      error
}

type createdMsg struct {
	session store.Session
	err     error
}

type sessionActionMsg struct {
	id     string
	label  string
	err    error
	attach bool
}

type uiMode int

const (
	modeList uiMode = iota
	modeFilter
	modeNewSession
	modeConfirmDelete
	// modeCommand is the vim/k9s style ":" command line.
	modeCommand
	// modeAuth is the credentials and billing screen.
	modeAuth
	// modeRail is the control rail: the machine's state, and the way to what
	// changes it.
	modeRail
	// modeProxy is the proxy settings screen, opened from the rail.
	modeProxy
	// modeAttached means an agent session is running in an embedded pane and
	// keystrokes belong to it, not to saint.
	modeAttached
)

// detachKey leaves an embedded session without quitting it.
//
// It is deliberately not esc: the agent running inside the pane uses esc
// constantly, so binding it here would make the embedded session unusable.
// ctrl+\ is effectively unused by terminal UIs, which makes it safe to reserve.
const detachKey = "ctrl+\\"

// forceQuitKey leaves saint from anywhere, including an attached session whose
// agent has stopped responding.
//
// While attached, nearly every keystroke is forwarded to the agent - that is
// the point of embedding - which means a hung agent would otherwise swallow
// q, :, esc and ctrl+c alike and leave no way out but killing the process
// from another terminal. The TUI must never be able to become unquittable.
// The only other keys saint reserves for itself are PgUp/PgDn (and the mouse
// wheel) for scrollback - see updateAttached - which never reach the agent.
const forceQuitKey = "ctrl+q"

type Model struct {
	loader    *SnapshotLoader
	service   *session.Service
	rows      []SessionRow
	orphans   []sandbox.Info
	counts    SnapshotCounts
	warnings  []string
	status    string
	selected  int
	scroll    int
	width     int
	height    int
	mode      uiMode
	busy      bool
	showAll   bool
	expanded  bool
	lastError error
	help      help.Model
	keys      keyMap

	// seen is the fleet as it was at the last draw, and marks is what has moved
	// since and has not been looked at yet. Together they are the only thing in
	// saint that answers its own stated purpose: spot the one that changed.
	seen  map[string]sessionState
	marks map[string]changeMark

	// lastClick and lastClickAt are what make a double click a gesture rather
	// than two unrelated presses. Bubble Tea reports presses, not click counts,
	// so saint has to remember the previous one itself.
	lastClick   clickTarget
	lastClickAt time.Time

	// breath is the animation frame, and breathing says whether a clock is
	// currently scheduled. The clock exists only while a session is working, so
	// an idle saint wakes up for nothing.
	breath    int
	breathing bool

	// sawSnapshot records that the fleet has been listed successfully at least
	// once. It is how the first-run screen can tell "the container runtime is
	// not there" from "saint has not looked yet", which are different answers
	// and must not be rendered as the same one.
	sawSnapshot bool

	// spin animates while an action is in flight.
	//
	// A status line that says "deleting session..." and never moves is
	// indistinguishable from a hung program - which saint has been. Motion is
	// the cheapest possible proof that the process is still alive, and it is
	// what people actually read to decide whether to wait or to kill it.
	spin spinner.Model

	filterInput  textinput.Model
	commandInput textinput.Model
	newForm      newSessionForm
	deleteID     string

	// deleteContainer is set when the row being deleted is an orphan: there is
	// no session record to remove, only a container.
	deleteContainer string

	// pane holds the embedded agent session, if one is attached.
	pane *sessionPane

	// credentials is optional. Without it the auth screen is read-only, which
	// is what `saint ls` and the tests use.
	credentials Credentials
	auth        authScreen
	authMode    string

	// rail is the control rail, and railOpen is remembered across runs so it is
	// where you left it.
	rail     railModel
	railOpen bool
	// railFolds are the rail sections the user has folded away, remembered so
	// the rail is the shape they left it.
	railFolds []string
	proxy     proxyScreen

	// now is swappable so rendered values that depend on the clock - a
	// credential's remaining life - are testable.
	now func() time.Time

	// proxyStatus describes the local API proxy. It is always on screen because
	// the proxy stops when saint does: a session that was working can quietly
	// lose API access, and the header is where that becomes visible.
	proxyStatus string
}

// WithCredentials makes the auth screen able to store secrets, not just show
// them.
func (m Model) WithCredentials(c Credentials) Model {
	m.credentials = c
	return m
}

// WithProxyStatus records the proxy state for the header chip.
func (m Model) WithProxyStatus(status string) Model {
	m.proxyStatus = status
	return m
}

func NewModel(loader *SnapshotLoader, service *session.Service) Model {
	filterInput := textinput.New()
	filterInput.Prompt = "/ "
	filterInput.Placeholder = "filter sessions"
	filterInput.CharLimit = 120
	filterInput.Width = 32

	authMode := store.DefaultProxyAuth
	railOpen := false
	var railFolds []string
	if loader != nil && loader.Store != nil {
		if cfg, err := loader.Store.EnsureConfig(); err == nil {
			authMode = cfg.ProxyAuthOrDefault()
			railOpen = cfg.RailOpen
			railFolds = cfg.RailCollapsed
		}
	}

	commandInput := textinput.New()
	commandInput.Prompt = ":"
	commandInput.Placeholder = "q"
	commandInput.CharLimit = 60
	commandInput.Width = 32

	keys := keyMap{
		Up:      key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
		Down:    key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
		New:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Attach:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach/start")),
		Expand:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "details")),
		Stop:    key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop")),
		Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Auth:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "auth")),
		Refresh: key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
		Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Command: key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "command")),
		Detach:  key.NewBinding(key.WithKeys(detachKey), key.WithHelp("ctrl+\\", "detach")),
		Force:   key.NewBinding(key.WithKeys(forceQuitKey), key.WithHelp("ctrl+q", "force quit")),
		Quit:    key.NewBinding(key.WithKeys("q", ":q"), key.WithHelp("q/:q", "quit")),
	}

	return Model{
		loader:       loader,
		service:      service,
		status:       "discovering sessions...",
		expanded:     true,
		help:         help.New(),
		keys:         keys,
		filterInput:  filterInput,
		commandInput: commandInput,
		auth:         newAuthScreen(),
		authMode:     authMode,
		rail:         newRailModel(),
		railOpen:     railOpen,
		railFolds:    railFolds,
		spin:         newSpinner(),
	}
}

// newSpinner is the one place the busy animation is configured.
func newSpinner() spinner.Model {
	s := spinner.New()
	// Dots reads as activity at a glance without pulling the eye away from the
	// list, which is where the user is actually looking.
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))
	return s
}

// startBusy marks an action in flight and starts the animation.
//
// Every asynchronous action goes through here so none of them can silently
// look frozen.
func (m *Model) startBusy(status string) tea.Cmd {
	m.busy = true
	m.status = status
	return m.spin.Tick
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.clampSelection()
		if m.pane != nil {
			cols, rows := m.paneSize()
			m.pane.Resize(cols, rows)
		}
		return m, nil
	case credentialsMsg:
		m.auth.states = msg.states
		m.auth.loadErr = msg.err
		if m.auth.selected >= len(m.auth.states) {
			m.auth.selected = 0
		}
		if m.mode == modeNewSession {
			m.newForm.applyCredentialStates(msg.states, time.Now())
		}
		return m, nil
	case credentialActionMsg:
		m.busy = false
		if msg.err != nil {
			m.lastError = msg.err
			m.status = statusLine(msg.err.Error())
			return m, nil
		}
		m.lastError = nil
		if msg.label != "" {
			m.status = msg.label
		}
		return m, m.loadCredentialsCmd()
	case spinner.TickMsg:
		// Only animate while something is actually happening. A permanent
		// 10fps wakeup is wasted work on a tool that spends most of its life
		// idle.
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case paneDirtyMsg:
		// Ignore output from a pane the user already detached from.
		if m.pane == nil || m.pane.id != msg.id {
			return m, nil
		}
		return m, waitForPaneEvent(m.pane)
	case paneExitedMsg:
		if m.pane == nil || m.pane.id != msg.id {
			return m, nil
		}
		m.closePane()
		// Leave attached mode too. Without this the pane is gone but keystrokes
		// still route to updateAttached, so the next key is silently swallowed.
		m.mode = modeList
		if msg.err != nil {
			m.status = "session ended: " + statusLine(msg.err.Error())
		} else {
			m.status = "session ended"
		}
		return m, m.loadCmd()
	case loadedMsg:
		selectedID := m.selectedID()
		if msg.err != nil {
			m.lastError = msg.err
			m.status = msg.err.Error()
			return m, nil
		}
		m.sawSnapshot = true
		m.seen, m.marks = noteChanges(m.seen, m.marks, msg.snapshot.Rows, m.clock())
		m.rows = msg.snapshot.Rows
		m.orphans = msg.snapshot.Orphans
		m.warnings = msg.snapshot.Warnings
		m.counts = msg.snapshot.Counts
		m.restoreSelection(selectedID)
		if len(m.warnings) > 0 {
			m.status = m.warnings[0]
		} else if !m.busy {
			m.status = "synced " + time.Now().Format("15:04:05")
		}

		// Start the animation when a session begins working and there is no
		// clock yet. Stopping is the clock's own job: it simply declines to
		// schedule another frame.
		if !m.breathing && anyWorking(m.rows) {
			m.breathing = true
			return m, breathCmd(m.rows, m.clock())
		}
		return m, nil
	case createdMsg:
		m.busy = false
		m.mode = modeList
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.status = "session created; attaching..."
		return m.attach(msg.session.ID)
	case sessionActionMsg:
		m.busy = false
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.status = msg.label
		if msg.attach {
			return m.attach(msg.id)
		}
		return m, m.loadCmd()
	case breathMsg:
		m.breath++
		next := breathCmd(m.rows, m.clock())
		m.breathing = next != nil
		return m, next
	case time.Time:
		// The list keeps refreshing while a session is attached, so the rows
		// above the pane stay live the way k9s keeps its resource list moving.
		if m.mode == modeList || m.mode == modeFilter || m.mode == modeAttached {
			return m, tea.Batch(m.loadCmd(), tickCmd())
		}
		return m, tickCmd()
	case tea.KeyMsg:
		// A keypress is the one moment saint can honestly infer that somebody is
		// in front of the terminal, so it is when a change stops being news.
		m.seeChanges()
		switch m.mode {
		case modeAttached:
			return m.updateAttached(msg)
		case modeCommand:
			return m.updateCommand(msg)
		case modeAuth:
			return m.updateAuth(msg)
		case modeRail:
			return m.updateRail(msg)
		case modeProxy:
			return m.updateProxy(msg)
		case modeNewSession:
			return m.updateNewSession(msg)
		case modeConfirmDelete:
			return m.updateDeleteConfirm(msg)
		case modeFilter:
			return m.updateFilter(msg)
		default:
			return m.updateList(msg)
		}
	case tea.MouseMsg:
		return m.updateMouse(msg)
	}
	return m, nil
}

func (m Model) View() string {
	if m.width == 0 {
		return "loading..."
	}

	// While a session is attached the pane replaces the detail panel: the list
	// stays above it so the other sessions remain visible, exactly as in k9s.
	if m.mode == modeAttached && m.pane != nil {
		// The side panes stay up while attached: watching context fill and the
		// current selection is most useful precisely when an agent is working.
		return m.withPanels(lipgloss.JoinVertical(
			lipgloss.Left,
			m.renderHeader(),
			m.renderTable(),
			m.renderPane(),
			m.renderFooter(),
		))
	}

	// Nothing to list yet. This is the only frame most people will remember, so
	// it gets the screen rather than a one-line hint under an empty table.
	if m.mode == modeList && len(m.rows) == 0 && strings.TrimSpace(m.filterInput.Value()) == "" {
		header := m.renderHeader()
		footer := m.renderFooter()
		// The box the first-run screen may fill: everything the header and the
		// status bar are not using. Handing it m.height would push the status
		// bar off the bottom of the terminal.
		free := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
		return m.withPanels(lipgloss.JoinVertical(
			lipgloss.Left,
			header,
			renderFirstRun(m.contentWidth(), free, m.firstRun()),
			footer,
		))
	}

	top := []string{m.renderHeader()}
	if m.mode == modeFilter || strings.TrimSpace(m.filterInput.Value()) != "" {
		// Only while it is doing something. A permanent row showing an empty
		// filter is a row of the list spent saying "no filter".
		top = append(top, m.renderFilters())
	}
	top = append(top, m.renderTable())

	base := m.withPanels(m.fillHeight(
		lipgloss.JoinVertical(lipgloss.Left, top...),
		lipgloss.JoinVertical(lipgloss.Left, m.renderDetailPanel(), m.renderFooter()),
	))

	switch m.mode {
	case modeProxy:
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.renderProxyScreen())
	case modeRail:
		if m.railOverlaid() {
			// Too narrow to sit beside the list, so it sits over it. Better
			// than being unreachable in the split panes saint is most used in.
			return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.renderRail(m.height))
		}
		// Wide enough to be a column: the list stays visible beside it.
		return base
	case modeAuth:
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.renderAuthScreen())
	case modeNewSession:
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.newForm.View(m.width))
	case modeConfirmDelete:
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.renderDeleteDialog())
	default:
		return base
	}
}

func (m Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", forceQuitKey:
		// Tear the pane down before quitting so the container exec client is
		// killed rather than orphaned.
		m.closePane()
		return m, tea.Quit
	case "?":
		m.showAll = !m.showAll
		return m, nil
	case "a":
		return m.openAuth()
	case "v":
		return m.toggleListStyle()
	case "s":
		return m.openRail()
	case "/":
		m.mode = modeFilter
		m.filterInput.Focus()
		return m, textinput.Blink
	case ":":
		m.mode = modeCommand
		m.commandInput.SetValue("")
		m.commandInput.Focus()
		return m, textinput.Blink
	case "n":
		m.newForm = newSessionDialog(m.defaultRepo(), m.defaultRuntime())
		m.mode = modeNewSession
		return m, textinput.Blink
	case "r", "R":
		m.status = "refreshing live state..."
		return m, m.loadCmd()
	case "tab":
		m.expanded = !m.expanded
		return m, nil
	}

	if m.busy {
		return m, nil
	}

	switch msg.String() {
	case "up":
		m.moveSelection(-1)
		return m, nil
	case "down":
		m.moveSelection(1)
		return m, nil
	case "enter":
		row, ok := m.currentRow()
		if !ok {
			return m.openNewSession()
		}
		if row.Orphan {
			// An orphan has no session record, so there is nothing to attach
			// to or start - only something to remove.
			m.status = "orphan container: no session record; press d to remove it"
			return m, nil
		}
		switch row.State {
		case "running":
			return m.attach(row.ID)
		case "stopped":
			spin := m.startBusy("starting session...")
			return m, tea.Batch(spin, m.startSessionCmd(row.ID))
		default:
			m.status = "session is missing its container; delete and recreate it"
			return m, nil
		}
	case "c":
		row, ok := m.currentRow()
		if !ok {
			return m, nil
		}
		if row.Orphan {
			m.status = "orphan container: no session record to renew"
			return m, nil
		}
		// Replacing the container is the only way to hand a running guest a new
		// environment, and it costs the agent whatever it held in memory. That
		// is stated here rather than discovered afterwards.
		spin := m.startBusy("renewing credential (replaces the microVM)...")
		return m, tea.Batch(spin, m.renewCredentialCmd(row.ID))
	case "x":
		row, ok := m.currentRow()
		if !ok {
			return m, nil
		}
		if row.State != "running" {
			m.status = "only running sessions can be stopped"
			return m, nil
		}
		spin := m.startBusy("stopping session...")
		return m, tea.Batch(spin, m.stopSessionCmd(row.ID))
	case "d":
		row, ok := m.currentRow()
		if !ok {
			return m, nil
		}
		m.deleteID = row.ID
		m.deleteContainer = ""
		if row.Orphan {
			m.deleteContainer = row.Container
		}
		m.mode = modeConfirmDelete
		return m, nil
	}

	return m, nil
}

// openNewSession opens the spawn form and refreshes credential status, so the
// credential row can show how much life each option has left before the user
// commits to one.
func (m Model) openNewSession() (tea.Model, tea.Cmd) {
	m.newForm = newSessionDialogWith(m.defaultRepo(), m.defaultRuntime(), m.defaultCredential())
	m.newForm.applyCredentialStates(m.auth.states, time.Now())
	m.mode = modeNewSession
	return m, tea.Batch(textinput.Blink, m.loadCredentialsCmd())
}

// defaultCredential is the choice this saint last used.
func (m Model) defaultCredential() string {
	if m.loader == nil || m.loader.Store == nil {
		return ""
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil {
		return ""
	}
	return cfg.DefaultCredential
}

// clock is the time the view renders against.
func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// openAuth shows the credentials screen and refreshes it.
func (m Model) openAuth() (tea.Model, tea.Cmd) {
	m.mode = modeAuth
	m.auth.selected = 0
	if m.loader != nil && m.loader.Store != nil {
		if cfg, err := m.loader.Store.EnsureConfig(); err == nil {
			m.authMode = cfg.ProxyAuthOrDefault()
		}
	}
	return m, m.loadCredentialsCmd()
}

func (m Model) updateFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "enter":
		m.mode = modeList
		m.filterInput.Blur()
		m.clampSelection()
		return m, nil
	case "ctrl+c":
		m.closePane()
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.clampSelection()
	return m, cmd
}

func (m Model) updateNewSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		return m, nil
	case "enter":
		request := m.newForm.Request()
		m.rememberCredential(request.Credential)
		spin := m.startBusy("creating session...")
		return m, tea.Batch(spin, m.createSessionCmd(request))
	case "ctrl+c":
		m.closePane()
		return m, tea.Quit
	}

	cmd := m.newForm.Update(msg)
	return m, cmd
}

func (m Model) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "n":
		m.mode = modeList
		m.deleteID = ""
		m.deleteContainer = ""
		return m, nil
	case "y", "enter":
		if strings.TrimSpace(m.deleteID) == "" && strings.TrimSpace(m.deleteContainer) == "" {
			m.mode = modeList
			return m, nil
		}
		id := m.deleteID
		container := m.deleteContainer
		m.deleteID = ""
		m.deleteContainer = ""
		m.mode = modeList
		if container != "" {
			spin := m.startBusy("removing container...")
			return m, tea.Batch(spin, m.removeContainerCmd(container))
		}
		spin := m.startBusy("deleting session...")
		return m, tea.Batch(spin, m.deleteSessionCmd(id))
	case "ctrl+c":
		m.closePane()
		return m, tea.Quit
	default:
		return m, nil
	}
}

// fillHeight pins bottom to the last row of the terminal.
//
// Without it the detail panel and status bar float wherever the list happens to
// end, which reads as the frame having come apart - and leaves the sidebar
// extending past them into empty space.
func (m Model) fillHeight(top, bottom string) string {
	block := lipgloss.JoinVertical(lipgloss.Left, top, bottom)
	if m.height <= 0 {
		return block
	}
	if gap := m.height - lipgloss.Height(block); gap > 0 {
		return lipgloss.JoinVertical(lipgloss.Left, top, strings.Repeat("\n", gap-1), bottom)
	}
	return block
}

func (m Model) renderHeader() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	subtitleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	chipStyle := lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color(colOnDark)).Background(lipgloss.Color(colRule))
	chipMuted := lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color(colInk)).Background(lipgloss.Color(colRule))
	chipWarn := lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color(colOnDark)).Background(lipgloss.Color(colAttention))
	overflowChip := chipMuted.Render("…")

	// The proxy chip comes before the counts, because fitHeaderSegments keeps a
	// prefix and drops from the right: order is priority.
	//
	// It used to be last, so on any terminal under about 130 columns it was the
	// first thing replaced by an ellipsis - three counts that were often all
	// zero survived, and "the credential boundary is not working" did not. That
	// is the one fact this header exists to carry.
	var leading []string
	if full, short, needsPerson := m.proxyChip(); full != "" {
		style := chipMuted
		if needsPerson {
			style = chipWarn
		}
		// Shorten before dropping. A proxy chip cut to "proxy blocked" still
		// answers the question; an ellipsis does not.
		//
		// The fit is measured, not estimated. An estimate that was a couple of
		// cells optimistic kept the full text at a width where the row could
		// not hold it, and the chip was then dropped outright - so the widths
		// at which it survived were not even monotonic.
		// Room for the overflow marker is reserved, because there is always at
		// least one chip after this one. Without that reservation a full chip
		// that exactly filled the row was pushed back out the moment the "…"
		// was added, and the chip vanished at precisely the widths where it
		// only just fitted.
		text := full
		row := []string{titleStyle.Render("SAINT"), renderModeBadge(m.headerModeLabel()), style.Render(statusLine(full))}
		budget := m.contentWidth() - lipgloss.Width(overflowChip) - 1
		if lipgloss.Width(strings.Join(row, " ")) > budget {
			text = short
		}
		leading = append(leading, style.Render(statusLine(text)))
	}

	chipParts := []string{
		chipStyle.Render(fmt.Sprintf("running %d", m.counts.Running)),
		chipMuted.Render(fmt.Sprintf("stopped %d", m.counts.Stopped)),
		countChip(chipMuted, chipWarn, "missing", m.counts.Missing),
	}
	if len(m.orphans) > 0 {
		chipParts = append(chipParts, chipMuted.Render(fmt.Sprintf("orphans %d", len(m.orphans))))
	}
	if m.busy {
		chipParts = append(chipParts, chipStyle.Render(m.spin.View()+" working"))
	}
	chipParts = append(leading, chipParts...)

	// Title, mode and counts share a row. They are all "where am I and what is
	// running", they are all short, and fitHeaderSegments drops the least
	// important first when the terminal is narrow.
	titleRow := fitHeaderSegments(
		m.contentWidth(),
		overflowChip,
		append([]string{
			titleStyle.Render("SAINT"),
			renderModeBadge(m.headerModeLabel()),
		}, chipParts...)...,
	)
	_ = subtitleStyle

	return lipgloss.JoinVertical(lipgloss.Left, titleRow, m.renderCommandMenu())
}

func (m Model) renderFilters() string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	value := strings.TrimSpace(m.filterInput.Value())
	if value == "" {
		value = "all sessions"
	}
	var line strings.Builder
	line.WriteString(labelStyle.Render("filter "))
	line.WriteString(m.filterInput.View())
	line.WriteString("  ")
	line.WriteString(labelStyle.Render("view "))
	if m.expanded {
		line.WriteString("expanded details")
	} else {
		line.WriteString("compact details")
	}
	return line.String()
}

// emptyStateHint is what a new user reads before anything else.
func emptyStateHint() string {
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	accent := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright)).Bold(true)

	return strings.Join([]string{
		"No sessions yet.",
		"",
		accent.Render("  a") + muted.Render("  set up authentication - do this first"),
		accent.Render("  n") + muted.Render("  create a session"),
		"",
		muted.Render("  Sessions never prompt for credentials; they use what you store once."),
		"",
	}, "\n")
}

// renderTable draws the session list in whichever style is configured.
//
// The two are a flag rather than a migration because which one reads better is
// a question about a real fleet at a real width, and that is answered by
// looking, not by arguing.
func (m Model) renderTable() string {
	if m.config().ListStyleOrDefault() == store.ListStyleBlocks {
		return m.renderBlocks()
	}
	return m.renderSessionTable()
}

func (m Model) renderSessionTable() string {
	// No border. The table sits between a bordered header row and a bordered
	// detail panel, inside a column that already sits against the sidebar's
	// border - four vertical rules within six columns of each other, none of
	// them carrying information. The list is the content; it gets the space.
	panel := lipgloss.NewStyle().
		Padding(0, 1).
		Width(max(60, m.contentWidth()-2))

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkSoft))
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	selectedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colOnDark)).Background(lipgloss.Color(colSelect))

	// Every line is cut to the panel's text width. The fixed columns alone are
	// wider than a narrow terminal, and a row that wraps turns one session into
	// two rows - which then throws off the selection highlight and the visible
	// range arithmetic.
	inner := m.listInnerWidth()

	var b strings.Builder
	b.WriteString(headerStyle.Render(truncateString(m.formatListHeader(), inner)))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(strings.Repeat("─", min(inner, max(40, panel.GetWidth()-4)))))
	b.WriteString("\n")

	rows := m.filteredRows()
	if len(rows) == 0 {
		// The empty state is the whole first-run experience. It names the two
		// keys that matter and, crucially, the one that has to come first -
		// a session created before a credential is stored starts fine and then
		// fails on the agent's first request, which is a much worse place to
		// discover the problem.
		b.WriteString(emptyStateHint())
		return panel.Render(b.String())
	}

	start, end := m.visibleRange(len(rows))
	for idx := start; idx < end; idx++ {
		line := truncateString(m.formatRow(rows[idx]), inner)
		if idx == m.selected {
			b.WriteString(selectedStyle.Render(line))
			b.WriteString("\n")
			continue
		}

		// The dot carries the state and the rest of the row stays ordinary.
		// Colouring the whole line as well would make a healthy fleet a wall of
		// green, and a wall of anything is not a signal.
		dot, colour := sessionDot(rows[idx], m.breath)
		b.WriteString(lipgloss.NewStyle().Foreground(colour).Render(dot))
		b.WriteString(truncateString(trimLeadingCell(line, 1), max(0, inner-1)))
		b.WriteString("\n")
	}

	if end < len(rows) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("… %d more", len(rows)-end)))
		b.WriteString("\n")
	}

	return panel.Render(b.String())
}

// usageSummary renders what the proxy has metered for a session.
//
// Three states have to stay distinguishable, because collapsing them would put
// a confident wrong number in front of the user: no request has been proxied
// yet, requests were proxied but the provider reported no counters, and real
// measured usage.
func usageSummary(row SessionRow) string {
	if !row.UsageKnown {
		return "-"
	}
	u := row.Usage
	if !u.Reported {
		return fmt.Sprintf("unreported (%s)", plural(u.Requests, "request"))
	}

	parts := []string{
		fmt.Sprintf("ctx %s", formatTokens(u.Context)),
		fmt.Sprintf("in %s", formatTokens(u.InputTokens)),
		fmt.Sprintf("out %s", formatTokens(u.OutputTokens)),
		fmt.Sprintf("cache r%s/w%s", formatTokens(u.CacheReadTokens), formatTokens(u.CacheWriteTokens)),
		plural(u.Requests, "request"),
	}
	if u.Model != "" {
		parts = append([]string{u.Model}, parts...)
	}
	if u.Partial > 0 {
		parts = append(parts, fmt.Sprintf("%d partial", u.Partial))
	}
	return strings.Join(parts, " • ")
}

// formatBytes renders a size on disk.
//
// Separate from formatTokens on purpose. That one counts tokens and divides by
// 1000; bytes divide by 1024, and rendering 4 MiB as "4.19M" - which is what
// borrowing the token formatter produced - is both the wrong unit and a wrong
// number dressed as a right one.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// formatTokens keeps counts readable at a glance. Values under 10k stay exact:
// rounding a small context to "1k" hides the difference between a fresh session
// and a warm one.
func formatTokens(n int64) string {
	switch {
	case n < 0:
		return "0"
	case n < 10_000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (m Model) renderDetailPanel() string {
	panelWidth := max(60, m.contentWidth()-2)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colRule)).
		Padding(0, 1).
		Width(panelWidth)
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	// Healthy is information, not a summons, so it reads as an ordinary value
	// and states itself in words. Only warnStyle below is allowed a colour.
	accentStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInk))
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))

	row, ok := m.currentRow()
	if !ok {
		return panel.Render("No session selected.")
	}

	// Every line is cut to the panel's text width. Names, repository paths and
	// branches come off the user's filesystem at whatever length they happen to
	// be, and a line past this width does not render narrow - it wraps, which
	// gives the panel an extra row and shifts the frame under it.
	inner := panelWidth - 2

	var b strings.Builder
	// Name, runtime, state and age are the sidebar's first four lines. When the
	// sidebar is on screen this panel starts with what the sidebar does not
	// have; when it is not, it has to carry them itself.
	if !m.sidebarVisible() {
		// The state takes its space before the name is measured. A cut name is
		// still recognisable; a state pushed off the line is a fact gone.
		meta := truncateString(fmt.Sprintf("%s • %s • %s", row.Runtime, row.State, row.LastRun), max(0, inner-2))
		b.WriteString(titleStyle.Render(truncateString(row.Name, max(0, inner-2-lipgloss.Width(meta)))))
		b.WriteString("  ")
		b.WriteString(labelStyle.Render(meta))
		b.WriteString("\n")

		// The sidebar breaks the same numbers out line by line. Repeating them
		// here as one dense run is strictly worse than not repeating them.
		const label = "tokens "
		b.WriteString(labelStyle.Render(label))
		b.WriteString(truncateString(usageSummary(row), max(0, inner-len(label))))
		b.WriteString("\n")
	}
	b.WriteString(labelStyle.Render("repos"))
	b.WriteString("\n")

	repos := row.RepoDetails
	if !m.expanded && len(repos) > 1 {
		repos = repos[:1]
	}
	for _, repo := range repos {
		marker := " "
		if repo.Active {
			marker = "▸"
		}
		branch := repo.Branch
		if branch == "" {
			branch = "-"
		}
		status := "clean"
		switch {
		case repo.Missing:
			status = "missing"
		case repo.NoVCS:
			// Not "clean": saint has no history for this directory and cannot
			// say whether anything in it changed.
			status = "no git"
		case repo.DirtyCount > 0:
			status = fmt.Sprintf("%d dirty", repo.DirtyCount)
		}
		// The path, the branch and the review list each get a share of the line
		// rather than the whole of it, so that a deep checkout cannot push the
		// status and the review marker - the two parts that need a person - off
		// the end. The assembled line is cut to the panel regardless.
		line := fmt.Sprintf("%s %s  %s  %s  %s",
			marker, truncateString(repo.Path, max(12, inner/3)), truncateString(branch, max(8, inner/6)),
			status, repo.LastTouched)
		if repo.ReadOnly {
			line += "  (ro)"
		}
		needsReview := len(repo.ReviewPaths) > 0
		if needsReview {
			line += "  review: " + truncateString(strings.Join(repo.ReviewPaths, " "), max(8, inner/4))
		}
		line = truncateString(line, inner)
		switch {
		case needsReview:
			b.WriteString(warnStyle.Render(line))
		case repo.Active:
			b.WriteString(accentStyle.Render(line))
		default:
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	if !m.expanded && len(row.RepoDetails) > 1 {
		b.WriteString(labelStyle.Render(fmt.Sprintf("+%d more repos", len(row.RepoDetails)-1)))
		b.WriteString("\n")
	}
	if len(m.orphans) > 0 {
		names := make([]string, 0, len(m.orphans))
		for _, orphan := range m.orphans {
			names = append(names, orphan.Name)
		}
		b.WriteString("\n")
		const label = "orphan containers "
		b.WriteString(labelStyle.Render(label))
		b.WriteString(truncateString(sanitizeInline(strings.Join(names, ", ")), max(0, inner-len(label))))
		b.WriteString("\n")
	}

	return panel.Render(strings.TrimRight(b.String(), "\n"))
}

// renderPane draws the embedded agent session inside a titled box.
//
// The emulator has already resolved every escape sequence into a grid of the
// exact pane size, so this only has to frame it.
func (m Model) renderPane() string {
	if m.pane == nil {
		return ""
	}

	cols, _ := m.pane.Size()
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))

	title := titleStyle.Render("session: " + m.pane.name)
	// The escapes are not repeated here. The header's command row carries them
	// for as long as the session is attached, and that row is saint's own
	// chrome - it keeps drawing whatever the agent inside the pane is doing,
	// which is exactly the case the escapes exist for.
	hint := hintStyle.Render(m.pane.id)
	if m.pane.Scrolled() {
		// Scrollback has no separate mode to signal in the footer's key list -
		// it's a passive overlay - so the pane header is where the user learns
		// they're looking at history and how to get back to live.
		hint = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colAttention)).
			Render("SCROLLING (any key returns to live)")
	}
	gap := max(1, cols-lipgloss.Width(title)-lipgloss.Width(hint))
	header := title + strings.Repeat(" ", gap) + hint

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colSelect)).
		Padding(0, 1)

	return box.Render(header + "\n" + m.pane.Render())
}

func (m Model) renderFooter() string {
	m.help.ShowAll = m.showAll
	m.help.Width = m.contentWidth()
	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInk)).Background(lipgloss.Color(colSunken)).Padding(0, 1)
	if m.lastError != nil {
		statusStyle = statusStyle.Foreground(lipgloss.Color(colInkBright)).Background(lipgloss.Color(colAttention))
	}
	// The command line takes the status row while it is open, the way vim and
	// k9s put ":" at the bottom of the screen.
	bottom := statusStyle.Render(truncateString(m.status, max(10, m.contentWidth()-2)))
	if m.mode == modeCommand {
		bottom = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colInkBright)).
			Background(lipgloss.Color(colSunken)).
			Padding(0, 1).
			Render(m.commandInput.View())
	}

	// The header carries the keys for the current mode, so the footer does not
	// repeat them. `?` still opens the full reference - it is the one place a
	// complete list is worth the rows it costs.
	if !m.showAll {
		return bottom
	}

	// Clamp the help here rather than relying on the outer net: JoinVertical
	// pads every block out to the widest one, so a single over-wide line would
	// stretch the whole column before anything downstream could cut it back.
	return lipgloss.JoinVertical(
		lipgloss.Left,
		clampWidth(m.help.View(m.keys), m.contentWidth()),
		bottom,
	)
}

func (m Model) renderDeleteDialog() string {
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colAttention)).
		Padding(1, 2).
		Width(min(m.width-6, 64))
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	bodyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkSoft))

	title := "Delete Session"
	detail := "This removes the container and deletes the saved session record."
	if m.deleteContainer != "" {
		title = "Remove Container"
		detail = "This removes the orphan container " + statusLine(m.deleteContainer) +
			". There is no session record to delete."
	}

	var body strings.Builder
	body.WriteString(titleStyle.Render(title))
	body.WriteString("\n")
	body.WriteString(bodyStyle.Render(detail))
	body.WriteString("\n\n")
	body.WriteString(bodyStyle.Render("enter/y delete  esc/n cancel"))
	return panel.Render(body.String())
}

func (m Model) loadCmd() tea.Cmd {
	return func() tea.Msg {
		snapshot, err := m.loader.Load(context.Background())
		return loadedMsg{snapshot: snapshot, err: err}
	}
}

func (m Model) createSessionCmd(req session.CreateRequest) tea.Cmd {
	return func() tea.Msg {
		created, err := m.service.Create(context.Background(), req)
		return createdMsg{session: created, err: err}
	}
}

func (m Model) startSessionCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := m.service.Start(context.Background(), id)
		return sessionActionMsg{id: id, label: "session started; attaching...", err: err, attach: err == nil}
	}
}

func (m Model) stopSessionCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := m.service.Stop(context.Background(), id)
		return sessionActionMsg{id: id, label: "session stopped", err: err}
	}
}

// removeContainerCmd deletes an orphan container.
//
// There is no session record to clean up - that is what makes it an orphan - so
// this goes straight at the runtime rather than through the session service.
func (m Model) removeContainerCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if m.loader == nil || m.loader.Sandbox == nil {
			return sessionActionMsg{id: name, label: "container removed", err: errors.New("sandbox is not configured")}
		}
		err := m.loader.Sandbox.Remove(context.Background(), name)
		return sessionActionMsg{id: name, label: "container removed", err: err}
	}
}

// rememberCredential records the choice so the next session defaults to it.
//
// Per-session choice should not mean answering the same question every time -
// the row is there to be seen, not to be filled in.
func (m Model) rememberCredential(spec string) {
	if m.loader == nil || m.loader.Store == nil || spec == "" {
		return
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil || cfg.DefaultCredential == spec {
		return
	}
	cfg.DefaultCredential = spec
	_ = m.loader.Store.SaveConfig(cfg)
}

func (m Model) deleteSessionCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := m.service.Delete(context.Background(), id)
		if err == nil && m.loader != nil {
			// Drop the metered totals with the session. Session IDs can be
			// reused, and inheriting a stranger's token count is worse than
			// showing none.
			m.loader.Usage.Forget(id)
		}
		return sessionActionMsg{id: id, label: "session deleted", err: err}
	}
}

// attach opens the session in an embedded pane and keeps saint on screen.
//
// This deliberately does not use tea.ExecProcess. Handing the whole terminal to
// `container exec -it ... tmux attach` works, but it replaces saint entirely
// until the agent exits, which loses the session list, the status bar and any
// sense of the other sessions running alongside. Instead the attach command
// runs under a PTY whose output is emulated into a fixed-size grid and drawn as
// one region of saint's own frame - the k9s model, where the surrounding
// chrome never goes away.
func (m Model) attach(id string) (tea.Model, tea.Cmd) {
	cmd, err := m.service.AttachCmd(id)
	if err != nil {
		m.status = "attach failed: " + statusLine(err.Error())
		return m, m.loadCmd()
	}

	name := id
	if row, ok := m.rowByID(id); ok && strings.TrimSpace(row.Name) != "" {
		name = row.Name
	}

	cols, rows := m.paneSize()
	pane, err := startSessionPane(id, name, cmd, cols, rows)
	if err != nil {
		m.status = "attach failed: " + statusLine(err.Error())
		return m, m.loadCmd()
	}

	m.closePane()
	m.pane = pane
	m.mode = modeAttached
	m.busy = false
	m.status = "attached to " + name + "  (ctrl+\\ detaches, session keeps running)"
	return m, waitForPaneEvent(pane)
}

// updateAttached forwards keystrokes to the embedded session.
//
// Everything except detachKey, forceQuitKey and the scroll keys (PgUp/PgDn)
// goes through to the agent, including q, :, esc and ctrl+c - while attached
// those belong to the program inside the pane, not to saint. That is the
// whole point of embedding rather than shelling out.
func (m Model) updateAttached(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case detachKey:
		m.closePane()
		m.mode = modeList
		m.status = "detached; session still running"
		return m, m.loadCmd()
	case forceQuitKey:
		// Always reachable, even when the agent is wedged.
		m.closePane()
		return m, tea.Quit
	}

	if m.pane == nil {
		m.mode = modeList
		return m, nil
	}

	// These scroll saint's own view of the pane's history and are never
	// forwarded to the agent - scrolling to read past output shouldn't also
	// send the agent a keystroke it has to interpret.
	//
	// Shift+arrow exists because the wheel is not reliable here and cannot be
	// made so. A terminal in the alternate screen may translate a wheel notch
	// into a bare arrow key rather than a mouse event; saint then receives an
	// ordinary "up", forwards it as every other keystroke is forwarded, and the
	// agent walks its prompt history. saint cannot tell that apart from someone
	// holding the arrow key, so guessing would break typing to fix scrolling.
	//
	// A *shifted* arrow is unambiguous: no terminal synthesises one for a wheel
	// notch. It is therefore safe to keep, and it gives line-by-line movement,
	// which PgUp/PgDn cannot. The cost is that an agent can no longer receive
	// shift+arrow, which is stated in the header rather than left to be
	// discovered.
	switch msg.String() {
	case "pgup":
		_, rows := m.pane.Size()
		m.pane.ScrollUp(rows)
		return m, nil
	case "pgdown":
		_, rows := m.pane.Size()
		m.pane.ScrollDown(rows)
		return m, nil
	case "shift+up":
		m.pane.ScrollUp(1)
		return m, nil
	case "shift+down":
		m.pane.ScrollDown(1)
		return m, nil
	}

	// Any other key while scrolled means the user is done reading history and
	// is typing to the agent again: snap back to live first, then still
	// deliver the keystroke normally. Scrollback is a passive overlay, not a
	// mode the user has to consciously exit.
	if m.pane.Scrolled() {
		m.pane.ScrollToLive()
	}

	if data := encodeKey(msg); len(data) > 0 {
		if err := m.pane.Write(data); err != nil {
			m.closePane()
			m.mode = modeList
			m.status = "session input failed: " + statusLine(err.Error())
			return m, m.loadCmd()
		}
	}
	return m, nil
}

// updateCommand drives the ":" command line.
func (m Model) updateCommand(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeList
		m.commandInput.Blur()
		m.commandInput.SetValue("")
		return m, nil
	case "enter":
		input := m.commandInput.Value()
		m.commandInput.Blur()
		m.commandInput.SetValue("")
		m.mode = modeList

		switch runCommand(input) {
		case commandQuit:
			m.closePane()
			return m, tea.Quit
		case commandNone:
			return m, nil
		case commandDetach:
			if m.pane == nil {
				m.status = "no attached session"
				return m, nil
			}
			m.closePane()
			m.status = "detached; session still running"
			return m, m.loadCmd()
		case commandHelp:
			m.showAll = !m.showAll
			return m, nil
		case commandRefresh:
			m.status = "refreshing live state..."
			return m, m.loadCmd()
		case commandNew:
			return m.openNewSession()
		case commandAuth:
			return m.openAuth()
		default:
			m.status = "unknown command: " + statusLine(strings.TrimSpace(input))
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.commandInput, cmd = m.commandInput.Update(msg)
	return m, cmd
}

// closePane detaches from the embedded session without stopping it. The agent
// keeps running under tmux inside the container, so re-attaching is instant.
func (m *Model) closePane() {
	if m.pane == nil {
		return
	}
	m.pane.Close()
	m.pane = nil
}

// paneSize is the geometry available to an embedded session: full width inside
// the pane border, and whatever vertical space the list and chrome leave.
func (m Model) paneSize() (int, int) {
	_, paneRows := m.attachedSplit()
	return clampPaneSize(m.contentWidth()-paneChromeCols, paneRows)
}

func (m Model) rowByID(id string) (SessionRow, bool) {
	for _, row := range m.rows {
		if row.ID == id {
			return row, true
		}
	}
	return SessionRow{}, false
}

func (m *Model) moveSelection(delta int) {
	rows := m.filteredRows()
	if len(rows) == 0 {
		m.selected = 0
		return
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(rows) {
		m.selected = len(rows) - 1
	}
	m.clampSelection()
}

func (m *Model) restoreSelection(id string) {
	rows := m.filteredRows()
	if len(rows) == 0 {
		m.selected = 0
		m.scroll = 0
		return
	}
	if id != "" {
		for idx, row := range rows {
			if row.ID == id {
				m.selected = idx
				m.clampSelection()
				return
			}
		}
	}
	m.clampSelection()
}

func (m *Model) clampSelection() {
	rows := m.filteredRows()
	if len(rows) == 0 {
		m.selected = 0
		m.scroll = 0
		return
	}
	if m.selected >= len(rows) {
		m.selected = len(rows) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
	listHeight := m.listHeight()
	if m.selected < m.scroll {
		m.scroll = m.selected
	}
	if m.selected >= m.scroll+listHeight {
		m.scroll = m.selected - listHeight + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m Model) visibleRange(total int) (int, int) {
	height := m.listHeight()
	start := m.scroll
	if start > total {
		start = 0
	}
	end := start + height
	if end > total {
		end = total
	}
	return start, end
}

// Space saint's own chrome occupies when a session is attached, so the list and
// the pane can split what is left.
//
//	rows: header 3, list box border/header 4, pane border+title 3, footer 2
//	cols: pane border 2, pane padding 2
const (
	attachedListRows = 5
	attachedChrome   = 3 + 4 + 3 + 2
	// paneChromeCols is the pane border and padding. The sidebar is not part
	// of it: paneSize subtracts it from contentWidth, which has already given
	// the sidebar its columns.
	paneChromeCols = 4
)

// attachedSplit divides the available height between the session list and the
// pane. The list yields first: a cramped pane renders a garbled agent, which
// reads as a frozen terminal rather than a small one.
func (m Model) attachedSplit() (listRows, paneRows int) {
	available := m.height - attachedChrome

	listRows = attachedListRows
	if available-listRows < minPaneRows {
		listRows = available - minPaneRows
	}
	if listRows > attachedListRows {
		listRows = attachedListRows
	}
	if listRows < 1 {
		listRows = 1
	}

	paneRows = available - listRows
	if paneRows < minPaneRows {
		paneRows = minPaneRows
	}
	return listRows, paneRows
}

func (m Model) listHeight() int {
	// An attached session gets the bulk of the screen; the list shrinks to a
	// glanceable strip rather than disappearing.
	if m.mode == modeAttached {
		listRows, _ := m.attachedSplit()
		return listRows
	}
	height := m.height - 18
	if height < 6 {
		height = 6
	}
	return height
}

func (m Model) filteredRows() []SessionRow {
	query := strings.ToLower(strings.TrimSpace(m.filterInput.Value()))
	if query == "" {
		return m.rows
	}
	filtered := make([]SessionRow, 0, len(m.rows))
	for _, row := range m.rows {
		if rowMatches(row, query) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func rowMatches(row SessionRow, query string) bool {
	fields := []string{row.ID, row.Name, row.Repos, row.Runtime, row.State, row.Activity}
	for _, repo := range row.RepoDetails {
		fields = append(fields, repo.Path, repo.Name, repo.Branch, strings.Join(repo.ReviewPaths, " "))
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

func (m Model) currentRow() (SessionRow, bool) {
	rows := m.filteredRows()
	if len(rows) == 0 || m.selected < 0 || m.selected >= len(rows) {
		return SessionRow{}, false
	}
	return rows[m.selected], true
}

func (m Model) selectedID() string {
	row, ok := m.currentRow()
	if !ok {
		return ""
	}
	return row.ID
}

func (m Model) defaultRepo() string {
	if m.service != nil && strings.TrimSpace(m.service.Workdir) != "" {
		return m.service.Workdir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func (m Model) defaultRuntime() sandbox.Runtime {
	if m.loader == nil || m.loader.Store == nil {
		return sandbox.RuntimeClaude
	}
	cfg, err := m.loader.Store.EnsureConfig()
	if err != nil || cfg.DefaultRuntime == "" {
		return sandbox.RuntimeClaude
	}
	return cfg.DefaultRuntime
}

// listColumn is one column of the session table. Value pulls the cell out of a
// row so the header and the body cannot drift apart - they are generated from
// the same list.
type listColumn struct {
	Title string
	Width int
	Value func(SessionRow) string
}

// listColumns is the session table, left to right. The order is also the drop
// order: when the terminal cannot hold them all, columns leave from the right,
// whole, rather than every column squeezing into illegibility.
func listColumns() []listColumn {
	return []listColumn{
		// The status marker. Its heading is blank because a two-cell column
		// titled "ST" would be a label competing with the thing it labels, and
		// the dot's meaning is carried by the STATE and ACTIVITY columns beside
		// it in words.
		{"", 2, func(r SessionRow) string { return "" }},
		{"NAME", 22, func(r SessionRow) string { return r.Name }},
		{"REPOS", 20, func(r SessionRow) string { return r.Repos }},
		{"RT", 9, func(r SessionRow) string { return r.Runtime }},
		{"STATE", 10, func(r SessionRow) string { return r.State }},
		{"ACTIVITY", 10, func(r SessionRow) string { return r.Activity }},
		{"LAST RUN", 11, func(r SessionRow) string { return r.LastRun }},
	}
}

// listInnerWidth is the text width inside the session table's border and
// padding.
func (m Model) listInnerWidth() int {
	return max(60, m.contentWidth()-2) - 2
}

// visibleListColumns is the longest prefix of the table that fits. The first
// column is always kept: a table with no columns is not a narrower table, it is
// a blank pane.
func (m Model) visibleListColumns() []listColumn {
	columns := listColumns()
	width := m.listInnerWidth()
	used := 0
	for i, column := range columns {
		if i > 0 && used+column.Width > width {
			return columns[:i]
		}
		used += column.Width
	}
	return columns
}

func (m Model) formatListHeader() string {
	return m.formatCells(func(c listColumn) string { return c.Title })
}

func (m Model) formatRow(row SessionRow) string {
	return m.formatCells(func(c listColumn) string { return c.Value(row) })
}

// formatCells lays one line out. The last column drawn is truncated rather than
// padded: padding it would leave a run of trailing spaces that reads as an empty
// column the user keeps looking for a heading for.
func (m Model) formatCells(cell func(listColumn) string) string {
	columns := m.visibleListColumns()
	var b strings.Builder
	for i, column := range columns {
		if i == len(columns)-1 {
			b.WriteString(truncateString(cell(column), column.Width))
			break
		}
		b.WriteString(padCell(cell(column), column.Width))
	}
	return b.String()
}

func padCell(value string, width int) string {
	value = truncateString(value, width-1)
	return value + strings.Repeat(" ", max(1, width-len(value)))
}

func truncateString(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return string(runes[:1])
	}
	return string(runes[:width-1]) + "…"
}

func tickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return t
	})
}

type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	New     key.Binding
	Attach  key.Binding
	Expand  key.Binding
	Stop    key.Binding
	Delete  key.Binding
	Auth    key.Binding
	Refresh key.Binding
	Filter  key.Binding
	Help    key.Binding
	Command key.Binding
	Detach  key.Binding
	Quit    key.Binding
	Force   key.Binding
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.New, k.Attach, k.Stop, k.Delete, k.Auth, k.Filter, k.Command, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Expand, k.Refresh},
		{k.New, k.Attach, k.Detach, k.Stop, k.Delete, k.Auth},
		{k.Force},
		{k.Filter, k.Command, k.Help, k.Quit},
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = exec.ErrNotFound

// proxyChip renders the proxy's state for the header: the full line, a short
// form for a narrow terminal, and whether it needs a person.
//
// Every state that is not a bound, reachable proxy needs one, and that includes
// "blocked" - which is the worst of them precisely because it looks healthy. A
// blocked proxy has bound its port and answers nothing: the host sees a
// listener, no guest request ever arrives, and the agents fall back to whatever
// credentials they can find. It used to be styled as calmly as a working proxy,
// because the check only matched the "proxy off" prefix.
func (m Model) proxyChip() (full, short string, needsPerson bool) {
	status := strings.TrimSpace(m.proxyStatus)
	if status == "" {
		return "", "", false
	}

	head, _, _ := strings.Cut(status, ":")
	switch {
	case strings.HasPrefix(status, "proxy: external"):
		return status, "proxy external", false
	case strings.HasPrefix(status, "proxy: "):
		return status, "proxy ok", false
	default:
		// "proxy off: <reason>" and "proxy blocked: <addr> (firewall?…)" both
		// shorten to their first clause, which is the part that names the
		// problem.
		return status, strings.TrimSpace(head), true
	}
}

// countChip colours a count only when it is not zero.
//
// "missing 0" wore the attention colour permanently, which spends the one
// saturated colour in the interface on the absence of a problem. A count of
// zero is information; a count above zero is a session saint cannot find.
func countChip(calm, attention lipgloss.Style, label string, n int) string {
	style := calm
	if n > 0 {
		style = attention
	}
	return style.Render(fmt.Sprintf("%s %d", label, n))
}

// renewCredentialCmd gives a session a freshly read credential.
//
// A session's credential is a copy taken when its container was created, and a
// process environment cannot be changed afterwards - so the host's Claude Code
// login refreshing does nothing for a session already running on an old copy.
// Replacing the container is the only lever, and the status line says so before
// it is pulled rather than after.
func (m Model) renewCredentialCmd(id string) tea.Cmd {
	return func() tea.Msg {
		if m.service == nil {
			return sessionActionMsg{id: id, label: "credential renewed", err: errors.New("session service is not configured")}
		}
		session, err := m.service.RenewCredential(context.Background(), id)
		if err != nil {
			return sessionActionMsg{id: id, label: "credential renewed", err: err}
		}
		label := "credential renewed"
		if !session.CredentialExpiresAt.IsZero() {
			label += " · " + formatLife(time.Until(session.CredentialExpiresAt)) + " left"
		}
		return sessionActionMsg{id: id, label: label}
	}
}

// trimLeadingCell drops n display cells from the front of a line.
//
// The marker column is laid out as part of the row so every other column stays
// where the header says it is; the dot is then drawn over its first cell in the
// state's colour. Trimming by cells rather than bytes matters because the
// glyphs that go there are multi-byte.
func trimLeadingCell(line string, n int) string {
	runes := []rune(line)
	if n >= len(runes) {
		return ""
	}
	return string(runes[n:])
}
