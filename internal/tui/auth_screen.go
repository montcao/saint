package tui

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
)

// CredentialKind is one secret saint can hold.
//
// The TUI does not import the keychain: a host that wants credentials editable
// from the UI supplies a Credentials implementation, and one that does not
// simply leaves it nil and gets a read-only screen.
type CredentialKind struct {
	Name string

	// Provider is the company the credential authenticates to. It is the
	// heading credentials group under, because "claude" beside "anthropic"
	// beside "openai" reads as three vendors when Claude is Anthropic.
	Provider string

	// Short is the label to use beneath a provider heading, where repeating the
	// provider would be noise.
	Short string

	Config  string
	Summary string
	Setup   string
}

// ShortLabel is the row label to use when the provider is already stated.
func (k CredentialKind) ShortLabel() string {
	if k.Short != "" {
		return k.Short
	}
	return k.Name
}

// CredentialState is what saint knows about one secret. It never carries the
// secret itself - only whether there is one and where it comes from.
type CredentialState struct {
	Kind    CredentialKind
	Source  string
	Present bool

	// ExpiresAt is when the credential stops working, when saint knows. Zero
	// means unknown, which is not a promise that it lasts forever.
	ExpiresAt time.Time

	// Detail is a short non-secret note, such as the plan a login belongs to.
	Detail string

	Err error
	// Editable is false when the value comes from somewhere saint cannot write,
	// such as a 1Password reference or a command.
	Editable bool
}

// Credentials is the host-side credential store the auth screen drives.
type Credentials interface {
	// List reports every credential and its current state.
	List(ctx context.Context) ([]CredentialState, error)
	// Set stores a value. Implementations must never log it.
	Set(ctx context.Context, config, value string) error
	// Clear removes a stored value.
	Clear(ctx context.Context, config string) error
}

// credentialsMsg carries a refreshed view of the credential store.
type credentialsMsg struct {
	states []CredentialState
	err    error
}

// credentialActionMsg reports the result of storing or clearing a secret.
type credentialActionMsg struct {
	label string
	err   error
}

// authScreen is the credentials and billing screen.
//
// It exists because the setup is otherwise tribal knowledge: a new user has no
// way to discover that a key has to be stored before any session will work, and
// no way to see which account their sessions are about to bill.
type authScreen struct {
	states   []CredentialState
	selected int
	loadErr  error

	// editing is the credential currently being typed into, if any.
	editing *CredentialState
	input   textinput.Model
}

func newAuthScreen() authScreen {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "paste, then enter"
	input.CharLimit = 512
	input.Width = 44
	// Secrets are pasted into a screen someone may be sharing.
	input.EchoMode = textinput.EchoPassword
	return authScreen{input: input}
}

func (a authScreen) current() (CredentialState, bool) {
	if a.selected < 0 || a.selected >= len(a.states) {
		return CredentialState{}, false
	}
	return a.states[a.selected], true
}

func (a *authScreen) move(delta int) {
	if len(a.states) == 0 {
		a.selected = 0
		return
	}
	a.selected += delta
	if a.selected < 0 {
		a.selected = len(a.states) - 1
	}
	if a.selected >= len(a.states) {
		a.selected = 0
	}
}

// nextAuthMode cycles the billing mode in a fixed order so repeated presses are
// predictable.
func nextAuthMode(mode string) string {
	switch mode {
	case store.ProxyAuthAuto:
		return store.ProxyAuthSubscription
	case store.ProxyAuthSubscription:
		return store.ProxyAuthKey
	default:
		return store.ProxyAuthAuto
	}
}

// resolvedAuthMode reports which mode a new Claude session would use, and why.
// Auto is only acceptable as a default because this is always on screen.
func resolvedAuthMode(configured string, states []CredentialState) (mode, why string) {
	subscription := false
	for _, state := range states {
		if state.Present && store.IsSubscriptionCredential(state.Kind.Config) {
			subscription = true
		}
	}
	return store.ResolveProxyAuth(configured, subscription)
}

func (m Model) loadCredentialsCmd() tea.Cmd {
	return func() tea.Msg {
		if m.credentials == nil {
			return credentialsMsg{err: errors.New("credential store is not configured")}
		}
		states, err := m.credentials.List(context.Background())
		return credentialsMsg{states: states, err: err}
	}
}

func (m Model) setCredentialCmd(config, value string) tea.Cmd {
	return func() tea.Msg {
		if m.credentials == nil {
			return credentialActionMsg{err: errors.New("credential store is not configured")}
		}
		err := m.credentials.Set(context.Background(), config, value)
		return credentialActionMsg{label: "stored", err: err}
	}
}

func (m Model) clearCredentialCmd(config string) tea.Cmd {
	return func() tea.Msg {
		if m.credentials == nil {
			return credentialActionMsg{err: errors.New("credential store is not configured")}
		}
		err := m.credentials.Clear(context.Background(), config)
		return credentialActionMsg{label: "cleared", err: err}
	}
}

// setAuthModeCmd persists the billing mode.
func (m Model) setAuthModeCmd(mode string) tea.Cmd {
	return func() tea.Msg {
		if m.loader == nil || m.loader.Store == nil {
			return credentialActionMsg{err: errors.New("store is not configured")}
		}
		cfg, err := m.loader.Store.EnsureConfig()
		if err != nil {
			return credentialActionMsg{err: err}
		}
		cfg.ProxyAuth = mode
		if err := m.loader.Store.SaveConfig(cfg); err != nil {
			return credentialActionMsg{err: err}
		}
		return credentialActionMsg{label: "billing mode is now " + mode}
	}
}

func (m Model) updateAuth(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While a secret is being typed, only the editor gets keys. Anything else
	// would act on a keystroke meant for the value.
	if m.auth.editing != nil {
		switch msg.String() {
		case "esc":
			m.auth.editing = nil
			m.auth.input.Reset()
			m.auth.input.Blur()
			return m, nil
		case "enter":
			value := strings.TrimSpace(m.auth.input.Value())
			config := m.auth.editing.Kind.Config
			m.auth.editing = nil
			m.auth.input.Reset()
			m.auth.input.Blur()
			if value == "" {
				m.status = "nothing entered"
				return m, nil
			}
			m.status = "storing..."
			return m, m.setCredentialCmd(config, value)
		}
		var cmd tea.Cmd
		m.auth.input, cmd = m.auth.input.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "esc", "a", "q":
		m.mode = modeList
		return m, nil
	case forceQuitKey, "ctrl+c":
		m.closePane()
		return m, tea.Quit
	case "up", "k":
		m.auth.move(-1)
		return m, nil
	case "down", "j":
		m.auth.move(1)
		return m, nil
	case "enter":
		state, ok := m.auth.current()
		if !ok {
			return m, nil
		}
		if !state.Editable {
			m.status = state.Kind.Name + " comes from " + state.Source + "; change it there"
			return m, nil
		}
		editing := state
		m.auth.editing = &editing
		m.auth.input.Reset()
		m.auth.input.Focus()
		return m, textinput.Blink
	case "x":
		state, ok := m.auth.current()
		if !ok || !state.Editable {
			return m, nil
		}
		m.status = "clearing..."
		return m, m.clearCredentialCmd(state.Kind.Config)
	case "m":
		next := nextAuthMode(m.authMode)
		m.authMode = next
		return m, m.setAuthModeCmd(next)
	case "R":
		return m, m.loadCredentialsCmd()
	}
	return m, nil
}

// pasteHint names the key rather than saying "paste".
//
// "Paste, then enter" assumes the reader knows which key that is in this
// terminal, which is the assumption that left the first user of this screen
// stuck in front of it.
func pasteHint() string {
	if runtime.GOOS == "darwin" {
		return "⌘V or ctrl+v to paste · enter store · esc cancel"
	}
	return "ctrl+shift+v or ctrl+v to paste · enter store · esc cancel"
}

func (m Model) renderAuthScreen() string {
	width := min(max(60, m.width-8), 92)
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colFocusRule)).
		Padding(1, 2).
		Width(width)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	headingStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colMuted))
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	// Every state this screen reports about a credential - set, not set, which
	// account sessions bill - is a fact the user chose, not a thing asking to be
	// dealt with, so they are all one ordinary value colour and the words tell
	// them apart. Only a credential saint tried to read and could not gets the
	// attention colour, because that is the one state nobody chose and nothing
	// will clear on its own.
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInk))
	attentionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))
	selectedStyle := selectionStyle()

	var b strings.Builder
	b.WriteString(titleStyle.Render("Authentication"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("Set these once. Every session uses them; none of them ask again."))
	b.WriteString("\n\n")

	b.WriteString(headingStyle.Render("CREDENTIALS"))
	b.WriteString("\n")

	if m.auth.loadErr != nil {
		b.WriteString(attentionStyle.Render(statusLine(m.auth.loadErr.Error())))
		b.WriteString("\n")
	}
	if len(m.auth.states) == 0 && m.auth.loadErr == nil {
		b.WriteString(mutedStyle.Render("loading..."))
		b.WriteString("\n")
	}

	inner := width - 6
	for idx, state := range m.auth.states {
		status := valueStyle.Render("not set")
		switch {
		case state.Present:
			status = valueStyle.Render("set")
		case state.Err != nil:
			status = attentionStyle.Render(statusLine(state.Err.Error()))
		}
		label := fmt.Sprintf("%-11s %-30s ", state.Kind.Name, truncateString(state.Source, 30))
		line := label + status
		if idx == m.auth.selected {
			line = selectedStyle.Render(truncateString(label, inner)) + " " + status
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	if state, ok := m.auth.current(); ok {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(truncateString(state.Kind.Summary, inner)))
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(truncateString(state.Kind.Setup, inner)))
		b.WriteString("\n")
	}

	if m.auth.editing != nil {
		b.WriteString("\n")
		b.WriteString(headingStyle.Render(strings.ToUpper(m.auth.editing.Kind.Name)))
		b.WriteString("\n")
		b.WriteString(m.auth.input.View())
		b.WriteString("\n")
		// The value is masked, so without a count there is no way to tell a
		// paste landed at all - which is exactly how this screen failed the
		// first person to use it.
		if n := len([]rune(m.auth.input.Value())); n > 0 {
			b.WriteString(valueStyle.Render(fmt.Sprintf("%d characters", n)))
			b.WriteString("\n")
		}
		b.WriteString(mutedStyle.Render(pasteHint()))
		return panel.Render(b.String())
	}

	mode, why := resolvedAuthMode(m.authMode, m.auth.states)
	b.WriteString("\n")
	b.WriteString(headingStyle.Render("BILLING"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%-11s %s\n", "setting", m.authMode))
	// Which account sessions bill is not better or worse either way; the
	// resolved mode and the reason for it are both on the line, and the reason
	// is the part a surprised user actually needs.
	b.WriteString(fmt.Sprintf("%-11s %s %s\n", "sessions", valueStyle.Render(mode), mutedStyle.Render("("+why+")")))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("↑↓ move · enter set · x clear · m billing mode · R refresh · esc back"))

	return panel.Render(b.String())
}
