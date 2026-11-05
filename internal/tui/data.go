package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

type Snapshot struct {
	Rows     []SessionRow
	Orphans  []sandbox.Info
	Warnings []string
	Counts   SnapshotCounts
}

type SnapshotCounts struct {
	Running int
	Stopped int
	Missing int
}

type RepoDetail struct {
	// Path is for display and filtering only, and is sanitized like every other
	// string here. Anything that has to reach the filesystem or the sandbox uses
	// the store's copy, which is left exactly as the user wrote it.
	Path        string
	Name        string
	Branch      string
	DirtyCount  int
	LastTouched string
	ReviewPaths []string
	ReadOnly    bool
	Missing     bool
	Active      bool

	// NoVCS marks a directory that is not a git repository. It is shown rather
	// than hidden: "clean" would be a claim saint cannot make about a directory
	// it has no history for.
	NoVCS bool
}

type SessionRow struct {
	ID      string
	Name    string
	Repos   string
	Runtime string
	State   string
	LastRun string

	// Activity is whether a request from this session is crossing the proxy
	// right now. See activityLabel for what the values do and do not claim.
	Activity string

	// BlockedReason names why the session appears to be waiting for a person,
	// and is empty when it does not appear to be.
	//
	// Unlike Activity this is inferred from what the agent has drawn rather than
	// measured from traffic crossing the proxy, so it is a separate field: a
	// caller that treated the two as interchangeable would have lost the
	// difference between a fact and a pattern match.
	BlockedReason string
	RepoDetails   []RepoDetail

	// Orphan marks a container that carries saint's prefix but has no session
	// record behind it - a leftover from a crash, or a session file deleted by
	// hand. It is shown as a row so it can be removed from the table like
	// anything else, rather than being a number in the header nobody can act on.
	Orphan bool

	// Container is the runtime name backing this row.
	Container string

	// Credential is the source this session's credential came from, and
	// CredentialExpiresAt is when it stops working. Zero means no known expiry.
	Credential          string
	CredentialExpiresAt time.Time

	// Auth is the session's recorded billing mode, "key" or "subscription". It
	// is per session rather than global because a session keeps the mode it was
	// created with even after the config changes.
	Auth string

	// Usage is what the proxy has metered for this session in the current saint
	// process. UsageKnown is false when the session has made no proxied request
	// yet, which is not the same as having spent nothing.
	Usage      usage.Session
	UsageKnown bool
}

type SnapshotLoader struct {
	Store     *store.Store
	Sandbox   sandbox.Sandbox
	Inspector *RepoInspector

	// Usage is optional. `saint ls` and most tests run without a proxy, and a
	// nil meter simply produces rows with no usage rather than failing.
	Usage *usage.Meter
}

// containerNamePrefix is the one place the session-ID-to-container-name mapping
// lives on the read side.
const containerNamePrefix = "saint-"

// ContainerName is the container that backs a session.
func ContainerName(sessionID string) string {
	return containerNamePrefix + sessionID
}

// AttachCmd builds the command that attaches to a session's container.
//
// The sandbox is reused from the loader rather than being stored a second time
// on the Model. Session IDs are validated in the store at load time and again
// in the sandbox layer before exec.
func (l *SnapshotLoader) AttachCmd(sessionID string) (*exec.Cmd, error) {
	if l.Sandbox == nil {
		return nil, errors.New("sandbox is not configured")
	}
	if !store.ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	return l.Sandbox.AttachCmd(ContainerName(sessionID), l.attachOptions(sessionID))
}

// attachOptions describe how to rebuild a session whose tmux server has died,
// so a container is never stranded with nothing to attach to.
func (l *SnapshotLoader) attachOptions(sessionID string) sandbox.AttachOptions {
	if l.Store == nil {
		return sandbox.AttachOptions{}
	}
	sessions, err := l.Store.LoadSessions()
	if err != nil {
		return sandbox.AttachOptions{}
	}
	for _, session := range sessions {
		if session.ID != sessionID || len(session.Repos) == 0 {
			continue
		}
		workdir := session.Repos[0].Path
		opts := sandbox.AttachOptions{Workdir: workdir}
		if command, err := sandbox.AgentCommand(session.Runtime, workdir); err == nil {
			opts.Command = command
		}
		return opts
	}
	return sandbox.AttachOptions{}
}

func (l *SnapshotLoader) Load(ctx context.Context) (Snapshot, error) {
	// Sessions whose ID would be unsafe to hand to the container CLI are dropped
	// at load time; surface that as a warning so a silently missing row is not a
	// mystery.
	sessions, sessionWarnings, err := l.Store.LoadSessionsWithWarnings()
	if err != nil {
		return Snapshot{}, err
	}
	infos, err := l.Sandbox.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	repoSet := make([]sandbox.Repo, 0)
	for _, session := range sessions {
		repoSet = append(repoSet, session.Repos...)
	}
	repoStatuses, repoErrs := l.Inspector.Inspect(ctx, repoSet)

	infoByName := make(map[string]sandbox.Info, len(infos))
	for _, info := range infos {
		infoByName[info.Name] = info
	}

	metered := l.Usage.Snapshot()

	rows := make([]SessionRow, 0, len(sessions))
	warnings := make([]string, 0, len(repoErrs)+len(sessionWarnings))
	counts := SnapshotCounts{}
	warnings = append(warnings, sessionWarnings...)
	for _, repoErr := range repoErrs {
		warnings = append(warnings, repoErr.Error())
	}

	for _, session := range sessions {
		info, ok := infoByName[ContainerName(session.ID)]
		state := "missing"
		if ok {
			switch info.State {
			case sandbox.StateRunning:
				state = "running"
			case sandbox.StateStopped:
				state = "stopped"
			default:
				state = "missing"
			}
		}

		// A session that stopped before its agent ever started left a reason
		// behind. Without this the row says "stopped" and nothing else, which
		// reads as an agent that ran and finished rather than one that never
		// began - the two look identical and want opposite responses.
		if state != "running" {
			if reason := bootstrapError(l.Store.BootstrapErrorPath(session.ID)); reason != "" {
				warnings = append(warnings, sanitizeInline(session.Name)+": "+reason)
			}
		}

		switch state {
		case "running":
			counts.Running++
		case "stopped":
			counts.Stopped++
		default:
			counts.Missing++
		}

		statuses := make([]RepoStatus, 0, len(session.Repos))
		for _, repo := range session.Repos {
			status := repoStatuses[repo.Path]
			status.ReadOnly = repo.ReadOnly
			statuses = append(statuses, status)
		}

		activeIndex := SelectActiveRepo(statuses)
		repoDetails := make([]RepoDetail, 0, len(session.Repos))
		for idx, repo := range session.Repos {
			status := statuses[idx]
			name := filepath.Base(repo.Path)
			if name == "." || name == string(filepath.Separator) {
				name = repo.Path
			}
			detail := RepoDetail{
				Path:        sanitizeInline(repo.Path),
				Name:        sanitizeInline(name),
				Branch:      sanitizeInline(status.Branch),
				DirtyCount:  status.DirtyCount,
				LastTouched: humanTime(status.LastTouched),
				ReviewPaths: sanitizeMany(status.ReviewPaths),
				ReadOnly:    repo.ReadOnly,
				Missing:     status.Missing,
				NoVCS:       status.NoVCS,
				Active:      idx == activeIndex,
			}
			if detail.Branch == "" {
				detail.Branch = "-"
			}
			repoDetails = append(repoDetails, detail)
		}

		runtimeLabel := string(session.Runtime)
		if runtimeLabel == "" && ok {
			runtimeLabel = string(info.Runtime)
		}

		// Reading the screen is inference, so it is only asked for when it could
		// change the answer: a session nothing is measuring, or one that is
		// measured and currently quiet. A request in flight is already the
		// stronger claim.
		blocked := ""
		if metered[session.ID].InFlight == 0 {
			blocked = sessionBlocked(state, l.Store.TranscriptPath(session.ID))
		}

		row := SessionRow{
			Auth:                session.ProxyAuthOrDefault(),
			Credential:          session.Credential,
			CredentialExpiresAt: session.CredentialExpiresAt,
			Container:           ContainerName(session.ID),
			ID:                  session.ID,
			Name:                sanitizeInline(session.Name),
			Repos:               repoSummary(session.Repos, statuses),
			Runtime:             sanitizeInline(runtimeLabel),
			State:               state,
			BlockedReason:       blocked,
			Activity:            blockedLabel(activityLabel(state, metered[session.ID].InFlight, l.Usage != nil), blocked),
			LastRun:             humanAge(session.LastRunAt),
			RepoDetails:         repoDetails,
		}
		row.Usage, row.UsageKnown = metered[session.ID]
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Name < rows[j].Name
	})

	var orphans []sandbox.Info
	sessionNames := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		sessionNames[ContainerName(session.ID)] = struct{}{}
	}
	for _, info := range infos {
		if !strings.HasPrefix(info.Name, containerNamePrefix) {
			continue
		}
		if _, ok := sessionNames[info.Name]; ok {
			continue
		}
		orphans = append(orphans, info)
	}
	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i].Name < orphans[j].Name
	})

	// Orphans go after the real sessions, which are already sorted. They are
	// rows so the user can select and remove them; they are last because they
	// are debris, not work.
	for _, info := range orphans {
		state := "orphan"
		if info.State == sandbox.StateRunning {
			state = "orphan (running)"
		}
		rows = append(rows, SessionRow{
			ID:        strings.TrimPrefix(info.Name, containerNamePrefix),
			Name:      sanitizeInline(info.Name),
			Runtime:   sanitizeInline(string(info.Runtime)),
			State:     state,
			Activity:  "-",
			LastRun:   "-",
			Orphan:    true,
			Container: info.Name,
		})
	}

	return Snapshot{Rows: rows, Orphans: orphans, Warnings: warnings, Counts: counts}, nil
}

func FormatPlainTable(snapshot Snapshot) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPOS\tRT\tSTATE\tACTIVITY\tLAST RUN")
	for _, row := range snapshot.Rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Name, row.Repos, row.Runtime, row.State, row.Activity, row.LastRun)
	}
	if len(snapshot.Orphans) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "ORPHAN CONTAINERS")
		for _, orphan := range snapshot.Orphans {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\t\n", orphan.Name, "-", orphan.Runtime, stateLabel(orphan.State))
		}
	}
	_ = w.Flush()
	return b.String()
}

func repoSummary(repos []sandbox.Repo, statuses []RepoStatus) string {
	if len(repos) == 0 {
		return "-"
	}
	active := SelectActiveRepo(statuses)
	index := 0
	marker := ""
	if active >= 0 {
		index = active
		marker = "▸"
	}
	name := filepath.Base(repos[index].Path)
	if name == "." || name == string(filepath.Separator) {
		name = repos[index].Path
	}
	if len(statuses) > index && len(statuses[index].ReviewPaths) > 0 {
		name += "!"
	}
	summary := marker + sanitizeInline(name)
	if len(repos) > 1 {
		summary += fmt.Sprintf(" +%d", len(repos)-1)
	}
	return strings.TrimSpace(summary)
}

func humanAge(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	delta := time.Since(ts)
	switch {
	case delta < time.Minute:
		return fmt.Sprintf("%ds ago", int(delta.Seconds()))
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(delta.Hours()/24))
	}
}

func humanTime(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	delta := time.Since(ts)
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta.Hours()))
	default:
		return ts.Format("Jan 2")
	}
}

// activityLabel says whether a request from this session is crossing the proxy
// right now.
//
// It is model traffic and nothing else. An agent compiling, running its tests,
// or sitting at its own prompt waiting for a human is doing none of it, and
// from outside the guest saint cannot tell those three apart - so all three are
// "idle" here rather than three states saint would be inventing. "waiting for
// prompt" in particular would need saint to read the agent's own screen, which
// is a guess dressed as a measurement.
//
// A stopped or missing container gets "-": idle is a claim about something that
// is running.
//
// watched is what keeps the column honest, and it is not a detail. The in-flight
// count is live state belonging to the proxy this process is running, so a
// caller with no meter - `saint ls`, which starts, lists and exits - would see
// zero for every session and print "idle" for all of them, every time. That is
// a confident wrong answer rather than a gap, so it prints "-" instead. The
// same applies after a restart: sessions saint did not watch start are unknown
// to it, not idle.
// blockedLabel folds the inferred blocked state into the activity column.
//
// Order is by evidence: a measured request in flight beats a screen that looks
// like a prompt, and both beat idle, which is only ever the absence of the
// other two.
func blockedLabel(activity, reason string) string {
	if activity == "working" || reason == "" {
		return activity
	}
	return "blocked"
}

func activityLabel(state string, inFlight int, watched bool) string {
	if !watched || state != stateLabel(sandbox.StateRunning) {
		return "-"
	}
	if inFlight > 0 {
		return "working"
	}
	return "idle"
}

// bootstrapError reads the reason a session's init script gave up, if it left
// one.
//
// The file is inside the guest-writable mount, so every guard here is about
// what happens when the contents are not saint's: the read is capped, only the
// first line is taken, and it is sanitised by the caller before display. It is
// shown, never acted on.
func bootstrapError(path string) string {
	const maxBytes = 4096

	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return sanitizeInline(line)
}

func stateLabel(state sandbox.SandboxState) string {
	switch state {
	case sandbox.StateRunning:
		return "running"
	case sandbox.StateStopped:
		return "stopped"
	default:
		return "missing"
	}
}

func sanitizeMany(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, sanitizeInline(value))
	}
	return out
}
