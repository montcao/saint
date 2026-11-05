package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"strings"
	"sync"
)

// Token accounting is observed, never requested.
//
// saint does not run the agent - it forwards for one - so the only place the
// host can learn what a session actually spent is the response passing through
// this proxy. The scanners below read that response as it streams to the guest
// and pull out the usage counters.
//
// Two invariants govern everything in this file:
//
//  1. Metering must never be able to break a request. Every failure path here
//     abandons measurement and lets the bytes through untouched.
//  2. Body content must never be retained. Prompts and completions pass through
//     these buffers; only integers and the model name may outlive the request.
//     Do not add a "log the body while debugging" branch.
const (
	// maxJSONMeterBytes caps how much of a non-streaming body is held for
	// parsing. Without a cap, a broken or hostile upstream could exhaust host
	// memory through a proxy that is otherwise only forwarding.
	maxJSONMeterBytes = 256 << 10

	// maxSSELineBytes caps a single server-sent-event line. An upstream that
	// never sends a newline must not grow this buffer without bound.
	maxSSELineBytes = 1 << 20
)

// Usage is the token accounting for one request.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int

	// Reported distinguishes "the upstream said zero" from "the upstream said
	// nothing". OpenAI omits usage from streamed responses unless the caller
	// asks for it, so a displayed 0 would often be a lie.
	Reported bool
}

// UsageEvent is one metered request.
//
// It carries the session ID and never the session token or the provider key -
// the same rule the request logging in ServeHTTP follows.
type UsageEvent struct {
	SessionID string
	Provider  string
	// Model is read from the response, not the request: the request body is the
	// whole conversation and this proxy does not touch it.
	Model string
	Usage Usage
	// Stream reports whether the response was server-sent events.
	Stream bool
	// Partial means the response ended before the point where usage is final -
	// a cancelled generation, or a detached session. The counts are still worth
	// having; they are just not the whole request.
	Partial bool

	// Endpoint is the upstream this request actually went to, as
	// "host/v1/path". It is what the request and partial counts are counts of,
	// and without it those numbers are totals with no stated subject.
	//
	// It is the request's path and the upstream's host, never a query string:
	// a path identifies a capability and a query can carry content.
	Endpoint string
}

// scanResult is what a scanner observed.
type scanResult struct {
	model string
	usage Usage
	// valid is false when the scanner could not trust what it read - malformed
	// JSON, or a body past the size cap. No event is emitted in that case.
	valid bool
	// complete is false when the response stopped before its terminal event.
	complete bool
}

// bodyScanner observes response bytes in arrival order.
//
// Implementations must not block, must not error, and must not retain body
// content beyond what a single measurement needs.
type bodyScanner interface {
	write(p []byte)
	result() scanResult
}

// newBodyScanner picks a scanner for a response content type, or nil when the
// response carries nothing worth metering.
func newBodyScanner(contentType string) bodyScanner {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType, _, _ = strings.Cut(contentType, ";")
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}
	switch mediaType {
	case "text/event-stream":
		return &sseScanner{}
	case "application/json":
		return &jsonScanner{}
	default:
		return nil
	}
}

// newDecoder returns a reader that turns an encoded body into the plaintext a
// scanner can read, or nil when the body is already plaintext.
//
// This exists because Go's transport decompresses only the responses whose
// Accept-Encoding it added itself. A client that asks for gzip explicitly -
// Claude Code does - gets the compressed stream handed through untouched, and a
// meter reading those bytes finds no frames at all: no model, no tokens, and
// every stream marked partial because message_stop never arrives.
//
// Observed in production before it was understood: 177 consecutive streamed
// responses from one real session, every one attributed and every one reporting
// nothing. The counts were not wrong, they were absent, which is harder to
// notice than a wrong number and is why it survived so long.
//
// The decoder is for the meter's copy only. The bytes the guest receives are
// never touched: saint does not decompress on the agent's behalf, because a
// response the proxy re-encoded is a response the proxy is responsible for.
func newDecoder(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		// The reader is created lazily on first read: a body that is never read
		// should not cost a gzip header parse, and a truncated stream must fail
		// where it is read rather than where it is set up.
		return &lazyGzipReader{src: r}
	case "", "identity":
		return nil
	default:
		// br, zstd, anything else: saint does not claim to read it. Reporting
		// nothing is correct here; reporting a guess would not be.
		return nil
	}
}

// lazyGzipReader defers opening the gzip stream until the first read.
type lazyGzipReader struct {
	src io.Reader
	zr  *gzip.Reader
	err error
}

func (l *lazyGzipReader) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.zr == nil {
		zr, err := gzip.NewReader(l.src)
		if err != nil {
			// A body that does not decompress is one this meter cannot read.
			// That is an absence of counters, never an error the guest sees.
			l.err = err
			return 0, err
		}
		l.zr = zr
	}
	return l.zr.Read(p)
}

// meteredBody wraps a response body and observes the bytes copied out of it.
//
// It is deliberately a pass-through: Read returns exactly what the underlying
// body returned, so the reverse proxy's flushing behaviour - and therefore SSE
// streaming - is unchanged.
type meteredBody struct {
	src  io.ReadCloser
	scan bodyScanner
	emit func(scanResult)
	once sync.Once

	// encoding is the body's Content-Encoding. When it is one saint can read,
	// the bytes are held until the body ends and decoded in one pass rather
	// than streamed through a decompressor.
	//
	// Streaming would need a pipe and a goroutine to pull on it, and a
	// goroutine that outlives the response it was reading is a leak in a
	// process that runs for days. The frames this needs are at both ends of the
	// stream anyway - input tokens in message_start, output tokens in
	// message_delta - so there is no partial answer to be had earlier.
	encoding string
	// held is the undecoded body, bounded. Past the cap nothing more is kept
	// and the response reports no usage, which is the honest outcome: saint
	// would rather say nothing than decode half a stream and report the half.
	held    []byte
	overrun bool
}

// maxEncodedMeterBytes bounds what is held for decoding. Generous against a
// long generation's compressed transcript, and a bound rather than a guess,
// because a response body has no size saint controls.
const maxEncodedMeterBytes = 8 << 20

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if n > 0 {
		b.observe(p[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

// observe takes the bytes on their way past, either scanning them directly or
// holding them for decoding.
func (b *meteredBody) observe(chunk []byte) {
	if b.encoding == "" {
		b.scan.write(chunk)
		return
	}
	if b.overrun {
		return
	}
	if len(b.held)+len(chunk) > maxEncodedMeterBytes {
		b.overrun = true
		b.held = nil
		return
	}
	b.held = append(b.held, chunk...)
}

func (b *meteredBody) Close() error {
	b.finish()
	return b.src.Close()
}

func (b *meteredBody) finish() {
	b.once.Do(func() {
		// A held body is decoded once, here, where the whole of it is
		// available. A decoder that fails leaves the scanner with nothing,
		// which reports as "not measured" - the correct answer for a body saint
		// could not read, and never an error the guest sees.
		if len(b.held) > 0 {
			if decoder := newDecoder(b.encoding, bytes.NewReader(b.held)); decoder != nil {
				if plain, err := io.ReadAll(io.LimitReader(decoder, maxEncodedMeterBytes)); err == nil {
					b.scan.write(plain)
				}
			}
			b.held = nil
		}

		res := b.scan.result()
		if !res.valid {
			return
		}
		b.emit(res)
	})
}

// rawUsage covers both providers' spellings. Every field is a pointer so an
// absent counter stays absent instead of collapsing to zero: Anthropic sends
// input tokens in message_start and output tokens in message_delta, and merging
// those must not let the later frame erase the earlier one.
type rawUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`

	// OpenAI chat completions spell the same two counters differently.
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
}

func (r *rawUsage) apply(u *Usage) {
	if r == nil {
		return
	}
	u.Reported = true
	if r.InputTokens != nil {
		u.InputTokens = *r.InputTokens
	}
	if r.PromptTokens != nil {
		u.InputTokens = *r.PromptTokens
	}
	if r.OutputTokens != nil {
		u.OutputTokens = *r.OutputTokens
	}
	if r.CompletionTokens != nil {
		u.OutputTokens = *r.CompletionTokens
	}
	if r.CacheCreationInputTokens != nil {
		u.CacheCreationTokens = *r.CacheCreationInputTokens
	}
	if r.CacheReadInputTokens != nil {
		u.CacheReadTokens = *r.CacheReadInputTokens
	}
}

// sseFrame is the subset of a streamed event this proxy reads. Anything not
// named here is ignored rather than buffered.
type sseFrame struct {
	Type    string    `json:"type"`
	Model   string    `json:"model"`
	Usage   *rawUsage `json:"usage"`
	Message struct {
		Model string    `json:"model"`
		Usage *rawUsage `json:"usage"`
	} `json:"message"`
}

// sseScanner assembles usage from a server-sent-event stream incrementally. It
// holds at most one line at a time, so a long generation costs nothing.
type sseScanner struct {
	line     []byte
	model    string
	usage    Usage
	stopped  bool
	disabled bool
}

func (s *sseScanner) write(p []byte) {
	if s.disabled {
		return
	}
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			if len(s.line)+len(p) > maxSSELineBytes {
				s.abandon()
				return
			}
			s.line = append(s.line, p...)
			return
		}
		if len(s.line)+idx > maxSSELineBytes {
			s.abandon()
			return
		}
		s.line = append(s.line, p[:idx]...)
		s.consume()
		s.line = s.line[:0]
		p = p[idx+1:]
	}
}

// abandon stops metering and drops the buffer. The response is unaffected.
func (s *sseScanner) abandon() {
	s.disabled = true
	s.line = nil
}

func (s *sseScanner) consume() {
	line := bytes.TrimSuffix(s.line, []byte("\r"))
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return
	}
	// OpenAI terminates a stream with a sentinel rather than a JSON event.
	if bytes.Equal(payload, []byte("[DONE]")) {
		s.stopped = true
		return
	}

	var frame sseFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		// One unparseable frame is not a reason to stop metering the rest.
		return
	}
	switch {
	case frame.Message.Model != "":
		s.model = frame.Message.Model
	case frame.Model != "" && s.model == "":
		s.model = frame.Model
	}
	frame.Message.Usage.apply(&s.usage)
	frame.Usage.apply(&s.usage)
	if frame.Type == "message_stop" {
		s.stopped = true
	}
}

func (s *sseScanner) result() scanResult {
	if s.disabled {
		return scanResult{}
	}
	return scanResult{
		model:    s.model,
		usage:    s.usage,
		valid:    true,
		complete: s.stopped,
	}
}

// jsonScanner accumulates a non-streaming body up to the cap and parses it once
// the response ends.
type jsonScanner struct {
	buf      []byte
	disabled bool
}

func (s *jsonScanner) write(p []byte) {
	if s.disabled {
		return
	}
	if len(s.buf)+len(p) > maxJSONMeterBytes {
		s.disabled = true
		s.buf = nil
		return
	}
	s.buf = append(s.buf, p...)
}

func (s *jsonScanner) result() scanResult {
	if s.disabled {
		return scanResult{}
	}
	var body struct {
		Model string    `json:"model"`
		Usage *rawUsage `json:"usage"`
	}
	// A truncated body fails to parse, which is exactly right: a response the
	// guest never fully received should not be reported as a measured one.
	if err := json.Unmarshal(s.buf, &body); err != nil {
		return scanResult{}
	}
	var usage Usage
	body.Usage.apply(&usage)
	return scanResult{
		model:    body.Model,
		usage:    usage,
		valid:    true,
		complete: true,
	}
}

// meterEncoding reports the Content-Encoding the meter will decode, or "" when
// the body is already plaintext or is in a form saint does not read.
//
// Anything unrecognised returns "", which makes the body scan as plaintext and
// find nothing. That is the honest failure: no counters rather than guessed
// ones.
func meterEncoding(header string) string {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "gzip", "x-gzip":
		return "gzip"
	default:
		return ""
	}
}
