package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/store"
)

func proxyModel(t *testing.T) Model {
	t.Helper()

	st := store.New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	loader := &SnapshotLoader{Store: st, Sandbox: &fakeSandbox{}, Inspector: NewRepoInspector("/usr/bin/git", nil)}

	m := NewModel(loader, nil)
	m.width, m.height = 150, 32
	m.proxyStatus = "proxy: 192.168.64.1:8787"
	return m
}

// The rail states what is true; the proxy screen is where the parts that are a
// choice get changed. Before this, enter on a proxy row only re-probed it.
func TestProxyRowOpensTheProxyScreen(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	m.railOpen = true
	m.mode = modeRail
	m.rail.selected = indexOfRailLabel(t, m, "settings")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(Model).mode; got != modeProxy {
		t.Fatalf("mode = %v, want the proxy screen", got)
	}
}

func TestProxyScreenCyclesAndPersistsBilling(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	m.mode = modeProxy
	before := m.config().ProxyAuthOrDefault()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	after := m.config().ProxyAuthOrDefault()
	if after == before {
		t.Fatalf("billing did not change from %q", before)
	}
	// The change is written, not just held in memory: a setting that reverts on
	// restart is worse than one that cannot be changed.
	if m.authMode != after {
		t.Errorf("model authMode = %q, want the persisted %q", m.authMode, after)
	}
}

func TestProxyScreenTogglesTheJournal(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	m.mode = modeProxy
	m.proxy.selected = indexOfProxyLabel(t, m, "journal")
	before := m.config().ProxyLogRequestsOrDefault()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if got := m.config().ProxyLogRequestsOrDefault(); got == before {
		t.Errorf("journal did not toggle from %v", before)
	}
}

// A setting this screen does not edit says where it lives rather than silently
// doing nothing.
func TestProxyScreenNamesTheConfigKeyForValuesItCannotEdit(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	m.mode = modeProxy
	m.proxy.selected = indexOfProxyLabel(t, m, "port")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if status := next.(Model).status; !strings.Contains(status, "proxy_port") {
		t.Errorf("status = %q, want it to name the config key", status)
	}
}

// esc goes back to the rail it was opened from, not out to the list.
func TestProxyScreenReturnsToTheRail(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	m.mode = modeProxy

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := next.(Model).mode; got != modeRail {
		t.Errorf("mode = %v, want the rail", got)
	}
}

func indexOfProxyLabel(t *testing.T, m Model, label string) int {
	t.Helper()

	for idx, setting := range m.proxySettings() {
		if setting.Label == label {
			return idx
		}
	}
	t.Fatalf("no proxy setting labelled %q", label)
	return 0
}

// The journal row used to render 4 MiB as "4.19M bytes": a byte count pushed
// through the token formatter, which divides by 1000. Wrong unit, and a wrong
// number dressed as a right one.
func TestFormatBytesIsNotTheTokenFormatter(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0:        "0 B",
		512:      "512 B",
		1 << 10:  "1.0 KiB",
		4 << 20:  "4.0 MiB",
		20 << 20: "20.0 MiB",
		3 << 30:  "3.0 GiB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// The number a user wants from this row is what it costs them on disk, not the
// rotation point on its own.
func TestJournalRowStatesTheTotalOnDisk(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	setting := m.proxySettings()[indexOfProxyLabel(t, m, "journal size")]

	if !strings.Contains(setting.Value, "MiB") {
		t.Errorf("value = %q, want a byte size", setting.Value)
	}
	if strings.Contains(setting.Value, "M bytes") {
		t.Errorf("value = %q, still using the token formatter", setting.Value)
	}
	for _, want := range []string{"rotates at", "keeping"} {
		if !strings.Contains(setting.Note, want) {
			t.Errorf("note = %q, want it to explain %q", setting.Note, want)
		}
	}
}

// Guest egress is a security setting whose value the proxy reads once, at
// startup, and whose environment variables are written once, when a container
// is created. Telling a user it is "now on" while the running proxy still
// refuses to forward is the interface giving a wrong answer it had the
// information to avoid.
func TestProxySettingsSayWhenAChangeTakesEffect(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	byLabel := map[string]proxySetting{}
	for _, setting := range m.proxySettings() {
		byLabel[setting.Label] = setting
	}

	for _, label := range []string{"guest egress", "journal", "billing"} {
		setting, ok := byLabel[label]
		if !ok {
			t.Fatalf("no %q setting on the proxy screen", label)
		}
		if setting.Cycle == nil {
			t.Errorf("%q cannot be changed from the screen it is shown on", label)
		}
		if setting.Applies == "" {
			t.Errorf("%q does not say when a change to it lands", label)
		}
	}
}

// Egress proxying is off unless asked for, and both readings say in words what
// they mean - colour is never the only carrier.
func TestGuestEgressDefaultsToDirectAndDoesNotOverclaim(t *testing.T) {
	t.Parallel()

	egressSetting := func(m Model) proxySetting {
		t.Helper()
		for _, setting := range m.proxySettings() {
			if setting.Label == "guest egress" {
				return setting
			}
		}
		t.Fatal("no guest egress setting on the proxy screen")
		return proxySetting{}
	}

	m := proxyModel(t)
	off := egressSetting(m)
	if off.Value != "direct" {
		t.Errorf("default guest egress = %q, want %q", off.Value, "direct")
	}
	if off.Note == "" {
		t.Error("guest egress states no consequence beside the choice")
	}

	cfg := off.Cycle(m.config())
	if !cfg.ProxyGuestEgressOrDefault() {
		t.Fatal("cycling guest egress did not turn it on")
	}
	if err := m.loader.Store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	on := egressSetting(m)
	if on.Value == off.Value {
		t.Errorf("guest egress reads %q both on and off", on.Value)
	}
	// The claim has to stay bounded: saint observes the hosts a proxy-honouring
	// client reaches. It does not stop a guest that ignores the variables.
	if !strings.Contains(on.Note, "not an egress control") {
		t.Errorf("guest egress note overclaims: %q", on.Note)
	}
}

// Six settings, and exactly one of them can be a fault. The others are choices
// with two legitimate readings each - billed to a key or to a plan, journal on
// or off, egress direct or observed - and a screen that colours a correct
// choice is a screen whose colour cannot be trusted when something is actually
// wrong.
func TestProxyScreenSpendsAttentionOnlyOnABindNoGuestCanReach(t *testing.T) {
	t.Parallel()

	m := proxyModel(t)
	for _, setting := range m.proxySettings() {
		if setting.State == railNeedsPerson {
			t.Errorf("%q = %q asks for a person on a proxy that is working",
				setting.Label, setting.Value)
		}
	}

	// Pinned to loopback the proxy still starts, still binds, still reports an
	// address, and no guest will ever reach it: every session either fails to
	// authenticate or goes around saint entirely. That is the one reading here
	// that nobody chose on purpose.
	cfg := m.config()
	cfg.ProxyBind = store.ProxyBindLoopback
	if err := m.loader.Store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	bind := m.proxySettings()[indexOfProxyLabel(t, m, "bind")]
	if bind.State != railNeedsPerson {
		t.Errorf("a loopback bind = state %v, want it to ask for a person", bind.State)
	}
	// The note under a row is only drawn while that row is selected, and an IP
	// address does not tell most people what it implies - so the value itself
	// has to say it. Colour is never the only carrier.
	if !strings.Contains(bind.Value, "host only") {
		t.Errorf("bind value = %q, want it to say in words that guests cannot reach it", bind.Value)
	}
}
