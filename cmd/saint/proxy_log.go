package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/montcao/saint/internal/store"
)

// defaultJournalLimit is how many records `saint proxy log` shows without -n.
// It is a screenful and a bit: the question this command answers is "what just
// happened", and an unbounded dump scrolls the answer off the top.
const defaultJournalLimit = 50

func runProxyLog(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("proxy log", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "only records for this session id")
	jsonFlag := fs.Bool("json", false, "one JSON object per line, most recent first")
	limitFlag := fs.Int("n", defaultJournalLimit, "how many records to show; 0 for all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := st.EnsureConfig()
	if err != nil {
		return err
	}
	paths := st.Paths()

	records, err := readJournal(paths.ProxyLogPath, cfg.ProxyLogKeepFilesOrDefault(), *sessionFlag, *limitFlag)
	if err != nil {
		return err
	}

	if *jsonFlag {
		return writeJournalJSON(os.Stdout, records)
	}

	if len(records) == 0 {
		fmt.Printf("no records in %s\n", paths.ProxyLogPath)
		if !cfg.ProxyLogRequestsOrDefault() {
			fmt.Println("request records are off: set \"proxy_log_requests\": true in " + paths.ConfigPath)
		}
		return nil
	}
	fmt.Print(formatJournal(records))
	return nil
}

// readJournal returns records most recent first, walking the active file and
// then the rotated ones until it has enough.
//
// Rotated files are read whole. Each is bounded by proxy_log_max_bytes, which is
// what makes that safe.
func readJournal(path string, keep int, sessionID string, limit int) ([]journalRecord, error) {
	var collected []journalRecord
	for i := 0; i < keep; i++ {
		file := path
		if i > 0 {
			file = fmt.Sprintf("%s.%d", path, i)
		}
		records, err := readJournalFile(file, sessionID)
		if err != nil {
			return nil, err
		}
		// Within a file records are oldest first; the caller wants the reverse.
		for j := len(records) - 1; j >= 0; j-- {
			collected = append(collected, records[j])
			if limit > 0 && len(collected) >= limit {
				return collected, nil
			}
		}
	}
	return collected, nil
}

func readJournalFile(path, sessionID string) ([]journalRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var records []journalRecord
	scanner := bufio.NewScanner(file)
	// A record is one line and its guest-supplied values are capped, so this is
	// generous rather than load-bearing.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec journalRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// A half-written last line survives a crash. Skip it rather than
			// refusing to show the rest of the journal.
			continue
		}
		if !journalMatches(rec, sessionID) {
			continue
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// journalMatches applies the session filter.
//
// Drop records pass every filter on purpose: they mean records are missing
// here, and hiding that from a filtered view would let a user read an empty
// result as "this session made no calls".
func journalMatches(rec journalRecord, sessionID string) bool {
	if sessionID == "" || rec.Kind == journalKindDrop {
		return true
	}
	return rec.SessionID == sessionID
}

func writeJournalJSON(w io.Writer, records []journalRecord) error {
	encoder := json.NewEncoder(w)
	for _, rec := range records {
		if err := encoder.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

// formatJournal renders the journal as columns, most recent first.
//
// Values the program did not measure print as "-". Nothing is inferred: a
// request with no provider is one whose path did not name an upstream, and a
// metered request with no counters prints tokens=unreported rather than zeros.
func formatJournal(records []journalRecord) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tSESSION\tPROVIDER\tWHAT\tRESULT")
	for _, rec := range records {
		what, result := journalSummary(rec)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			rec.Time.Local().Format("2006-01-02 15:04:05"),
			dashIfEmpty(rec.SessionID),
			dashIfEmpty(rec.Provider),
			what,
			result,
		)
	}
	_ = w.Flush()
	return b.String()
}

// journalSummary renders the two free-form columns of one record.
func journalSummary(rec journalRecord) (what, result string) {
	switch rec.Kind {
	case journalKindRequest:
		what = strings.TrimSpace(dashIfEmpty(rec.Method) + " " + dashIfEmpty(rec.Path))
		if rec.Auth != "" {
			what += " (" + rec.Auth + ")"
		}
		status := "-"
		if rec.Status != 0 {
			status = fmt.Sprintf("%d", rec.Status)
		}
		return what, status + " " + formatJournalDuration(rec.DurationMS)

	case journalKindUsage:
		shape := "single"
		if rec.Stream {
			shape = "stream"
		}
		if rec.Partial {
			shape += " partial"
		}
		what = "usage " + dashIfEmpty(rec.Model) + " " + shape
		if rec.Usage == nil {
			// The distinction the whole journal turns on: an upstream that
			// reported nothing is not a request that cost nothing.
			return what, "tokens=unreported"
		}
		return what, fmt.Sprintf("in=%d out=%d cache_read=%d cache_write=%d",
			rec.Usage.InputTokens, rec.Usage.OutputTokens,
			rec.Usage.CacheReadTokens, rec.Usage.CacheCreationTokens)

	case journalKindDrop:
		return "dropped", fmt.Sprintf("%d records (journal buffer full)", rec.Dropped)
	}
	return dashIfEmpty(rec.Kind), "-"
}

func formatJournalDuration(ms int64) string {
	if ms < 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
