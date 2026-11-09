// Package inventory answers "which sessions exist, and what state are they and
// their repositories in?" for `saint ls`.
//
// Repository state is read with a git that never loads the repository's own
// configuration (git_isolated.go), because every mounted repository is
// writable from its guest.
package inventory

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// containerNamePrefix is the one place the session-ID-to-container-name
// mapping lives on the read side.
const containerNamePrefix = "saint-"

// ContainerName is the container that backs a session.
func ContainerName(sessionID string) string {
	return containerNamePrefix + sessionID
}

type Snapshot struct {
	Rows []Row

	// Warnings are things the user should act on: sessions dropped for an
	// unsafe ID, repositories that could not be read, a session whose agent
	// never started. They are sanitized for a terminal.
	Warnings []string
}

type Row struct {
	ID        string
	Name      string
	Container string
	Repos     string
	Runtime   string
	State     string
	LastRun   string

	// ReviewPaths are files in a session's repositories whose change deserves
	// a look before the host acts on them, such as git hooks or config the
	// guest could have planted.
	ReviewPaths []string

	// Orphan marks a container with saint's prefix and no session record: a
	// leftover from a crash, or a session file deleted by hand.
	Orphan bool
}

type Loader struct {
	Store     *store.Store
	Sandbox   sandbox.Sandbox
	Inspector *RepoInspector
}

func (l *Loader) Load(ctx context.Context) (Snapshot, error) {
	// Sessions whose ID would be unsafe to hand to the container CLI are
	// dropped at load time; say so, so a missing row is not a mystery.
	sessions, warnings, err := l.Store.LoadSessionsWithWarnings()
	if err != nil {
		return Snapshot{}, err
	}
	infos, err := l.Sandbox.List(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	var repoSet []sandbox.Repo
	for _, session := range sessions {
		repoSet = append(repoSet, session.Repos...)
	}
	repoStatuses, repoErrs := l.Inspector.Inspect(ctx, repoSet)
	for _, repoErr := range repoErrs {
		warnings = append(warnings, sanitizeInline(repoErr.Error()))
	}

	infoByName := make(map[string]sandbox.Info, len(infos))
	for _, info := range infos {
		infoByName[info.Name] = info
	}

	rows := make([]Row, 0, len(sessions))
	known := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		container := ContainerName(session.ID)
		known[container] = true
		info, ok := infoByName[container]
		state := "missing"
		if ok {
			state = stateLabel(info.State)
		}

		// A session that stopped before its agent started left a reason
		// behind. Without it the row says "stopped", which reads as an agent
		// that ran and finished rather than one that never began.
		if state != "running" {
			if reason := bootstrapError(l.Store.BootstrapErrorPath(session.ID)); reason != "" {
				warnings = append(warnings, sanitizeInline(session.Name)+": "+reason)
			}
		}

		statuses := make([]RepoStatus, 0, len(session.Repos))
		var review []string
		for _, repo := range session.Repos {
			status := repoStatuses[repo.Path]
			status.ReadOnly = repo.ReadOnly
			statuses = append(statuses, status)
			review = append(review, sanitizeMany(status.ReviewPaths)...)
		}

		runtime := string(session.Runtime)
		if runtime == "" && ok {
			runtime = string(info.Runtime)
		}

		rows = append(rows, Row{
			ID:          session.ID,
			Name:        sanitizeInline(session.Name),
			Container:   container,
			Repos:       repoSummary(session.Repos, statuses),
			Runtime:     sanitizeInline(runtime),
			State:       state,
			LastRun:     humanAge(session.LastRunAt),
			ReviewPaths: review,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	// Orphans go last: they are debris, not work, but they are rows so that
	// `saint rm` can be pointed at them.
	var orphans []Row
	for _, info := range infos {
		if !strings.HasPrefix(info.Name, containerNamePrefix) || known[info.Name] {
			continue
		}
		state := "orphan"
		if info.State == sandbox.StateRunning {
			state = "orphan (running)"
		}
		orphans = append(orphans, Row{
			ID:        strings.TrimPrefix(info.Name, containerNamePrefix),
			Name:      sanitizeInline(info.Name),
			Container: info.Name,
			Repos:     "-",
			Runtime:   sanitizeInline(string(info.Runtime)),
			State:     state,
			LastRun:   "-",
			Orphan:    true,
		})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })

	return Snapshot{Rows: append(rows, orphans...), Warnings: warnings}, nil
}

// FormatTable renders a snapshot for a terminal. A repository name ending in
// "!" has files to review, listed under the table.
func FormatTable(snapshot Snapshot) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCONTAINER\tREPOS\tRT\tSTATE\tLAST RUN")
	for _, row := range snapshot.Rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Name, row.Container, row.Repos, row.Runtime, row.State, row.LastRun)
	}
	_ = w.Flush()

	for _, row := range snapshot.Rows {
		if len(row.ReviewPaths) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s: review before trusting these files, which the guest can write:\n", row.Name)
		for _, path := range row.ReviewPaths {
			fmt.Fprintf(&b, "  %s\n", path)
		}
	}
	return b.String()
}

func repoSummary(repos []sandbox.Repo, statuses []RepoStatus) string {
	if len(repos) == 0 {
		return "-"
	}
	index := 0
	if active := SelectActiveRepo(statuses); active >= 0 {
		index = active
	}
	name := filepath.Base(repos[index].Path)
	if name == "." || name == string(filepath.Separator) {
		name = repos[index].Path
	}
	if len(statuses) > index && len(statuses[index].ReviewPaths) > 0 {
		name += "!"
	}
	summary := sanitizeInline(name)
	if len(repos) > 1 {
		summary += fmt.Sprintf(" +%d", len(repos)-1)
	}
	return summary
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

// bootstrapError reads the reason a session's init script gave up, if it
// left one. The file is in the guest-writable mount, so the read is capped,
// only the first line is taken, and it is sanitized. It is shown, never acted
// on.
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
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, sanitizeInline(value))
	}
	return out
}
