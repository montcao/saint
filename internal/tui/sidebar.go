package tui

import (
	"fmt"
	"sort"
	"strings"

	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/lipgloss"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

const sidebarInner = sidebarWidth - 4

// clampWidth truncates every line of a rendered block to width.
//
// This is a safety net over the whole main column rather than a fix for one
// widget. A single over-wide line makes lipgloss pad every other line out to
// match it, which pushes the sidebar off screen and wraps the frame - and a
// wrapped frame in the alt screen reads as a corrupted one, not a narrow one.
//
// The help footer is the widget that actually needs it: bubbles stops
// truncating once a line is already within an ellipsis of the limit, so a
// narrow terminal gets the whole key list regardless of Width.
func clampWidth(block string, width int) string {
	if width <= 0 {
		return block
	}
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > width {
			lines[i] = truncateString(line, width)
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderSidebar(height int) string {
	// The sidebar reports; it never holds the keyboard. Its border is the
	// ambient rule, which is what separates it at a glance from the rail and the
	// dialogs, whose brighter border says the keys are going there.
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colRule)).
		Padding(0, 1).
		Width(sidebarWidth - 2)
	if height > 2 {
		box = box.Height(height - 2)
	}

	row, ok := m.currentRow()
	if !ok {
		return box.Render(sidebarHeading("session") + "\nnone selected")
	}

	var b strings.Builder
	b.WriteString(sidebarHeading("session"))
	b.WriteString("\n")
	// The name is the one strongest thing on this surface: everything below it
	// is an attribute of it, and there is nothing else here competing to be read
	// first.
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInkBright)).
		Render(truncateString(row.Name, sidebarInner)))
	b.WriteString("\n")
	b.WriteString(sidebarMuted(truncateString(strings.TrimSpace(row.Runtime+" · "+row.State), sidebarInner)))
	b.WriteString("\n")
	b.WriteString(sidebarMuted(truncateString(row.LastRun, sidebarInner)))
	b.WriteString("\n")
	b.WriteString(billingLabel(row.Auth))
	b.WriteString("\n")
	if line := credentialLifeLine(row, time.Now()); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.sidebarUsage(row))

	return box.Render(strings.TrimRight(b.String(), "\n"))
}

// sidebarUsage renders the context and token blocks for one session.
func (m Model) sidebarUsage(row SessionRow) string {
	var b strings.Builder

	b.WriteString(sidebarHeading("context"))
	b.WriteString("\n")
	switch {
	case !row.UsageKnown:
		// The proxy has seen nothing for this session. Rendering 0% here would
		// claim an empty context for an agent that may be mid-conversation.
		b.WriteString(sidebarMuted("no proxied requests yet"))
		b.WriteString("\n")
		return b.String()
	case !row.Usage.Reported:
		b.WriteString(sidebarMuted("provider sent no counters"))
		b.WriteString("\n")
	default:
		b.WriteString(contextBlock(row.Usage))
	}

	u := row.Usage
	b.WriteString("\n")
	b.WriteString(sidebarHeading("tokens"))
	b.WriteString("\n")
	b.WriteString(sidebarKV("in", formatTokens(u.InputTokens)))
	b.WriteString(sidebarKV("out", formatTokens(u.OutputTokens)))
	b.WriteString(sidebarKV("cache read", formatTokens(u.CacheReadTokens)))
	b.WriteString(sidebarKV("cache write", formatTokens(u.CacheWriteTokens)))
	b.WriteString(sidebarKV("requests", fmt.Sprintf("%d", u.Requests)))
	if u.Partial > 0 {
		b.WriteString(sidebarKV("partial", fmt.Sprintf("%d", u.Partial)))
	}
	b.WriteString(renderEndpoints(u.Endpoints))

	// A cross-session total that just restates the selected session is noise.
	totals := m.usageTotals()
	if totals.Requests > 0 && m.meteredSessions() > 1 {
		b.WriteString("\n")
		b.WriteString(sidebarHeading("all sessions"))
		b.WriteString("\n")
		b.WriteString(sidebarKV("in", formatTokens(totals.InputTokens)))
		b.WriteString(sidebarKV("out", formatTokens(totals.OutputTokens)))
		b.WriteString(sidebarKV("requests", fmt.Sprintf("%d", totals.Requests)))
	}

	return b.String()
}

// contextBlock renders the occupancy bar, or the raw number when the window is
// not known well enough to divide by.
func contextBlock(u usage.Session) string {
	limit, known := usage.ContextLimit(u.Model)
	if !known || limit <= 0 || u.Context > limit {
		// Over the table's limit means the session is using a larger window
		// than this model's default - a beta header, most likely. A bar pinned
		// at 100% would be a lie; the raw count is not.
		return sidebarMuted(fmt.Sprintf("%s (window unknown)", formatTokens(u.Context))) + "\n"
	}

	fraction := float64(u.Context) / float64(limit)
	var b strings.Builder
	b.WriteString(renderBar(fraction, sidebarInner-5))
	b.WriteString(" ")
	b.WriteString(fmt.Sprintf("%3.0f%%", fraction*100))
	b.WriteString("\n")
	b.WriteString(sidebarMuted(fmt.Sprintf("%s / %s", formatTokens(u.Context), formatTokens(limit))))
	b.WriteString("\n")
	return b.String()
}

// meteredSessions counts how many sessions have measured usage.
func (m Model) meteredSessions() int {
	n := 0
	for _, row := range m.rows {
		if row.UsageKnown && row.Usage.Reported {
			n++
		}
	}
	return n
}

// usageTotals sums every session saint knows about in this process.
func (m Model) usageTotals() usage.Session {
	var total usage.Session
	for _, row := range m.rows {
		if !row.UsageKnown || !row.Usage.Reported {
			continue
		}
		total.Requests += row.Usage.Requests
		total.InputTokens += row.Usage.InputTokens
		total.OutputTokens += row.Usage.OutputTokens
		total.CacheReadTokens += row.Usage.CacheReadTokens
		total.CacheWriteTokens += row.Usage.CacheWriteTokens
	}
	return total
}

// contextFullThreshold is where the bar turns amber.
//
// Late enough that it is not crying wolf, early enough to still be able to act:
// a session past this point is close to compaction, and that is a decision the
// user wants to make deliberately rather than discover.
const contextFullThreshold = 0.8

// renderBar draws a filled proportion of the context window.
func renderBar(fraction float64, width int) string {
	if width < 1 {
		return ""
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}

	// A solid fill, not a gradient: a gradient's colour varies along the bar's
	// length rather than with its value, so it cannot answer the only question
	// this bar exists for - am I about to run out of room?
	//
	// Grey below the threshold. A bar that is coloured at every value teaches
	// the reader that its colour means "this is a bar", and then the one reading
	// that genuinely needs a person - a window close enough to full that
	// compaction is about to happen rather than be chosen - arrives in a channel
	// they have already learned to skip. The percentage beside it carries the
	// value either way.
	fill := barCalmFill
	if fraction >= contextFullThreshold {
		fill = barWarnFill
	}

	bar := progress.New(
		progress.WithSolidFill(fill),
		progress.WithoutPercentage(),
		progress.WithWidth(width),
		progress.WithFillCharacters('█', '░'),
	)
	bar.EmptyColor = barTrack

	// ViewAs rounds down, so a session with real context but less than one
	// cell's worth would render as an empty bar. Show the first cell instead:
	// "a little" and "none" are different answers.
	if fraction > 0 && int(fraction*float64(width)) == 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(fill)).Render("█") +
			lipgloss.NewStyle().Foreground(lipgloss.Color(barTrack)).Render(strings.Repeat("░", width-1))
	}
	return bar.ViewAs(fraction)
}

// credentialLifeLine warns before a session's credential lapses.
//
// saint reads the host's login at creation, so a session carries whatever was
// left rather than a full lifetime, and it cannot be refreshed from outside the
// container. Silence until the moment it fails would leave the user with an
// authentication error and no way to connect it to a cause.
func credentialLifeLine(row SessionRow, now time.Time) string {
	if row.CredentialExpiresAt.IsZero() {
		return ""
	}

	// "session credential", not "credential", and the difference is the whole
	// point of the line.
	//
	// A session's credential is a copy taken when its container was created,
	// not a reference to the host's. The host's Claude Code login refreshes
	// every few hours; the copy inside a running guest never does, because a
	// process environment cannot be changed after the process exists. So the
	// rail can honestly say the login has hours left while this line honestly
	// says this session's token is finished. Both are true, and the reader has
	// no way to know that unless the two are named differently.
	attention := lipgloss.NewStyle().Foreground(lipgloss.Color(colAttention))

	left := row.CredentialExpiresAt.Sub(now)
	if left <= 0 {
		return attention.Render("session credential expired · c renews it")
	}
	// Plenty of life left is a fact about the session, not a request, so it sits
	// on the ramp with the rest of the panel's ambient lines.
	if left > credentialWarnWithin {
		return sidebarMuted("session credential " + formatLife(left) + " left")
	}
	return attention.Render("session credential " + formatLife(left) + " left · c renews it")
}

// credentialWarnWithin is when the remaining life stops being a note and starts
// being a warning.
const credentialWarnWithin = 30 * time.Minute

// billingLabel names which account a session is charging.
//
// It is on screen next to the token counts because those numbers mean different
// things in the two modes: plan limits in one, money in the other.
func billingLabel(auth string) string {
	// Neither reading needs a person: which account a session bills is a fact
	// about how it was created, and it cannot be changed from here. The api-key
	// reading used to be amber, one line above the credential-expiry warning
	// that is also amber, which is exactly the collision that made the colour
	// stop meaning anything - a session billing correctly and a credential about
	// to die looked the same from across the room.
	//
	// api key is the brighter step of the two because it is the reading with a
	// cost attached, and a user scanning the fleet for spend is scanning for it.
	if auth == store.ProxyAuthSubscription {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(colInkSoft)).Render("subscription")
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colInk)).Render("api key · billed")
}

// colFocusRule is the border of a surface that currently has the keyboard, and
// colRule is the border of one that only reports.
//
// That difference used to be carried by indigo, which meant the hue had to be
// kept out of every other meaning in the interface to stay legible. A step on
// the grey ramp says the same thing - brighter frame, the keys are in here -
// without spending a colour on it, and it leaves the one saturated colour free
// to mean "this needs a person" and nothing else.
const colFocusRule = colMuted

// selectionStyle is the one selection style in the interface.
//
// Every surface that can move a cursor uses it, so "where am I" is a single
// shape the eye learns once. It is a light grey ground rather than a coloured
// one because a selection is a statement about the cursor, not about whether
// anything is wrong - as indigo it was reliably the loudest thing on screen,
// which is a lot of emphasis for a fact the user created themselves by pressing
// a key.
func selectionStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(colOnDark)).
		Background(lipgloss.Color(colSelect))
}

func sidebarHeading(text string) string {
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(colMuted)).
		Render(strings.ToUpper(text))
}

func sidebarMuted(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted)).Render(text)
}

// sidebarKV puts the label left and the value hard right, so the numbers form a
// column the eye can scan without reading the labels.
func sidebarKV(label, value string) string {
	label = truncateString(label, sidebarInner-len(value)-1)
	gap := max(1, sidebarInner-lipgloss.Width(label)-lipgloss.Width(value))
	return sidebarMuted(label) + strings.Repeat(" ", gap) + value + "\n"
}

// endpointsShown caps the list. A session reaches a handful of upstreams and
// the panel has a fixed height; the count below says what was left out rather
// than the list quietly ending.
const endpointsShown = 5

// renderEndpoints names the upstreams a session actually reached.
//
// This is what the request and partial counts above are counts of. A bare "412
// requests" invites the reader to assume it means turns they took, and it does
// not: it counts every call the agent made, including the ones it makes on its
// own behalf. Naming the endpoints is the difference between a number and a
// number with a subject.
//
// It is also the only place saint says out loud where a session's traffic went.
// That is a narrow claim and worth keeping narrow: these are the upstreams that
// crossed the proxy, so anything a guest reached by another route is absent by
// construction rather than by omission.
//
// Each entry takes two lines. The address gets a line of its own because it is
// the whole point of the section and a 34-column panel cannot hold an address
// and a count side by side without cutting the address - and a cut address is
// the one thing here that would be useless. The scheme is not shown: saint
// proxies https and nothing else, so it would be the same eight characters on
// every row, spent out of the thirty-two an address has to fit in.
func renderEndpoints(endpoints map[string]usage.EndpointCount) string {
	if len(endpoints) == 0 {
		return ""
	}

	type entry struct {
		name  string
		count usage.EndpointCount
	}
	list := make([]entry, 0, len(endpoints))
	for name, count := range endpoints {
		list = append(list, entry{name: name, count: count})
	}
	// Busiest first, and by name where two are level so the order does not
	// shuffle between refreshes for no reason the reader can see.
	sort.Slice(list, func(i, j int) bool {
		if list[i].count.Requests != list[j].count.Requests {
			return list[i].count.Requests > list[j].count.Requests
		}
		return list[i].name < list[j].name
	})

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(sidebarHeading("reached"))
	b.WriteString("\n")

	shown := list
	if len(shown) > endpointsShown {
		shown = shown[:endpointsShown]
	}
	for _, item := range shown {
		// Wrapped, not truncated. An address that has been cut cannot be
		// checked against anything, which is the one use this list has.
		for _, part := range wrapAddress(item.name, sidebarInner) {
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colInk)).Render(part))
			b.WriteString("\n")
		}

		count := fmt.Sprintf("%d", item.count.Requests)
		if item.count.Partial > 0 {
			count += fmt.Sprintf(" · %d partial", item.count.Partial)
		}
		gap := max(1, sidebarInner-2-lipgloss.Width(count))
		b.WriteString(sidebarMuted("  " + strings.Repeat(" ", gap-2) + count))
		b.WriteString("\n")
	}
	if left := len(list) - len(shown); left > 0 {
		b.WriteString(sidebarMuted(fmt.Sprintf("  … %d more\n", left)))
	}
	return b.String()
}

// wrapAddress breaks an address into lines that fit the panel, continuations
// indented so a wrapped address still reads as one thing.
//
// It breaks on a path separator where there is one in range, because a break
// after a slash is a place a reader can rejoin the two halves; a break in the
// middle of a hostname is not.
func wrapAddress(address string, width int) []string {
	if width <= 2 {
		return []string{address}
	}

	var out []string
	rest := address
	for {
		limit := width
		indent := ""
		if len(out) > 0 {
			indent = "  "
			limit = width - 2
		}
		if len([]rune(rest)) <= limit {
			return append(out, indent+rest)
		}

		cut := limit
		if at := strings.LastIndex(string([]rune(rest)[:limit]), "/"); at > 0 {
			cut = len([]rune(string([]rune(rest)[:limit])[:at+1]))
		}
		out = append(out, indent+string([]rune(rest)[:cut]))
		rest = string([]rune(rest)[cut:])
	}
}
