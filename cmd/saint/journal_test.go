package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

// fixedJournalTime keeps every record in a test the same encoded length, which
// is what makes the rotation boundary assertions exact.
var fixedJournalTime = time.Date(2026, 8, 27, 15, 4, 5, 0, time.UTC)

func testRecord(path string) journalRecord {
	return journalRecord{
		Time:      fixedJournalTime,
		Kind:      journalKindRequest,
		SessionID: "abc123",
		Method:    "POST",
		Path:      path,
		Provider:  "anthropic",
		Auth:      "key",
		Status:    200,
	}
}

// encodedSize is the number of bytes one record occupies in the file.
func encodedSize(t *testing.T, rec journalRecord) int64 {
	t.Helper()
	line, err := json.Marshal(rec.sanitized())
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return int64(len(line) + 1)
}

// closeJournal closes j and waits for its writer to finish draining.
//
// Close itself gives up after journalCloseTimeout, because a quit path must not
// hang on a log file. That bound is right for saint and wrong for a test that
// reads the file next: on a loaded CI runner the writer can still be draining
// when Close returns, and the test then reads a journal that is about to be
// complete. Waiting on done makes the assertion about what the writer wrote,
// not about how fast the runner was.
func closeJournal(t *testing.T, j *journal) {
	t.Helper()
	j.Close()
	select {
	case <-j.done:
	case <-time.After(30 * time.Second):
		t.Fatal("journal writer did not finish draining after Close")
	}
}

func readAllRecords(t *testing.T, path string, keep int) []journalRecord {
	t.Helper()
	records, err := readJournal(path, keep, "", 0)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	return records
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// A file must never exceed the configured size, and it must not rotate one
// record early either: both ends of the boundary are asserted.
func TestJournalRotatesAtTheSizeBoundary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	size := encodedSize(t, testRecord("/v1/a"))

	j := newJournal(path, 2*size, 3)
	for _, p := range []string{"/v1/a", "/v1/b", "/v1/c", "/v1/d", "/v1/e"} {
		j.record(testRecord(p))
	}
	closeJournal(t, j)

	// Two records fit exactly; the third is what forces a rotation.
	if got := len(fileLines(t, path)); got != 1 {
		t.Errorf("active file holds %d records, want 1", got)
	}
	if got := len(fileLines(t, path+".1")); got != 2 {
		t.Errorf("proxy.jsonl.1 holds %d records, want 2", got)
	}
	if got := len(fileLines(t, path+".2")); got != 2 {
		t.Errorf("proxy.jsonl.2 holds %d records, want 2", got)
	}

	for _, file := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("stat %s: %v", file, err)
		}
		if info.Size() > 2*size {
			t.Errorf("%s is %d bytes, over the %d byte limit", file, info.Size(), 2*size)
		}
	}

	// Newest first, and nothing lost between the files.
	records := readAllRecords(t, path, 3)
	var paths []string
	for _, rec := range records {
		paths = append(paths, rec.Path)
	}
	want := []string{"/v1/e", "/v1/d", "/v1/c", "/v1/b", "/v1/a"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("records = %v, want %v", paths, want)
	}
}

// keep_files is the whole file count, so it is a real ceiling on disk use.
func TestJournalPrunesToKeepFiles(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	size := encodedSize(t, testRecord("/v1/a"))

	j := newJournal(path, size, 2)
	for i := 0; i < 8; i++ {
		j.record(testRecord("/v1/a"))
	}
	closeJournal(t, j)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected one rotated file: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("proxy.jsonl.2 exists, but keep_files is 2 (active plus one)")
	}
}

// keep_files=1 means the active file is the whole journal. It must start over
// rather than grow past the bound.
func TestJournalWithoutHistoryStartsOver(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	size := encodedSize(t, testRecord("/v1/a"))

	j := newJournal(path, size, 1)
	for _, p := range []string{"/v1/a", "/v1/b", "/v1/c"} {
		j.record(testRecord(p))
	}
	closeJournal(t, j)

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("proxy.jsonl.1 exists, but keep_files is 1")
	}
	lines := fileLines(t, path)
	if len(lines) != 1 || !strings.Contains(lines[0], "/v1/c") {
		t.Errorf("active file = %v, want only the newest record", lines)
	}
}

// Lowering keep_files must actually reclaim the space. Leaving the old files
// behind would make the configured ceiling a fiction.
func TestJournalPrunesFilesLeftByALargerKeepFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.jsonl")
	for _, suffix := range []string{".1", ".2", ".3", ".4"} {
		if err := os.WriteFile(path+suffix, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", suffix, err)
		}
	}

	j := newJournal(path, 1<<20, 2)
	j.record(testRecord("/v1/a"))
	closeJournal(t, j)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("proxy.jsonl.1 was pruned but is within keep_files: %v", err)
	}
	for _, suffix := range []string{".2", ".3", ".4"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("proxy.jsonl%s survived a lowered keep_files", suffix)
		}
	}
}

// The proxy's callbacks run on the goroutine copying an agent's response. When
// the buffer is full the record has to be dropped, and the drop has to be
// visible: a journal with a silent gap is worse than one that admits to it.
func TestJournalDropsRatherThanBlocks(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	j := newJournalWriter(path, 1<<20, 2)

	gate := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	j.beforeWrite = func() {
		once.Do(func() {
			close(blocked)
			<-gate
		})
	}
	go j.writeLoop()

	// Park the writer inside the first write so nothing drains the buffer.
	j.record(testRecord("/v1/a"))
	<-blocked

	const overflow = 32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < journalBuffer+overflow; i++ {
			j.record(testRecord("/v1/a"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("record blocked on a full buffer; it must drop instead")
	}

	close(gate)
	closeJournal(t, j)

	records := readAllRecords(t, path, 2)
	written, dropped := 0, 0
	for _, rec := range records {
		if rec.Kind == journalKindDrop {
			dropped += rec.Dropped
			continue
		}
		written++
	}
	if dropped != overflow {
		t.Errorf("journal reported %d drops, want %d", dropped, overflow)
	}
	if want := 1 + journalBuffer; written != want {
		t.Errorf("journal wrote %d records, want %d", written, want)
	}
}

// Different requests call the callbacks concurrently. Every record must be
// either written or counted as dropped; none may vanish. Run under -race.
func TestJournalConcurrentWriters(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	// Large enough that nothing rotates: rotation legitimately discards
	// records, which would make the accounting below meaningless.
	j := newJournal(path, 8<<20, 4)

	const writers, each = 8, 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < each; k++ {
				j.record(testRecord("/v1/a"))
			}
		}()
	}
	wg.Wait()
	closeJournal(t, j)

	written, dropped := 0, 0
	for _, rec := range readAllRecords(t, path, 4) {
		if rec.Kind == journalKindDrop {
			dropped += rec.Dropped
			continue
		}
		written++
	}
	if written+dropped != writers*each {
		t.Errorf("wrote %d and dropped %d, want %d accounted for", written, dropped, writers*each)
	}
}

// Recording after Close must be a no-op rather than a panic. The proxy closes the
// journal after the server, and an in-flight response can still fire a callback.
func TestJournalRecordAfterCloseIsSafe(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	j.record(testRecord("/v1/a"))
	closeJournal(t, j)
	j.record(testRecord("/v1/b"))
	closeJournal(t, j)

	if got := len(readAllRecords(t, path, 2)); got != 1 {
		t.Errorf("journal holds %d records, want the one written before Close", got)
	}
}

func newJournalTestRegistry(t *testing.T, dir string, session store.Session) (*tokenRegistry, string) {
	t.Helper()

	st := store.New(dir)
	if err := st.SaveSessions([]store.Session{session}); err != nil {
		t.Fatalf("save sessions: %v", err)
	}
	token, err := st.EnsureProxyToken(session.ID)
	if err != nil {
		t.Fatalf("ensure proxy token: %v", err)
	}
	return newTokenRegistry(st), token
}

func TestJournalHandlerRecordsWhatCrossedTheProxy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokens, token := newJournalTestRegistry(t, dir, store.Session{ID: "abc123", ProxyAuth: store.ProxyAuthSubscription})

	path := filepath.Join(dir, "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	handler := newJournalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}), j, tokens)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", token)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	closeJournal(t, j)

	records := readAllRecords(t, path, 2)
	if len(records) != 1 {
		t.Fatalf("journal holds %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Kind != journalKindRequest {
		t.Errorf("kind = %q, want %q", rec.Kind, journalKindRequest)
	}
	if rec.SessionID != "abc123" {
		t.Errorf("session = %q, want abc123", rec.SessionID)
	}
	if rec.Auth != store.ProxyAuthSubscription {
		t.Errorf("auth = %q, want %q", rec.Auth, store.ProxyAuthSubscription)
	}
	if rec.Method != http.MethodPost || rec.Path != "/v1/messages" {
		t.Errorf("method/path = %q %q, want POST /v1/messages", rec.Method, rec.Path)
	}
	if rec.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", rec.Provider)
	}
	if rec.Status != http.StatusAccepted {
		t.Errorf("status = %d, want %d", rec.Status, http.StatusAccepted)
	}
	if rec.DurationMS < 0 {
		t.Errorf("duration = %dms, want a measured value", rec.DurationMS)
	}
}

// Observing a response must not buffer it. Claude Code streams server-sent
// events for minutes at a time, and a wrapper that swallowed the flush would
// turn a live agent into a frozen one - the response would still be correct,
// which is what would make it hard to find.
func TestJournalHandlerDoesNotBufferAStream(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokens, token := newJournalTestRegistry(t, dir, store.Session{ID: "abc123"})

	j := newJournal(filepath.Join(dir, "proxy.jsonl"), 1<<20, 2)
	defer j.Close()

	proceed := make(chan struct{})
	handler := newJournalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))

		// Both routes to a flush must reach the real writer: the controller
		// walks Unwrap, and older code type-asserts http.Flusher.
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer is not an http.Flusher")
		}

		<-proceed
		_, _ = w.Write([]byte("data: second\n\n"))
	}), j, tokens)

	server := httptest.NewServer(handler)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// The handler is parked until this read returns, so a buffered response
	// deadlocks here rather than arriving late.
	first := make(chan string, 1)
	go func() {
		buf := make([]byte, len("data: first\n\n"))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			first <- ""
			return
		}
		first <- string(buf)
	}()

	select {
	case got := <-first:
		if got != "data: first\n\n" {
			t.Errorf("first chunk = %q, want it delivered before the handler continued", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("the first chunk never arrived; the wrapper buffered the stream")
	}
	close(proceed)
}

// A caller saint does not recognise still belongs in the journal - it is
// something other than this user's sessions reaching the proxy - but it must be
// recorded as unattributed rather than guessed at.
func TestJournalHandlerLeavesAnUnknownCallerUnattributed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokens, _ := newJournalTestRegistry(t, dir, store.Session{ID: "abc123"})

	path := filepath.Join(dir, "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	handler := newJournalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}), j, tokens)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "not-a-session-token")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	closeJournal(t, j)

	records := readAllRecords(t, path, 2)
	if len(records) != 1 {
		t.Fatalf("journal holds %d records, want 1", len(records))
	}
	if records[0].SessionID != "" {
		t.Errorf("session = %q, want it left empty", records[0].SessionID)
	}
	if records[0].Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", records[0].Status)
	}
}

// A path the proxy would route by header rather than by prefix must not be
// attributed to a provider here. An unknown upstream is an unknown upstream.
func TestJournalProvider(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/v1/messages":           "anthropic",
		"/v1/messages?beta=1":    "anthropic",
		"/v1/complete":           "anthropic",
		"/v1/models":             "anthropic",
		"/v1/chat/completions":   "openai",
		"/v1/responses":          "openai",
		"/v1/embeddings":         "openai",
		"/v1/organizations/me":   "",
		"/something/we/dontknow": "",
	}
	for path, want := range cases {
		if got := journalProvider(path); got != want {
			t.Errorf("journalProvider(%q) = %q, want %q", path, got, want)
		}
	}
}

// The journal is a file on disk that a human reads back. No credential, no
// session token and no part of a request or response body may reach it - the
// same bar usageLine is held to.
func TestJournalCarriesNoSecrets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokens, token := newJournalTestRegistry(t, dir, store.Session{ID: "abc123"})

	const (
		providerKey = "sk-ant-api03-not-a-real-key"
		prompt      = "the-user-prompt-must-not-be-logged"
		completion  = "the-model-completion-must-not-be-logged"
	)

	path := filepath.Join(dir, "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	handler := newJournalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(completion))
	}), j, tokens)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(prompt))
	req.Header.Set("x-api-key", token)
	req.Header.Set("Authorization", "Bearer "+providerKey)
	req.Header.Set(proxy.SaintSessionHeader, token)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	j.record(usageRecord(proxy.UsageEvent{
		SessionID: "abc123",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		Usage:     proxy.Usage{InputTokens: 12, Reported: true},
	}, fixedJournalTime))
	closeJournal(t, j)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	contents := string(data)
	for _, secret := range []string{token, providerKey, prompt, completion, "Bearer"} {
		if strings.Contains(contents, secret) {
			t.Errorf("journal contains %q", secret)
		}
	}
	for _, line := range fileLines(t, path) {
		var rec journalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record %q is not JSON: %v", line, err)
		}
		// auth holds a mode name - "key" or "subscription" - which says which
		// account paid, not what the credential was. Every other byte of the
		// record is held to usageLine's bar.
		rec.Auth = ""
		rest, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("re-encode record: %v", err)
		}
		for _, field := range []string{"token", "key", "authorization"} {
			if strings.Contains(strings.ToLower(string(rest)), field) {
				t.Errorf("record %s mentions %q; it must carry neither credentials nor their names", rest, field)
			}
		}
	}
}

// A guest picks its own request paths. Control characters would corrupt the
// terminal of whoever reads the journal back, and an unbounded path would push
// a rotation's worth of records out of the file.
func TestJournalSanitisesGuestSuppliedText(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	rec := testRecord("/v1/\x1b[2Jmessages\n" + strings.Repeat("x", 4096))
	j.record(rec)
	closeJournal(t, j)

	lines := fileLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("journal holds %d lines, want 1", len(lines))
	}
	records := readAllRecords(t, path, 2)
	got := records[0].Path
	if strings.ContainsAny(got, "\x1b\n\r") {
		t.Errorf("path %q still carries control characters", got)
	}
	if len(got) > journalMaxTextBytes+len("…") {
		t.Errorf("path is %d bytes, want it capped near %d", len(got), journalMaxTextBytes)
	}
}

// The rule the whole journal turns on: a request nobody metered must not look
// like a request that cost nothing.
func TestUsageRecordDistinguishesUnreportedFromZero(t *testing.T) {
	t.Parallel()

	unreported := usageRecord(proxy.UsageEvent{SessionID: "abc123", Provider: "openai", Stream: true}, fixedJournalTime)
	if unreported.Usage != nil {
		t.Errorf("usage = %+v, want nil for an upstream that reported nothing", unreported.Usage)
	}

	zero := usageRecord(proxy.UsageEvent{
		SessionID: "abc123",
		Provider:  "anthropic",
		Usage:     proxy.Usage{Reported: true},
	}, fixedJournalTime)
	if zero.Usage == nil {
		t.Fatal("usage = nil, want a measured zero")
	}
	if zero.Usage.InputTokens != 0 || zero.Usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zeros", zero.Usage)
	}
}

// Forward-proxied traffic is a different shape, and the request journal would
// otherwise report a path, a provider and a status for it that are all false -
// a CONNECT has no path, and its status comes back as the recorder's 200
// default because the connection was hijacked, even when saint refused it.
func TestJournalRecordsTunnelsWithoutInventingFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokens, token := newJournalTestRegistry(t, dir, store.Session{ID: "abc123"})

	path := filepath.Join(dir, "proxy.jsonl")
	j := newJournal(path, 1<<20, 2)
	handler := newJournalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), j, tokens)

	req := httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil)
	req.Host = "example.com:443"
	req.URL = &url.URL{Host: "example.com:443"}
	// A proxy client turns HTTP_PROXY=http://saint:TOKEN@host into this and
	// nothing else, so without reading it every tunnel is unattributed.
	req.Header.Set("Proxy-Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte("saint:"+token)))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	closeJournal(t, j)

	records := readAllRecords(t, path, 2)
	if len(records) != 1 {
		t.Fatalf("journal holds %d records, want 1", len(records))
	}

	got := records[0]
	if got.Kind != journalKindTunnel {
		t.Errorf("kind = %q, want %q", got.Kind, journalKindTunnel)
	}
	if got.SessionID != "abc123" {
		t.Errorf("session = %q, want the tunnel attributed to abc123", got.SessionID)
	}
	if got.Path != "example.com:443" {
		t.Errorf("path = %q, want the authority the CONNECT named", got.Path)
	}
	// The fields saint cannot honestly fill for a tunnel stay empty.
	if got.Provider != "" {
		t.Errorf("provider = %q, want empty: a CONNECT names no provider", got.Provider)
	}
	if got.Status != 0 {
		t.Errorf("status = %d, want 0: a hijacked connection has no status saint saw", got.Status)
	}
	if got.DurationMS != 0 {
		t.Errorf("duration = %dms, want 0: a tunnel lifetime is not a request duration", got.DurationMS)
	}
}
