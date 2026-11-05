package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadJournalIsMostRecentFirstAcrossRotations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	size := encodedSize(t, testRecord("/v1/a"))

	j := newJournal(path, 2*size, 3)
	for _, p := range []string{"/v1/a", "/v1/b", "/v1/c", "/v1/d"} {
		j.record(testRecord(p))
	}
	closeJournal(t, j)

	records, err := readJournal(path, 3, "", 3)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("readJournal returned %d records, want the 3 asked for", len(records))
	}
	want := []string{"/v1/d", "/v1/c", "/v1/b"}
	for i, rec := range records {
		if rec.Path != want[i] {
			t.Errorf("record %d = %q, want %q", i, rec.Path, want[i])
		}
	}
}

// Filtering to one session must not hide the fact that records were lost:
// an empty filtered view would otherwise read as "this session made no calls".
func TestReadJournalFiltersBySessionAndKeepsDrops(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)

	mine := testRecord("/v1/mine")
	theirs := testRecord("/v1/theirs")
	theirs.SessionID = "def456"

	j.record(mine)
	j.record(theirs)
	j.record(journalRecord{Time: fixedJournalTime, Kind: journalKindDrop, Dropped: 7})
	closeJournal(t, j)

	records, err := readJournal(path, 2, "abc123", 0)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("readJournal returned %d records, want the session's one plus the drop", len(records))
	}
	if records[0].Kind != journalKindDrop || records[0].Dropped != 7 {
		t.Errorf("first record = %+v, want the drop", records[0])
	}
	if records[1].Path != "/v1/mine" {
		t.Errorf("second record = %q, want /v1/mine", records[1].Path)
	}
}

// A crash leaves a half-written last line. It must cost that one record, not
// the whole journal.
func TestReadJournalSkipsATruncatedLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	j.record(testRecord("/v1/a"))
	closeJournal(t, j)

	appendLine(t, path, `{"ts":"2026-08-27T15:04:0`)

	records, err := readJournal(path, 2, "", 0)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if len(records) != 1 || records[0].Path != "/v1/a" {
		t.Fatalf("readJournal returned %+v, want the one intact record", records)
	}
}

func TestFormatJournalColumns(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 27, 15, 4, 5, 0, time.Local)
	records := []journalRecord{
		{
			Time: at, Kind: journalKindRequest, SessionID: "abc123",
			Method: "POST", Path: "/v1/messages", Provider: "anthropic",
			Auth: "key", Status: 200, DurationMS: 1420,
		},
		{
			Time: at, Kind: journalKindUsage, SessionID: "abc123",
			Provider: "anthropic", Model: "claude-opus-5", Stream: true,
			Usage: &journalUsage{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 900, CacheCreationTokens: 64},
		},
		{
			Time: at, Kind: journalKindUsage, SessionID: "abc123",
			Provider: "openai", Stream: true, Partial: true,
		},
		{Time: at, Kind: journalKindDrop, Dropped: 14},
	}

	lines := strings.Split(strings.TrimRight(formatJournal(records), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("formatJournal produced %d lines, want a header and 4 records", len(lines))
	}
	if !strings.HasPrefix(lines[0], "TIME") || !strings.Contains(lines[0], "RESULT") {
		t.Errorf("header = %q", lines[0])
	}

	wants := [][]string{
		{"2026-08-27 15:04:05", "abc123", "anthropic", "POST /v1/messages (key)", "200 1.42s"},
		{"abc123", "anthropic", "usage claude-opus-5 stream", "in=1200 out=340 cache_read=900 cache_write=64"},
		// An upstream that reported nothing is not a request that cost nothing,
		// and a model saint never saw prints as - rather than as a guess.
		{"openai", "usage - stream partial", "tokens=unreported"},
		{"dropped", "14 records (journal buffer full)"},
	}
	for i, cells := range wants {
		for _, cell := range cells {
			if !strings.Contains(lines[i+1], cell) {
				t.Errorf("line %d = %q, want it to contain %q", i+1, lines[i+1], cell)
			}
		}
	}
}

// A status the program never saw must not read as a success.
func TestFormatJournalNeverInventsAStatus(t *testing.T) {
	t.Parallel()

	line := formatJournal([]journalRecord{{
		Time: time.Date(2026, 8, 27, 15, 4, 5, 0, time.Local),
		Kind: journalKindRequest, Method: "POST", Path: "/v1/messages",
	}})
	if strings.Contains(line, "200") {
		t.Errorf("formatJournal invented a status: %q", line)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}

func TestWriteJournalJSONIsOneObjectPerLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	records := []journalRecord{testRecord("/v1/a"), testRecord("/v1/b")}
	if err := writeJournalJSON(&out, records); err != nil {
		t.Fatalf("writeJournalJSON: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		var rec journalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q is not one JSON object: %v", line, err)
		}
	}
}
