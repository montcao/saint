package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
)

// proxySetting is one row on the proxy screen.
type proxySetting struct {
	Label string
	Value string
	// Note explains the consequence of the current value, in one line, next to
	// the choice that causes it.
	Note string
	// Cycle advances the setting. Nil means the value is shown but not editable
	// here, in which case Detail says where it lives.
	Cycle func(store.Config) store.Config
	// Detail names the config key, for settings this screen does not edit.
	Detail string
	// Applies says when a change lands, for settings nothing re-reads while
	// saint is running. Empty means the change is in force as soon as it is
	// saved.
	//
	// It exists because the alternative is a screen that says "on" next to a
	// thing that is off: the proxy reads most of these once at startup, and a
	// user who toggles a security setting and is told it is now on has been
	// given a wrong answer by an interface that knew better.
	Applies string
	State   railState
}

// proxyScreen is the settings surface behind the rail's proxy rows.
//
// The rail states what is true; this is where the parts of it that are a choice
// are changed. Settings that are a small closed set are cycled here; the ones
// that are a number are shown with the key they live under, because a number
// editor is a different thing from a settings screen and inventing one here
// would be worse than saying where the value is.
type proxyScreen struct {
	selected int
}

func (m Model) openProxy() (tea.Model, tea.Cmd) {
	m.mode = modeProxy
	m.proxy.selected = 0
	return m, nil
}

func (m Model) proxySettings() []proxySetting {
	cfg := m.config()
	mode, why := resolvedAuthMode(cfg.ProxyAuthOrDefault(), m.auth.states)

	// Both readings are a working proxy billing a real account, so neither is
	// asking for anything. The one that costs money per token says "api key" on
	// the row and spells out the consequence in the note beneath it.
	billing := proxySetting{
		Label:   "billing",
		Value:   cfg.ProxyAuthOrDefault(),
		Note:    mode + " · " + why,
		Applies: "applies to sessions created from now on",
		Cycle: func(c store.Config) store.Config {
			c.ProxyAuth = nextAuthMode(c.ProxyAuthOrDefault())
			return c
		},
	}

	// A journal that is off is the shipped default and a legitimate choice - one
	// that writes nothing about what an agent asked for is the private one. Both
	// readings say what they do in the note, and neither needs a person.
	journal := proxySetting{
		Label:   "journal",
		Value:   "off",
		Note:    "request records are not written",
		Applies: "takes effect when saint restarts",
		Cycle: func(c store.Config) store.Config {
			c.ProxyLogRequests = !c.ProxyLogRequestsOrDefault()
			return c
		},
	}
	if cfg.ProxyLogRequestsOrDefault() {
		journal.Value = "on"
		journal.Note = "every request is recorded · saint proxy log"
	}

	// A loopback bind is the one setting on this screen that needs a person: the
	// proxy starts, binds, reports an address and looks entirely healthy, and no
	// guest can reach it, so every session either fails to authenticate or goes
	// around saint entirely.
	//
	// The address alone does not say that - 127.0.0.1 means nothing to most
	// people and the note underneath is only drawn for the selected row - so the
	// row carries the words as well as the colour.
	bind := cfg.ProxyBindHost()
	bindNote := "discovered at runtime · reachable by guests"
	bindState := railReports
	if cfg.ProxyBindIsLoopback() {
		bind += " · host only"
		bindNote = "host only · guests cannot reach it"
		bindState = railNeedsPerson
	}

	// Off is the calm state here: a session whose traffic leaves the microVM
	// directly is what saint has always done. Turning it on is the choice with a
	// consequence, and the consequence is a sentence rather than a colour -
	// seeing every host a session reaches is a capability, not a fault.
	egress := proxySetting{
		Label:   "guest egress",
		Value:   "direct",
		Note:    "session traffic leaves the microVM unseen",
		Applies: "takes effect when saint restarts, for new sessions",
		Cycle: func(c store.Config) store.Config {
			c.ProxyGuestEgress = !c.ProxyGuestEgressOrDefault()
			return c
		},
	}
	if cfg.ProxyGuestEgressOrDefault() {
		egress.Value = "through saint"
		egress.Note = "saint sees every host a session reaches · not an egress control"
	}

	return []proxySetting{
		billing,
		journal,
		egress,
		{
			Label: "bind", Value: bind, Note: bindNote, State: bindState,
			Detail: `"proxy_bind" in ~/.saint/config.json`,
		},
		{
			Label: "port", Value: fmt.Sprintf("%d", cfg.ProxyPortOrDefault()),
			Detail: `"proxy_port" in ~/.saint/config.json`,
		},
		{
			Label: "network", Value: cfg.ProxyNetworkOrDefault(),
			Detail: `"proxy_network" in ~/.saint/config.json`,
		},
		{
			// The number a user actually wants is the total, not the rotation
			// point: "how much disk does keeping this history cost me".
			Label: "journal size",
			Value: formatBytes(cfg.ProxyLogMaxBytesOrDefault()*int64(cfg.ProxyLogKeepFilesOrDefault())) + " max",
			Note: fmt.Sprintf("rotates at %s, keeping %d files",
				formatBytes(cfg.ProxyLogMaxBytesOrDefault()), cfg.ProxyLogKeepFilesOrDefault()),
			Detail: `"proxy_log_max_bytes" and "proxy_log_keep_files" in ~/.saint/config.json`,
		},
	}
}

func (m *Model) proxyMove(delta int) {
	count := len(m.proxySettings())
	if count == 0 {
		m.proxy.selected = 0
		return
	}
	m.proxy.selected = (m.proxy.selected + delta + count) % count
}

func (m Model) updateProxy(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		// Back to the rail it was opened from, not out to the list.
		m.mode = modeRail
		return m, nil
	case forceQuitKey, "ctrl+c":
		m.closePane()
		return m, tea.Quit
	case "up", "k":
		m.proxyMove(-1)
		return m, nil
	case "down", "j":
		m.proxyMove(1)
		return m, nil
	case "enter", "left", "right", " ":
		settings := m.proxySettings()
		if m.proxy.selected < 0 || m.proxy.selected >= len(settings) {
			return m, nil
		}
		setting := settings[m.proxy.selected]
		if setting.Cycle == nil {
			m.status = setting.Detail
			return m, nil
		}
		if m.loader == nil || m.loader.Store == nil {
			return m, nil
		}
		cfg, err := m.loader.Store.EnsureConfig()
		if err != nil {
			m.lastError = err
			m.status = statusLine(err.Error())
			return m, nil
		}
		cfg = setting.Cycle(cfg)
		if err := m.loader.Store.SaveConfig(cfg); err != nil {
			m.lastError = err
			m.status = statusLine(err.Error())
			return m, nil
		}
		m.authMode = cfg.ProxyAuthOrDefault()
		updated := m.proxySettings()[m.proxy.selected]
		m.status = setting.Label + " is now " + updated.Value
		if updated.Applies != "" {
			m.status += " · " + updated.Applies
		}
		return m, m.loadCredentialsCmd()
	}
	return m, nil
}

func (m Model) renderProxyScreen() string {
	width := min(max(52, m.width-8), 76)
	inner := width - 6

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colFocusRule)).
		Padding(1, 2).
		Width(width - 2)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright))
	selectedStyle := selectionStyle()

	var b strings.Builder
	b.WriteString(titleStyle.Render("Proxy"))
	b.WriteString("\n")
	b.WriteString(sidebarMuted(truncateString("every session's traffic crosses this hop", inner)))
	b.WriteString("\n\n")

	for idx, setting := range m.proxySettings() {
		gap := max(1, inner-lipgloss.Width(setting.Label)-lipgloss.Width(setting.Value))
		line := setting.Label + strings.Repeat(" ", gap) + setting.Value
		if idx == m.proxy.selected {
			b.WriteString(selectedStyle.Render(truncateString(line, inner)))
		} else {
			b.WriteString(sidebarMuted(setting.Label) + strings.Repeat(" ", gap) +
				railValueStyle(setting.State).Render(setting.Value))
		}
		b.WriteString("\n")

		// The consequence sits under the choice that causes it, not in a
		// warning after the fact.
		if idx == m.proxy.selected {
			note := setting.Note
			if note == "" {
				note = setting.Detail
			}
			if note != "" {
				b.WriteString(sidebarMuted(truncateString("  "+note, inner)))
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(sidebarMuted(truncateString("↑↓ move · enter change · esc back", inner)))
	return box.Render(b.String())
}
