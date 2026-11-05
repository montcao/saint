package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
)

// Form rows, in tab order. Runtime is a row rather than a global shortcut so
// that choosing it cannot steal keys from the text fields.
const (
	formRowDir = iota
	formRowName
	formRowPrompt
	formRowRuntime
	formRowPermissions
	formRowCredential
	formRowCount
)

// credentialOption is one choice on the credential row.
type credentialOption struct {
	Spec string
	// Label is what the row shows. Remaining life is appended at render time.
	Label string
	// Note explains the trade in one line.
	Note string
}

// credentialOptions are ordered shortest-lived first, because a shorter-lived
// credential in a guest is the safer default: if a container is compromised the
// difference is hours of exposure rather than a year.
var credentialOptions = []credentialOption{
	{
		Spec:  store.CredentialClaudeCode,
		Label: "live login",
		Note:  "your Claude plan · shared by every session · expires in hours",
	},
	{
		Spec:  store.CredentialClaudeSubscription,
		Label: "stored token",
		Note:  "your Claude plan · shared by every session · lasts a year",
	},
	{
		Spec:  store.CredentialAPIKey,
		Label: "api key",
		Note:  "billed per token to the account that owns the key",
	},
}

type newSessionForm struct {
	repo    textinput.Model
	name    textinput.Model
	prompt  textinput.Model
	runtime sandbox.Runtime
	focus   int

	// skipPermissions runs the agent without its approval prompts.
	//
	// Defaults on: the microVM is the boundary, and an agent that stops to ask
	// before every edit inside its own sandbox is mostly friction. It is on the
	// form because the sandbox is not the only thing at stake - the session's
	// directory is mounted read-write and its contents are the user's.
	skipPermissions bool

	// credential is an index into credentialOptions.
	credential int

	// credentialLife is how long the selected credential has left, when saint
	// knows. Shown before the session is created rather than discovered when it
	// dies: the host's login is read at creation, so a session gets whatever is
	// left, not a full lifetime.
	credentialLife map[string]time.Duration

	// credentialErr explains why a credential cannot be used, if it cannot.
	credentialErr map[string]string
}

func newSessionDialog(defaultRepo string, defaultRuntime sandbox.Runtime) newSessionForm {
	return newSessionDialogWith(defaultRepo, defaultRuntime, "")
}

// newSessionDialogWith opens the form with a remembered credential choice.
func newSessionDialogWith(defaultRepo string, defaultRuntime sandbox.Runtime, defaultCredential string) newSessionForm {
	repo := textinput.New()
	repo.Prompt = ""
	repo.Placeholder = "~/code/project (any directory, git or not)"
	repo.SetValue(strings.TrimSpace(defaultRepo))
	repo.CharLimit = 400
	repo.Width = 56

	name := textinput.New()
	name.Prompt = ""
	name.Placeholder = "session name"
	name.SetValue(defaultNameFromRepo(defaultRepo))
	name.CharLimit = 80
	name.Width = 36

	prompt := textinput.New()
	prompt.Prompt = ""
	prompt.Placeholder = "optional bootstrap prompt"
	prompt.CharLimit = 300
	prompt.Width = 56

	repo.Focus()
	name.Blur()
	prompt.Blur()

	if defaultRuntime == "" {
		defaultRuntime = sandbox.RuntimeClaude
	}

	form := newSessionForm{
		skipPermissions: true,
		repo:            repo,
		name:            name,
		prompt:          prompt,
		runtime:         defaultRuntime,
		credentialLife:  map[string]time.Duration{},
		credentialErr:   map[string]string{},
	}
	for idx, option := range credentialOptions {
		if option.Spec == defaultCredential {
			form.credential = idx
		}
	}
	return form
}

// selectedCredential is the option the form is currently on.
func (f *newSessionForm) selectedCredential() credentialOption {
	if f.credential < 0 || f.credential >= len(credentialOptions) {
		return credentialOptions[0]
	}
	return credentialOptions[f.credential]
}

func (f *newSessionForm) cycleCredential(delta int) {
	f.credential = (f.credential + delta + len(credentialOptions)) % len(credentialOptions)
}

func (f *newSessionForm) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "shift+tab", "up", "down":
			f.moveFocus(msg.String())
			return nil
		case "left", "right":
			// Only on the runtime row. Everywhere else these move the cursor
			// inside the field, which is what every text input in every program
			// does. "h" and "l" used to toggle here too, which meant those two
			// letters could not be typed anywhere in the form.
			switch f.focus {
			case formRowRuntime:
				step := 1
				if msg.String() == "left" {
					step = -1
				}
				f.cycleRuntime(step)
				return nil
			case formRowPermissions:
				f.skipPermissions = !f.skipPermissions
				return nil
			case formRowCredential:
				step := 1
				if msg.String() == "left" {
					step = -1
				}
				f.cycleCredential(step)
				return nil
			}
		}
	}

	var cmd tea.Cmd
	switch f.focus {
	case formRowDir:
		f.repo, cmd = f.repo.Update(msg)
	case formRowName:
		f.name, cmd = f.name.Update(msg)
	case formRowPrompt:
		f.prompt, cmd = f.prompt.Update(msg)
	}
	return cmd
}

func (f *newSessionForm) Request() session.CreateRequest {
	return session.CreateRequest{
		Name:            strings.TrimSpace(f.name.Value()),
		Repos:           []sandbox.Repo{{Path: strings.TrimSpace(f.repo.Value())}},
		Runtime:         f.runtime,
		Prompt:          strings.TrimSpace(f.prompt.Value()),
		Credential:      f.selectedCredential().Spec,
		SkipPermissions: f.skipPermissions,
	}
}

func (f *newSessionForm) View(width int) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkSoft))
	// What the user is typing is the brightest thing in the dialog; the labels
	// beside it are a step down, because nobody has to read "directory" twice.
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkBright))
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	chosenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colInkBright)).Bold(true)
	inactiveStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colFocusRule)).
		Padding(1, 2).
		Width(min(width-6, 76))

	// choice renders one option of a side-by-side row.
	//
	// The dot is not decoration. Which option is live used to be carried by
	// colour and weight alone, which was survivable while the live one was cyan
	// among greys and is not now that it is a brighter grey among greys: the
	// difference between 255 bold and 244 is small, it is invisible in a
	// low-contrast terminal theme, and the thing it distinguishes is which
	// credential a container is about to be given. A filled dot beside the live
	// option says it in a channel that does not depend on the theme rendering
	// two greys differently.
	choice := func(live bool, text string) string {
		if live {
			return chosenStyle.Render("● " + text)
		}
		return inactiveStyle.Render("○ " + text)
	}

	runtimes := make([]string, 0, len(formRuntimes))
	for _, runtime := range formRuntimes {
		runtimes = append(runtimes, choice(runtime == f.runtime, string(runtime)))
	}
	// A visible marker on the focused row. A blinking cursor is easy to miss,
	// and the runtime row has no cursor at all - without this there is no way
	// to tell it is selected.
	label := func(row int, text string) string {
		if f.focus == row {
			return chosenStyle.Render("▸ " + text)
		}
		return labelStyle.Render("  " + text)
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("New Session"))
	b.WriteString("\n")
	b.WriteString(label(formRowDir, "directory"))
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(f.repo.View()))
	b.WriteString("\n\n")
	b.WriteString(label(formRowName, "name"))
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(f.name.View()))
	b.WriteString("\n\n")
	b.WriteString(label(formRowPrompt, "prompt"))
	b.WriteString("\n")
	b.WriteString(valueStyle.Render(f.prompt.View()))
	b.WriteString("\n\n")
	b.WriteString(label(formRowRuntime, "runtime"))
	b.WriteString("\n")
	b.WriteString(strings.Join(runtimes, "  "))
	b.WriteString("\n")
	b.WriteString(inactiveStyle.Render("credentials below apply to this runtime"))
	b.WriteString("\n\n")

	b.WriteString(label(formRowPermissions, "permissions"))
	b.WriteString("\n")
	auto := choice(f.skipPermissions, "skip (sandboxed)")
	ask := choice(!f.skipPermissions, "ask")
	b.WriteString(auto)
	b.WriteString("  ")
	b.WriteString(ask)
	b.WriteString("\n")
	if f.skipPermissions {
		b.WriteString(inactiveStyle.Render("the agent edits without asking · the microVM is the boundary"))
	} else {
		b.WriteString(inactiveStyle.Render("the agent asks before each action · slower, and you are the boundary"))
	}
	b.WriteString("\n\n")

	b.WriteString(label(formRowCredential, "credential"))
	b.WriteString("\n")
	for idx, option := range credentialOptions {
		text := option.Label
		// Remaining life goes next to the choice, not in a warning after the
		// fact: the host's login is read at creation, so this session gets
		// what is left rather than a full lifetime.
		if life, ok := f.credentialLife[option.Spec]; ok {
			text += fmt.Sprintf(" (%s left)", formatLife(life))
		}
		if reason, bad := f.credentialErr[option.Spec]; bad {
			text += " (" + reason + ")"
		}
		b.WriteString(choice(idx == f.credential, text))
		b.WriteString("  ")
	}
	b.WriteString("\n")
	b.WriteString(inactiveStyle.Render(f.selectedCredential().Note))
	b.WriteString("\n\n")

	hint := "enter create  ·  tab next field  ·  esc cancel"
	switch f.focus {
	case formRowRuntime:
		hint = "←/→ choose runtime  ·  enter create  ·  tab next field  ·  esc cancel"
	case formRowPermissions:
		hint = "←/→ choose permissions  ·  enter create  ·  tab next field  ·  esc cancel"
	case formRowCredential:
		hint = "←/→ choose credential  ·  enter create  ·  tab next field  ·  esc cancel"
	}
	b.WriteString(inactiveStyle.Render(hint))

	return panel.Render(b.String())
}

func (f *newSessionForm) moveFocus(key string) {
	switch key {
	case "shift+tab", "up":
		f.focus = (f.focus + formRowCount - 1) % formRowCount
	default:
		f.focus = (f.focus + 1) % formRowCount
	}

	f.repo.Blur()
	f.name.Blur()
	f.prompt.Blur()
	switch f.focus {
	case formRowDir:
		f.repo.Focus()
	case formRowName:
		f.name.Focus()
	case formRowPrompt:
		f.prompt.Focus()
	}
}

// formRuntimes are the runtimes a session can be created with, in the order
// they are offered.
var formRuntimes = []sandbox.Runtime{
	sandbox.RuntimeClaude,
	sandbox.RuntimeCodex,
}

// cycleRuntime moves through the runtimes. It is a cycle rather than a toggle
// because there are more than two now, and a toggle silently ignores the rest.
func (f *newSessionForm) cycleRuntime(delta int) {
	current := 0
	for idx, runtime := range formRuntimes {
		if runtime == f.runtime {
			current = idx
		}
	}
	f.runtime = formRuntimes[(current+delta+len(formRuntimes))%len(formRuntimes)]
}

// applyCredentialStates fills the credential row from what saint could read.
//
// Options that cannot be used say why on the row itself. A credential under the
// minimum life is called out here, before the session exists, rather than as a
// warning after it has already been created against an API key.
func (f *newSessionForm) applyCredentialStates(states []CredentialState, now time.Time) {
	f.credentialLife = map[string]time.Duration{}
	f.credentialErr = map[string]string{}

	byConfig := make(map[string]CredentialState, len(states))
	for _, state := range states {
		byConfig[state.Kind.Config] = state
	}

	for _, option := range credentialOptions {
		// The API key needs no credential of its own here; the proxy holds it.
		if option.Spec == store.CredentialAPIKey {
			continue
		}
		state, known := byConfig[option.Spec]
		if !known {
			continue
		}
		switch {
		case state.Err != nil:
			f.credentialErr[option.Spec] = statusLine(state.Err.Error())
		case !state.Present:
			f.credentialErr[option.Spec] = "not set"
		case state.ExpiresAt.IsZero():
			// No expiry saint can see. Say nothing rather than implying it
			// lasts forever.
		default:
			left := state.ExpiresAt.Sub(now)
			f.credentialLife[option.Spec] = left
			if left < session.MinCredentialLife {
				f.credentialErr[option.Spec] = "too little left"
			}
		}
	}
}

// formatLife renders a duration the way someone reads a countdown.
func formatLife(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dm", hours, minutes)
}

func defaultNameFromRepo(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return name
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
