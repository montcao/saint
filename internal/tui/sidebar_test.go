package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

func meteredRow(name string, u usage.Session) SessionRow {
	return SessionRow{
		ID: "abc123", Name: name, Runtime: "claude", State: "running", LastRun: "2m ago",
		UsageKnown: true, Usage: u,
	}
}

func TestLayoutAppearsOnlyWhenThereIsRoom(t *testing.T) {
	t.Parallel()

	cases := []struct {
		width     int
		wantRight bool
		wantMain  int
	}{
		{width: 0, wantRight: false, wantMain: 0},
		{width: 80, wantRight: false, wantMain: 80},
		{width: sidebarWidth + minMainWidth - 1, wantRight: false, wantMain: sidebarWidth + minMainWidth - 1},
		{width: sidebarWidth + minMainWidth, wantRight: true, wantMain: minMainWidth},
		// Past the threshold every extra column goes to the list, not to more
		// chrome.
		{width: 200, wantRight: true, wantMain: 200 - sidebarWidth},
	}

	for _, tc := range cases {
		m := Model{width: tc.width}
		if got := m.sidebarVisible(); got != tc.wantRight {
			t.Errorf("sidebarVisible() at width %d = %v, want %v", tc.width, got, tc.wantRight)
		}
		if got := m.contentWidth(); got != tc.wantMain {
			t.Errorf("contentWidth() at width %d = %d, want %d", tc.width, got, tc.wantMain)
		}
	}
}

// A line wider than the terminal wraps, and a wrapped frame in the alt screen
// looks like a corrupted one.
func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	t.Parallel()

	for _, width := range []int{70, 97, 98, 120, 200} {
		m := newRenderModel(t, width, 40)
		for i, line := range strings.Split(m.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: line %d is %d columns:\n%q", width, i, got, line)
			}
		}
	}
}

// The pane's column count becomes the PTY window size. If it still measured
// against the terminal rather than the main column, the agent would believe it
// had the side panes' columns too and wrap into them.
func TestPaneSizeYieldsColumnsToVisibleSidePanes(t *testing.T) {
	t.Parallel()

	wide := Model{width: 200, height: 40}
	cols, _ := wide.paneSize()
	if want := 200 - sidebarWidth - paneChromeCols; cols != want {
		t.Errorf("paneSize() cols = %d, want %d", cols, want)
	}

	medium := Model{width: sidebarWidth + minMainWidth, height: 40}
	cols, _ = medium.paneSize()
	if want := minMainWidth - paneChromeCols; cols != want {
		t.Errorf("paneSize() cols with right pane only = %d, want %d", cols, want)
	}

	narrow := Model{width: 80, height: 40}
	cols, _ = narrow.paneSize()
	if want := 80 - paneChromeCols; cols != want {
		t.Errorf("paneSize() cols with no side panes = %d, want %d", cols, want)
	}
}

func TestWideViewShowsTheSidebar(t *testing.T) {
	t.Parallel()

	m := newRenderModel(t, sidebarWidth+minMainWidth+40, 40)
	view := m.View()
	if !strings.Contains(view, "CONTEXT") {
		t.Fatalf("wide view is missing the right pane:\n%s", view)
	}
}

func TestContextBlock(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		session     usage.Session
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "known window",
			session:     usage.Session{Model: "claude-opus-5", Context: 100_000, Reported: true},
			wantContain: []string{"50%", "100.0k / 200.0k"},
		},
		{
			// A model saint has no verified window for must not get a
			// percentage invented for it.
			name:        "unknown model",
			session:     usage.Session{Model: "some-new-model", Context: 12_345, Reported: true},
			wantContain: []string{"12.3k", "window unknown"},
			wantAbsent:  []string{"%"},
		},
		{
			// Past the table's window means a larger one is in play. Pinning
			// the bar at 100% would be a confident lie.
			name:        "over the table's window",
			session:     usage.Session{Model: "claude-sonnet-5", Context: 900_000, Reported: true},
			wantContain: []string{"window unknown"},
			wantAbsent:  []string{"%"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := contextBlock(tc.session)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("contextBlock() = %q, want it to contain %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("contextBlock() = %q, want it to omit %q", got, absent)
				}
			}
		})
	}
}

func TestRenderBar(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fraction   float64
		width      int
		wantFilled int
	}{
		{fraction: 0, width: 10, wantFilled: 0},
		// A session with real context never renders as an empty bar.
		{fraction: 0.01, width: 10, wantFilled: 1},
		{fraction: 0.5, width: 10, wantFilled: 5},
		{fraction: 1, width: 10, wantFilled: 10},
		{fraction: 2, width: 10, wantFilled: 10},
		{fraction: -1, width: 10, wantFilled: 0},
	}

	for _, tc := range cases {
		bar := renderBar(tc.fraction, tc.width)
		if got := lipgloss.Width(bar); got != tc.width {
			t.Errorf("renderBar(%v) width = %d, want %d", tc.fraction, got, tc.width)
		}
		if got := strings.Count(bar, "█"); got != tc.wantFilled {
			t.Errorf("renderBar(%v) filled = %d, want %d", tc.fraction, got, tc.wantFilled)
		}
	}
	if got := renderBar(0.5, 0); got != "" {
		t.Errorf("renderBar with no width = %q, want empty", got)
	}
}

func TestSidebarStatesStayDistinct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  SessionRow
		want string
	}{
		{
			name: "nothing proxied yet",
			row:  SessionRow{ID: "abc123", Name: "demo", Runtime: "claude", State: "running"},
			want: "no proxied requests yet",
		},
		{
			name: "provider reported nothing",
			row:  meteredRow("demo", usage.Session{Requests: 2}),
			want: "provider sent no counters",
		},
		{
			name: "measured",
			row: meteredRow("demo", usage.Session{
				Model: "claude-opus-5", Requests: 3,
				InputTokens: 1200, OutputTokens: 340, Context: 100_000, Reported: true,
			}),
			want: "50%",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := Model{width: 120, height: 40, rows: []SessionRow{tc.row}}
			got := m.renderSidebar(20)
			if !strings.Contains(got, tc.want) {
				t.Errorf("sidebar is missing %q:\n%s", tc.want, got)
			}
		})
	}
}

func TestUsageTotalsSkipUnmeasuredSessions(t *testing.T) {
	t.Parallel()

	m := Model{rows: []SessionRow{
		meteredRow("a", usage.Session{Requests: 2, InputTokens: 100, OutputTokens: 10, Reported: true}),
		meteredRow("b", usage.Session{Requests: 3, InputTokens: 200, OutputTokens: 20, Reported: true}),
		// Counted by neither: no counters ever arrived.
		meteredRow("c", usage.Session{Requests: 9}),
		{ID: "d", Name: "d"},
	}}

	total := m.usageTotals()
	if total.Requests != 5 || total.InputTokens != 300 || total.OutputTokens != 30 {
		t.Errorf("usageTotals() = %+v, want 5 requests, 300 in, 30 out", total)
	}
}

func newRenderModel(t *testing.T, width, height int) Model {
	t.Helper()

	m := NewModel(nil, nil)
	m.width = width
	m.height = height
	m.status = strings.Repeat("a long status line ", 12)
	m.rows = []SessionRow{
		meteredRow("alpha", usage.Session{
			Model: "claude-opus-5", Requests: 37,
			InputTokens: 1_200_000, OutputTokens: 84_100,
			CacheReadTokens: 900, CacheWriteTokens: 64,
			Context: 128_400, Reported: true,
		}),
		{ID: "bbb", Name: "beta", Runtime: "codex", State: "stopped", LastRun: "1h ago"},
	}
	return m
}

// The attached layout is the one that matters most: the pane's width becomes
// the PTY window size, so an over-wide frame here is a garbled agent.
func TestAttachedViewFitsTerminalWidth(t *testing.T) {
	t.Parallel()

	for _, width := range []int{80, 98, 120, 160} {
		m := newRenderModel(t, width, 30)
		cols, rows := m.paneSize()
		pane, err := startSessionPane("abc123", "alpha", exec.Command("/bin/cat"), cols, rows)
		if err != nil {
			t.Fatalf("startSessionPane() error = %v", err)
		}
		m.pane = pane
		m.mode = modeAttached

		view := m.View()
		pane.Close()

		for i, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("attached at width %d: line %d is %d columns:\n%q", width, i, got, line)
			}
		}
		if width >= sidebarWidth+minMainWidth && !strings.Contains(view, "CONTEXT") {
			t.Errorf("attached at width %d: the sidebar is missing", width)
		}
	}
}

// One selection style everywhere. The rail, the proxy screen, the auth screen
// and the session list all move a cursor, and "where am I" should be one shape
// the eye learns once rather than four that have to be recognised separately.
func TestSelectionIsOneStyleOnTheGreyRamp(t *testing.T) {
	t.Parallel()

	style := selectionStyle()
	if got := style.GetBackground(); got != lipgloss.Color(colSelect) {
		t.Errorf("selection ground = %v, want the palette's selected ground", got)
	}
	if got := style.GetForeground(); got != lipgloss.Color(colOnDark) {
		t.Errorf("selection text = %v, want the palette's text-on-light", got)
	}
	// Not the attention colour: a selection is a statement about where the
	// cursor is, which the user created by pressing a key, not about whether
	// anything is wrong.
	if style.GetBackground() == lipgloss.Color(colAttention) {
		t.Error("the selected row is drawn in the colour reserved for things needing a person")
	}
}

// Border colour carries the only rank in the system: a surface that has the
// keyboard against one that only reports. That used to be indigo against grey;
// it is now two steps of grey, and the two steps have to stay distinct or the
// rank disappears silently.
func TestAFocusedSurfaceIsBorderedDifferentlyFromAReportingOne(t *testing.T) {
	t.Parallel()

	if colFocusRule == colRule {
		t.Error("the rail, the dialogs and the sidebar all border the same, so nothing says which one has the keys")
	}
}

// Which account a session bills is information, not a request - it cannot even
// be changed from here - so it says which one in words rather than leaning on a
// colour it used to share with a credential about to expire one line below.
func TestBillingLabelNamesTheAccountInWords(t *testing.T) {
	t.Parallel()

	if got := billingLabel(store.ProxyAuthSubscription); !strings.Contains(got, "subscription") {
		t.Errorf("billingLabel(subscription) = %q, want it to name the plan", got)
	}
	if got := billingLabel(store.ProxyAuthKey); !strings.Contains(got, "api key") || !strings.Contains(got, "billed") {
		t.Errorf("billingLabel(key) = %q, want it to say the session is billed per token", got)
	}
}

// The rail can honestly say the plan login has hours left while this line
// honestly says the session's token is finished: a session's credential is a
// copy taken at container creation, and a process environment cannot be changed
// afterwards. Two different things were both called "credential", which read as
// saint contradicting itself.
func TestSessionCredentialLineNamesWhoseCredentialItIs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	expired := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(-time.Hour)}, now)
	if !strings.Contains(expired, "session credential") {
		t.Errorf("expired line does not say whose credential it is: %q", expired)
	}
	// And it must name the remedy, or the reader is told about a problem with
	// no way to act on it.
	if !strings.Contains(expired, "c ") {
		t.Errorf("expired line names no key to fix it: %q", expired)
	}

	healthy := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(6 * time.Hour)}, now)
	if !strings.Contains(healthy, "session credential") {
		t.Errorf("healthy line does not say whose credential it is: %q", healthy)
	}
	if strings.Contains(healthy, "renew") {
		t.Errorf("a credential with hours left asks to be renewed: %q", healthy)
	}

	soon := credentialLifeLine(SessionRow{CredentialExpiresAt: now.Add(5 * time.Minute)}, now)
	if !strings.Contains(soon, "renews") {
		t.Errorf("a nearly-expired credential names no remedy: %q", soon)
	}

	if got := credentialLifeLine(SessionRow{}, now); got != "" {
		t.Errorf("an unknown expiry rendered %q; unknown is not a claim", got)
	}
}

// The request and partial counts are counts of something, and this is what.
//
// A bare "412 requests" invites the reader to assume it means turns they took.
// It does not - it counts every call the agent made, including the ones it
// makes on its own behalf - and naming the upstreams is the difference between
// a number and a number with a subject.
func TestSidebarNamesTheUpstreamsBehindTheRequestCount(t *testing.T) {
	t.Parallel()

	got := stripEscapes(renderEndpoints(map[string]usage.EndpointCount{
		"api.anthropic.com/v1/messages":       {Requests: 412, Partial: 3},
		"api.anthropic.com/v1/models":         {Requests: 2},
		"statsig.anthropic.com/v1/initialize": {Requests: 9},
	}))

	for _, want := range []string{"REACHED", "/v1/messages", "412", "3 partial", "statsig.anthropic.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("the endpoint list omits %q:\n%s", want, got)
		}
	}
	// Busiest first, so the line that accounts for most of the count is read
	// first.
	if strings.Index(got, "/v1/messages") > strings.Index(got, "/v1/models") {
		t.Errorf("endpoints are not ordered by traffic:\n%s", got)
	}
	// The address is shown whole. The host is the part that says where the
	// traffic went, and a list that trimmed it could not answer the question
	// the section exists for. Long ones wrap rather than truncate, so the
	// assertion joins the lines back up - a cut address would still fail.
	rejoined := strings.ReplaceAll(strings.ReplaceAll(got, "\n  ", ""), "\n", "")
	for _, want := range []string{
		"api.anthropic.com/v1/messages",
		"statsig.anthropic.com/v1/initialize",
	} {
		if !strings.Contains(rejoined, want) {
			t.Errorf("address %q was not shown in full:\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Errorf("an address was truncated; a cut address cannot be checked against anything:\n%s", got)
	}
}

// Nothing measured is not the same as nothing reached.
func TestSidebarSaysNothingAboutUpstreamsItNeverSaw(t *testing.T) {
	t.Parallel()

	if got := renderEndpoints(nil); got != "" {
		t.Errorf("renderEndpoints(nil) = %q, want nothing", got)
	}
	if got := renderEndpoints(map[string]usage.EndpointCount{}); got != "" {
		t.Errorf("renderEndpoints(empty) = %q, want nothing", got)
	}
}

// The panel has a fixed height, so the list is capped - and says so, rather
// than quietly ending.
func TestTheEndpointListSaysWhatItLeftOut(t *testing.T) {
	t.Parallel()

	many := map[string]usage.EndpointCount{}
	for i := 0; i < endpointsShown+4; i++ {
		many[fmt.Sprintf("host%02d.example.com/v1/x", i)] = usage.EndpointCount{Requests: endpointsShown + 4 - i}
	}
	got := stripEscapes(renderEndpoints(many))
	if !strings.Contains(got, "4 more") {
		t.Errorf("a truncated endpoint list does not say how many were left out:\n%s", got)
	}
}

// Wrapping breaks after a path separator where it can. A break after a slash is
// a place a reader can rejoin the halves; a break inside a hostname is not.
func TestAddressWrappingBreaksOnPathSeparators(t *testing.T) {
	t.Parallel()

	got := wrapAddress("statsig.anthropic.com/v1/initialize", 30)
	if len(got) < 2 {
		t.Fatalf("wrapAddress() = %q, expected it to wrap", got)
	}
	if !strings.HasSuffix(got[0], "/") {
		t.Errorf("first line %q does not end on a path separator", got[0])
	}
	if rejoined := strings.ReplaceAll(strings.Join(got, ""), "  ", ""); rejoined != "statsig.anthropic.com/v1/initialize" {
		t.Errorf("wrapping lost or altered the address: %q", rejoined)
	}
	// Anything that fits is one line, untouched.
	if got := wrapAddress("api.openai.com/v1/responses", 30); len(got) != 1 || got[0] != "api.openai.com/v1/responses" {
		t.Errorf("wrapAddress() split an address that fits: %q", got)
	}
	// A hostname with no separator in range still has to come out whole.
	long := strings.Repeat("a", 40) + ".example.com"
	if rejoined := strings.ReplaceAll(strings.Join(wrapAddress(long, 30), ""), "  ", ""); rejoined != long {
		t.Errorf("a separator-less address was altered: %q", rejoined)
	}
}
