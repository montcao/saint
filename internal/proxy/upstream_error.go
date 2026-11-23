package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxErrorBodyBytes = 16 << 10

// UpstreamErrorEvent contains only validated diagnostic fields from a
// non-success provider response. RateLimits contains documented Anthropic
// header names with their normalized numeric or timestamp values.
type UpstreamErrorEvent struct {
	SessionID  string
	Provider   string
	Status     int
	Type       string
	RetryAfter string
	RateLimits map[string]string
}

func (p *Proxy) observeUpstreamError(resp *http.Response, sessionID, provider string) {
	event := UpstreamErrorEvent{
		SessionID:  sessionID,
		Provider:   provider,
		Status:     resp.StatusCode,
		RetryAfter: retryAfter(diagnosticHeader(resp.Header, "Retry-After")),
	}
	if provider == "anthropic" {
		event.RateLimits = anthropicRateLimits(resp.Header)
	}
	encoding := resp.Header.Get("Content-Encoding")
	if len(encoding) > 64 {
		p.emitUpstreamError(event)
		return
	}
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	switch encoding {
	case "", "identity", "gzip", "x-gzip":
	default:
		p.emitUpstreamError(event)
		return
	}
	// Preserve the upgrade body's ReadWriteCloser interface. SSE does not
	// have a single JSON error envelope, so capture only its headers too.
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if resp.Body == nil || resp.StatusCode == http.StatusSwitchingProtocols ||
		resp.ContentLength > maxErrorBodyBytes || strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream") {
		p.emitUpstreamError(event)
		return
	}
	resp.Body = &upstreamErrorBody{
		ReadCloser: resp.Body,
		compressed: encoding == "gzip" || encoding == "x-gzip",
		emit: func(kind string) {
			event.Type = kind
			p.emitUpstreamError(event)
		},
	}
}

func (p *Proxy) emitUpstreamError(event UpstreamErrorEvent) {
	defer func() {
		if recover() != nil && p.cfg.Logf != nil {
			p.cfg.Logf("upstream error callback panicked")
		}
	}()
	p.cfg.OnUpstreamError(event)
}

func (p *Proxy) emitUpstreamStatus(sessionID, provider string, status int) {
	defer func() {
		if recover() != nil && p.cfg.Logf != nil {
			p.cfg.Logf("upstream status callback panicked")
		}
	}()
	p.cfg.OnUpstreamStatus(sessionID, provider, status)
}

// upstreamErrorBody observes the bytes that the reverse proxy already reads.
// Both the on-wire copy and the decompressed copy are bounded. The proxy
// serves its original body and headers and never waits for a look-ahead read.
type upstreamErrorBody struct {
	io.ReadCloser
	compressed bool
	overflow   bool
	body       []byte
	emit       func(string)
}

func (b *upstreamErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.emit == nil {
		return n, err
	}
	if !b.overflow {
		if n > maxErrorBodyBytes-len(b.body) {
			b.overflow = true
			b.body = nil
		} else {
			b.body = append(b.body, p[:n]...)
		}
	}
	if err != nil {
		b.finish(err == io.EOF)
	}
	return n, err
}

func (b *upstreamErrorBody) Close() error {
	b.finish(false)
	return b.ReadCloser.Close()
}

func (b *upstreamErrorBody) finish(complete bool) {
	if b.emit == nil {
		return
	}
	kind := ""
	if complete && !b.overflow {
		kind = parseUpstreamErrorType(b.body, b.compressed)
	}
	emit := b.emit
	b.body = nil
	b.emit = nil
	emit(kind)
}

func parseUpstreamErrorType(body []byte, compressed bool) string {
	if compressed {
		in, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return ""
		}
		defer in.Close()
		body, err = io.ReadAll(io.LimitReader(in, maxErrorBodyBytes+1))
		if err != nil || len(body) > maxErrorBodyBytes {
			return ""
		}
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	// Unknown strings could echo request content or credentials. Only
	// recognized provider error types cross the observation boundary.
	switch envelope.Error.Type {
	case "invalid_request_error", "authentication_error", "billing_error",
		"permission_error", "not_found_error", "conflict_error", "request_too_large",
		"rate_limit_error", "api_error", "timeout_error", "overloaded_error",
		"insufficient_quota", "server_error":
		return envelope.Error.Type
	default:
		return ""
	}
}

func diagnosticHeader(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) != 1 || len(values[0]) > 64 {
		return ""
	}
	return values[0]
}

func diagnosticCount(value string) string {
	if n, err := strconv.ParseUint(value, 10, 64); err == nil {
		return strconv.FormatUint(n, 10)
	}
	return ""
}

func retryAfter(value string) string {
	if n := diagnosticCount(value); n != "" {
		return n
	}
	if t, err := http.ParseTime(value); err == nil {
		return t.UTC().Format(http.TimeFormat)
	}
	return ""
}

func anthropicRateLimits(header http.Header) map[string]string {
	var observed map[string]string
	record := func(name, value string) {
		if value == "" {
			return
		}
		if observed == nil {
			observed = make(map[string]string)
		}
		observed[name] = value
	}
	for _, group := range []string{"requests", "tokens", "input-tokens", "output-tokens"} {
		for _, member := range []string{"limit", "remaining", "reset"} {
			name := "anthropic-ratelimit-" + group + "-" + member
			value := diagnosticHeader(header, name)
			if member == "reset" {
				t, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					continue
				}
				value = t.UTC().Format(time.RFC3339Nano)
			} else {
				value = diagnosticCount(value)
			}
			record(name, value)
		}
	}
	// Claude Code's subscription 429 handler also reads the unified status
	// and the five-hour and seven-day window limits.
	for _, window := range []string{"", "5h-", "7d-"} {
		prefix := "anthropic-ratelimit-unified-" + window
		status := diagnosticHeader(header, prefix+"status")
		switch status {
		case "allowed", "allowed_warning", "rejected":
			record(prefix+"status", status)
		}
		record(prefix+"reset", diagnosticCount(diagnosticHeader(header, prefix+"reset")))
		value, err := strconv.ParseFloat(diagnosticHeader(header, prefix+"utilization"), 64)
		if err == nil && value >= 0 && !math.IsInf(value, 0) {
			record(prefix+"utilization", strconv.FormatFloat(value, 'g', -1, 64))
		}
	}
	return observed
}
