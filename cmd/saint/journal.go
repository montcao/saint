package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/montcao/saint/internal/proxy"
)

// The proxy journal is the answer to "what did my agents call in the last hour,
// and which session did it". Everything crossing the credential boundary is
// already observed for metering; this writes that observation down in a form
// that survives the process and can be read back by `saint proxy log`.
//
// Three rules govern this file:
//
//  1. Nothing here may slow or break a request. The callbacks feeding it run on
//     the goroutine copying an agent's response, so recording is a non-blocking
//     send to a bounded buffer and one goroutine does the file I/O. A full
//     buffer drops the record and says so; it never waits.
//  2. No credential, session token, prompt or response body may reach the file.
//     Only the fields below are ever encoded, and a test asserts it.
//  3. The file is bounded. Its predecessor grew until the user noticed.
const (
	journalKindRequest = "req"
	// journalKindTunnel is forward-proxied traffic. It carries fewer fields on
	// purpose: a CONNECT has no path, no provider, and no status saint can
	// honestly report once the connection is hijacked.
	journalKindTunnel = "tunnel"
	journalKindUsage  = "usage"

	// journalKindDrop records that the buffer overflowed. A journal that
	// silently loses records is worse than one with a gap in it, because
	// nothing on screen says the gap is there.
	journalKindDrop = "drop"

	// journalBuffer is how many records may be waiting on the writer. It is
	// sized for a burst of concurrent agents rather than for a backlog: if the
	// disk is slow enough to fill this, the right outcome is a counted drop,
	// not a queue that grows behind the request path.
	journalBuffer = 256

	// journalCloseTimeout bounds shutdown. The writer only touches local files,
	// so this should never expire - but a quit path that can hang is the worst
	// failure this codebase ships, and that includes stopping the proxy.
	journalCloseTimeout = 2 * time.Second

	// journalMaxTextBytes caps any single value that came from a guest. A guest
	// chooses its own request paths, and one long enough to fill a rotation
	// would push every other record out of the journal.
	journalMaxTextBytes = 256
)

// journalUsage is the token accounting for one request. It exists as a pointer
// on journalRecord so that "the upstream reported nothing" is an absent object
// rather than four zeros, which would be a number saint did not measure.
type journalUsage struct {
	InputTokens         int `json:"in"`
	OutputTokens        int `json:"out"`
	CacheReadTokens     int `json:"cache_read"`
	CacheCreationTokens int `json:"cache_write"`
}

// journalRecord is one line of the journal.
//
// Every field is either measured by saint or copied from a header saint chose
// to read. There is deliberately no field for a body, a header set, or a
// credential of any kind.
type journalRecord struct {
	Time time.Time `json:"ts"`
	Kind string    `json:"kind"`

	// SessionID attributes the request. Empty means the caller presented no
	// token saint recognises - which is worth seeing, because it is something
	// other than one of this user's sessions talking to the proxy.
	SessionID string `json:"session,omitempty"`

	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`

	// Provider is empty when saint could not name the upstream from the path
	// alone. The proxy has a header-based fallback for that case; it is not
	// mirrored here, because a guessed provider in the journal would be a
	// confident wrong answer about which account paid.
	Provider string `json:"provider,omitempty"`

	// Auth is the session's recorded mode: whether the proxy injected the real
	// key or forwarded the guest's own subscription credential.
	Auth string `json:"auth,omitempty"`

	Status     int   `json:"status,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`

	Model   string `json:"model,omitempty"`
	Stream  bool   `json:"stream,omitempty"`
	Partial bool   `json:"partial,omitempty"`

	// Usage is nil when the upstream reported no counters.
	Usage *journalUsage `json:"usage,omitempty"`

	// Dropped is how many records were lost, on a journalKindDrop record.
	Dropped int `json:"dropped,omitempty"`
}

// journal appends records to a size-rotated file from a single goroutine.
type journal struct {
	path     string
	maxBytes int64
	keep     int

	records chan journalRecord
	quit    chan struct{}
	done    chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	dropped   atomic.Int64

	// beforeWrite runs on the writer goroutine before each line is encoded. It
	// is nil in production; tests install it before starting the writer, to hold
	// the buffer full and exercise the drop path deterministically.
	beforeWrite func()

	// file and size belong to the writer goroutine alone.
	file *os.File
	size int64
}

// newJournal starts a journal writing to path.
func newJournal(path string, maxBytes int64, keep int) *journal {
	j := newJournalWriter(path, maxBytes, keep)
	go j.writeLoop()
	return j
}

// newJournalWriter builds a journal without starting its writer goroutine, so a
// caller can install beforeWrite without racing it.
func newJournalWriter(path string, maxBytes int64, keep int) *journal {
	if maxBytes <= 0 {
		maxBytes = 1
	}
	if keep <= 0 {
		keep = 1
	}
	return &journal{
		path:     path,
		maxBytes: maxBytes,
		keep:     keep,
		records:  make(chan journalRecord, journalBuffer),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// record queues one record. It never blocks and never returns an error: the
// callers are the proxy's own callbacks, and an agent's request must not wait
// on, or fail because of, a log line.
func (j *journal) record(rec journalRecord) {
	if j == nil || j.closed.Load() {
		return
	}
	select {
	case j.records <- rec:
	default:
		j.dropped.Add(1)
	}
}

// Close stops the writer once the records already queued have been written.
func (j *journal) Close() {
	if j == nil {
		return
	}
	j.closeOnce.Do(func() {
		// Stop accepting before signalling, so the drain below terminates.
		j.closed.Store(true)
		close(j.quit)
	})
	select {
	case <-j.done:
	case <-time.After(journalCloseTimeout):
	}
}

func (j *journal) writeLoop() {
	defer close(j.done)
	for {
		select {
		case rec := <-j.records:
			j.write(rec)
		case <-j.quit:
			for {
				select {
				case rec := <-j.records:
					j.write(rec)
				default:
					j.flushDropped(time.Now())
					j.closeFile()
					return
				}
			}
		}
	}
}

// write emits one record, preceded by a note of anything dropped since the last
// one so the gap is visible at the point it happened.
func (j *journal) write(rec journalRecord) {
	j.flushDropped(rec.Time)
	j.writeLine(rec)
}

func (j *journal) flushDropped(at time.Time) {
	n := j.dropped.Swap(0)
	if n == 0 {
		return
	}
	j.writeLine(journalRecord{Time: at, Kind: journalKindDrop, Dropped: int(n)})
}

func (j *journal) writeLine(rec journalRecord) {
	if j.beforeWrite != nil {
		j.beforeWrite()
	}
	line, err := json.Marshal(rec.sanitized())
	if err != nil {
		return
	}
	line = append(line, '\n')

	if j.file == nil {
		if err := j.open(); err != nil {
			return
		}
	}
	// Rotate before writing rather than after, so the file never exceeds the
	// configured size. An empty file takes the line whatever its length: a
	// record larger than maxBytes has to land somewhere.
	if j.size > 0 && j.size+int64(len(line)) > j.maxBytes {
		if err := j.rotate(); err != nil {
			return
		}
	}
	n, err := j.file.Write(line)
	j.size += int64(n)
	if err != nil {
		// Drop the handle rather than looping on a broken one; the next record
		// reopens. A full disk must cost log lines, not the proxy.
		j.closeFile()
	}
}

func (j *journal) open() error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	j.file = file
	j.size = info.Size()
	j.pruneHistory()
	return nil
}

func (j *journal) closeFile() {
	if j.file == nil {
		return
	}
	_ = j.file.Close()
	j.file = nil
	j.size = 0
}

// rotatedPath names a retired file. .1 is the newest.
func (j *journal) rotatedPath(index int) string {
	return fmt.Sprintf("%s.%d", j.path, index)
}

// rotate retires the active file and discards whatever falls off the end.
func (j *journal) rotate() error {
	j.closeFile()

	history := j.keep - 1
	if history < 1 {
		// keep=1: the active file is the whole journal, so start it over rather
		// than let it grow past the bound.
		if err := removeIfPresent(j.path); err != nil {
			return err
		}
		return j.open()
	}

	if err := removeIfPresent(j.rotatedPath(history)); err != nil {
		return err
	}
	for i := history - 1; i >= 1; i-- {
		if err := renameIfPresent(j.rotatedPath(i), j.rotatedPath(i+1)); err != nil {
			return err
		}
	}
	if err := renameIfPresent(j.path, j.rotatedPath(1)); err != nil {
		return err
	}
	return j.open()
}

// pruneHistory removes files left behind by a larger keep_files setting. Without
// it, lowering the setting would leave the old files in place forever and the
// configured ceiling would be a fiction.
func (j *journal) pruneHistory() {
	for i := j.keep; ; i++ {
		path := j.rotatedPath(i)
		if _, err := os.Stat(path); err != nil {
			return
		}
		if err := removeIfPresent(path); err != nil {
			return
		}
	}
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func renameIfPresent(from, to string) error {
	if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// sanitized bounds and cleans the values that came from a guest.
func (r journalRecord) sanitized() journalRecord {
	r.SessionID = journalText(r.SessionID)
	r.Method = journalText(r.Method)
	r.Path = journalText(r.Path)
	r.Provider = journalText(r.Provider)
	r.Auth = journalText(r.Auth)
	r.Model = journalText(r.Model)
	return r
}

// journalText makes one untrusted value safe to store and later print.
//
// The guest chooses its request paths, and the agent chooses the model name in
// the response. Control characters in either would corrupt the terminal of
// whoever reads the journal back, and length has to be capped so one request
// cannot push a rotation's worth of records out of the file.
func journalText(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(cleaned) > journalMaxTextBytes {
		// Cut on a rune boundary so the result stays valid UTF-8.
		cut := journalMaxTextBytes
		for cut > 0 && !isUTF8Start(cleaned[cut]) {
			cut--
		}
		cleaned = cleaned[:cut] + "…"
	}
	return cleaned
}

func isUTF8Start(b byte) bool {
	return b&0xc0 != 0x80
}

// usageRecord turns a metered request into a journal record.
func usageRecord(ev proxy.UsageEvent, at time.Time) journalRecord {
	rec := journalRecord{
		Time:      at,
		Kind:      journalKindUsage,
		SessionID: ev.SessionID,
		Provider:  ev.Provider,
		Model:     ev.Model,
		Stream:    ev.Stream,
		Partial:   ev.Partial,
	}
	if ev.Usage.Reported {
		rec.Usage = &journalUsage{
			InputTokens:         ev.Usage.InputTokens,
			OutputTokens:        ev.Usage.OutputTokens,
			CacheReadTokens:     ev.Usage.CacheReadTokens,
			CacheCreationTokens: ev.Usage.CacheCreationTokens,
		}
	}
	return rec
}

// journalHandler records what crossed the proxy: the fields only the request
// itself carries - method, path, status, duration - which the usage callback
// never sees.
//
// It is installed only when proxy_log_requests is on, so a user who does not
// want request records pays nothing rather than paying for a disabled wrapper.
type journalHandler struct {
	next    http.Handler
	journal *journal
	tokens  *tokenRegistry
	now     func() time.Time
}

func newJournalHandler(next http.Handler, jr *journal, tokens *tokenRegistry) *journalHandler {
	return &journalHandler{next: next, journal: jr, tokens: tokens, now: time.Now}
}

func (h *journalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.now()
	recorder := &statusRecorder{ResponseWriter: w}

	// A forward-proxied request is a different shape, and almost every field
	// this journal records would be a lie about it: a CONNECT has no path, no
	// provider, and its status comes back as the recorder's 200 default because
	// the connection was hijacked - even when saint refused it. Its "duration"
	// would be the tunnel's whole lifetime rather than a request's.
	//
	// The proxy keeps the authoritative record of that traffic in
	// ForwardActivity, so the honest thing here is to say what is true and
	// nothing else.
	if isForwardShaped(r) {
		sessionID, auth := h.session(r.Header)
		h.next.ServeHTTP(recorder, r)
		h.journal.record(journalRecord{
			Time:      start,
			Kind:      journalKindTunnel,
			SessionID: sessionID,
			Method:    r.Method,
			// The authority, not a path: it is the only thing a CONNECT names.
			Path: forwardTarget(r),
			Auth: auth,
		})
		return
	}

	h.next.ServeHTTP(recorder, r)

	// Everything below runs after the response is finished, so none of it is on
	// the path the agent waits on. The inbound header set is still intact: the
	// proxy strips saint's header and the credential headers from a clone.
	sessionID, auth := h.session(r.Header)
	h.journal.record(journalRecord{
		Time:       start,
		Kind:       journalKindRequest,
		SessionID:  sessionID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Provider:   journalProvider(r.URL.Path),
		Auth:       auth,
		Status:     recorder.statusCode(),
		DurationMS: h.now().Sub(start).Milliseconds(),
	})
}

// isForwardShaped reports whether this is forward-proxy traffic rather than a
// model API call. It mirrors the proxy's own branch: a CONNECT, or a request
// line carrying an absolute URL.
func isForwardShaped(r *http.Request) bool {
	return r.Method == http.MethodConnect || (r.URL != nil && r.URL.IsAbs() && r.URL.Host != "")
}

// forwardTarget is the host a forward-proxied request names.
func forwardTarget(r *http.Request) string {
	if r.Method == http.MethodConnect {
		return r.Host
	}
	if r.URL != nil {
		return r.URL.Host
	}
	return ""
}

// session resolves the caller to one of this user's sessions.
//
// The proxy makes the same decision and does not hand the answer back through
// any callback, so it is made a second time here, against the same registry and
// the same constant-time comparison. The consequence of the two drifting is a
// request the journal cannot attribute - never one it attributes to the wrong
// session, because the token match is the same code either way.
func (h *journalHandler) session(header http.Header) (sessionID, auth string) {
	if h.tokens == nil {
		return "", ""
	}
	for _, candidate := range append(journalInboundTokens(header), journalProxyTokens(header)...) {
		if id, ok := h.tokens.Lookup(candidate); ok {
			return id, h.tokens.Auth(id)
		}
	}
	return "", ""
}

// journalProxyTokens reads the session token out of Proxy-Authorization.
//
// A CONNECT carries no other header a client will set: an HTTP proxy client
// turns HTTP_PROXY=http://saint:TOKEN@host into this and nothing else. Without
// it every tunnel journals as unattributed, which is how the two halves of this
// feature first fitted together.
func journalProxyTokens(header http.Header) []string {
	value := strings.TrimSpace(header.Get("Proxy-Authorization"))
	if value == "" {
		return nil
	}

	scheme, rest, found := strings.Cut(value, " ")
	if !found {
		return nil
	}
	rest = strings.TrimSpace(rest)

	switch strings.ToLower(scheme) {
	case "bearer":
		return []string{rest}
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil
		}
		// Either half may carry it, depending on how the client was pointed at
		// the proxy.
		user, pass, _ := strings.Cut(string(decoded), ":")
		return []string{pass, user}
	}
	return nil
}

// journalInboundTokens mirrors where the proxy looks for the session token, in
// the same order. It reads secrets and writes none of them.
func journalInboundTokens(header http.Header) []string {
	tokens := make([]string, 0, 3)
	if value := strings.TrimSpace(header.Get(proxy.SaintSessionHeader)); value != "" {
		tokens = append(tokens, value)
	}
	if value := strings.TrimSpace(header.Get("x-api-key")); value != "" {
		tokens = append(tokens, value)
	}
	if auth := strings.TrimSpace(header.Get("Authorization")); auth != "" {
		if rest, found := strings.CutPrefix(auth, "Bearer "); found {
			tokens = append(tokens, strings.TrimSpace(rest))
		} else {
			tokens = append(tokens, auth)
		}
	}
	return tokens
}

// journalProvider names the upstream a path is routed to, or "" when the path
// alone does not say.
//
// Only the proxy's fast-path prefixes are mirrored. Its header-based fallback
// exists so that a capability added to an agent keeps working rather than 404ing
// inside the guest; repeating that guess here would let the journal name an
// account that did not pay, so an unrecognised path is left unattributed.
func journalProvider(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"),
		strings.HasPrefix(path, "/v1/complete"),
		strings.HasPrefix(path, "/v1/models"):
		return "anthropic"
	case strings.HasPrefix(path, "/v1/chat/completions"),
		strings.HasPrefix(path, "/v1/responses"),
		strings.HasPrefix(path, "/v1/embeddings"):
		return "openai"
	}
	return ""
}

// statusRecorder remembers the status the guest was given.
//
// Unwrap is what keeps streaming intact: the reverse proxy flushes through an
// http.ResponseController, which walks this wrapper to reach the real writer, so
// SSE responses are not turned into buffered ones by being observed.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// statusCode reports what the guest saw. A handler that returned without
// writing anything produced a 200 with an empty body, which is what net/http
// sends.
func (s *statusRecorder) statusCode() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}
