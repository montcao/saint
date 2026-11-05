package proxy

import (
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The controls in this file guard a boundary wider than saint's own sessions.
//
// The proxy binds the container bridge gateway, so every container on that
// bridge can reach the port - not only the ones saint started. The per-session
// token is the whole of what stands between them and a real provider
// credential, and a token that leaks, or an agent that is compromised while
// holding a legitimate one, otherwise gets an unbounded write channel onto the
// user's account.
//
// Two rules hold everywhere below:
//
//  1. A zero value means "default" or "off", never "deny". Someone who upgrades
//     saint must not find their agents blocked by a control they never asked
//     for.
//  2. Enforcement fails closed and says why. Metering may fail open because a
//     lost count harms nobody, but a limit that quietly does not apply is not a
//     limit. What must never happen is a panic or a hang: a refused request
//     comes back immediately with a status the guest can act on.
const (
	// DefaultMaxRequestBytes caps a single request body.
	//
	// It has to clear the largest thing a real agent sends. Claude Code puts the
	// entire conversation in every turn, so the worst legitimate case is a full
	// context of file contents plus base64 images - megabytes, not tens of them.
	// 32 MiB sits well above that while still bounding what one guest can push
	// through the user's credential in a single call. Raise it with
	// Config.MaxRequestBytes if a real workload ever needs more; do not remove
	// the cap because one request was refused.
	DefaultMaxRequestBytes int64 = 32 << 20

	// UnlimitedRequestBytes turns the body cap off entirely.
	//
	// It is spelled as a negative value on purpose: zero already means "use the
	// default", and running with no cap at all has to be asked for explicitly
	// rather than fallen into.
	UnlimitedRequestBytes int64 = -1
)

const (
	// sweepInterval and idleBucketTTL govern how rate-limit state is reclaimed.
	//
	// The TTL is longer than the refill window on purpose: a bucket untouched
	// for two minutes has provably refilled to capacity, so discarding it hands
	// the session nothing it did not already have.
	sweepInterval = time.Minute
	idleBucketTTL = 2 * time.Minute
)

// Limit is one ceiling in force, shaped so a caller can render it honestly.
//
// Value alone is not enough: a limit that is switched off would render as the
// number 0, which reads as "nothing is allowed" - the opposite of the truth.
// Enforced is what a renderer must consult first.
type Limit struct {
	// Value is the ceiling in that limit's own unit. It is meaningful only when
	// Enforced is true, and is zero otherwise.
	Value int64

	// Enforced reports whether the limit applies at all.
	Enforced bool

	// Default reports that the value came from this package rather than from
	// the user's configuration.
	Default bool
}

// Settings is the proxy's live security posture as data.
//
// It is deliberately values only, with no formatting: what a terminal UI, a
// doctor check and a debug log should each say about the same posture are three
// different things, and none of them belong in the package that owns the
// boundary.
type Settings struct {
	// UpstreamHosts is the complete set of hosts this proxy will forward to,
	// sorted so a caller can print it without deciding an order.
	UpstreamHosts []string

	// MaxRequestBytes is the cap on a single request body.
	MaxRequestBytes Limit

	// RequestsPerMinute is the per-session ceiling on upstream calls.
	RequestsPerMinute Limit

	// Forward is the posture of the forward-proxy path, which is off unless
	// asked for. Its zero value is the honest report of a proxy that only
	// reverse-proxies.
	Forward ForwardSettings
}

// Settings reports the limits this proxy is actually applying, which is not the
// same as the ones it was configured with: a zero field resolves to a default,
// and the report has to show what the guest will meet.
func (p *Proxy) Settings() Settings {
	body := Limit{Default: p.cfg.MaxRequestBytes == 0}
	if resolved := p.maxRequestBytes(); resolved > 0 {
		body.Value = resolved
		body.Enforced = true
	}

	rate := Limit{Default: p.cfg.RequestsPerMinute <= 0}
	if p.cfg.RequestsPerMinute > 0 {
		rate.Value = int64(p.cfg.RequestsPerMinute)
		rate.Enforced = true
	}

	return Settings{
		UpstreamHosts:     slices.Sorted(maps.Keys(allowedUpstreamHosts)),
		MaxRequestBytes:   body,
		RequestsPerMinute: rate,
		Forward:           p.forwardSettings(),
	}
}

// maxRequestBytes resolves the configured body cap.
//
// Zero means the default and a negative value means no cap, so a Proxy whose
// Config never mentioned the field still gets the default rather than a cap of
// zero bytes.
func (p *Proxy) maxRequestBytes() int64 {
	if p.cfg.MaxRequestBytes == 0 {
		return DefaultMaxRequestBytes
	}
	return p.cfg.MaxRequestBytes
}

// enforceLimits applies the configured ceilings to an already-authenticated
// request.
//
// It reports whether the request may proceed; when it may not, the response has
// already been written. The returned body wrapper is what ErrorHandler consults
// to tell an oversized body apart from a genuine upstream failure, and is nil
// when no cap applies.
func (p *Proxy) enforceLimits(w http.ResponseWriter, r *http.Request, sessionID string) (*limitedBody, bool) {
	max := p.maxRequestBytes()

	// A declared length over the cap is refused before anything else happens.
	// Size is checked ahead of the rate ceiling because a body this proxy
	// refuses never reaches the upstream, so it should not spend a legitimate
	// session's request budget on the way to being rejected.
	if max > 0 && r.ContentLength > max {
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s refused %s %s: declared body %d bytes over the %d byte limit", sessionID, r.Method, r.URL.Path, r.ContentLength, max)
		}
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}

	if wait, ok := p.limiter.allow(sessionID); !ok {
		// Retry-After is the only part of this the guest can act on, so it is
		// sent as seconds - the form every HTTP client understands - and never
		// as zero, which would invite an immediate retry that fails again.
		retry := retryAfterSeconds(wait)
		w.Header().Set("Retry-After", retry)
		if p.cfg.Logf != nil {
			p.cfg.Logf("session=%s rate limited %s %s: over %d requests per minute, retry after %ss", sessionID, r.Method, r.URL.Path, p.cfg.RequestsPerMinute, retry)
		}
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return nil, false
	}

	if max <= 0 || r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	limited := &limitedBody{src: r.Body, remaining: max}
	r.Body = limited
	return limited, true
}

// errRequestTooLarge aborts a request whose body passed the cap in flight.
var errRequestTooLarge = errors.New("request body exceeds the configured limit")

// limitedBody stops a request body at the cap as it is copied upstream.
//
// The check cannot live in Content-Length alone: a chunked request declares no
// length at all, and a hostile one can declare a false one. So the bytes are
// counted as the reverse proxy streams them, and passing the cap fails the read
// - which aborts the upstream request rather than letting a truncated body
// arrive there and be answered as if it were whole.
//
// The verdict is recorded on the wrapper instead of being matched on the error
// coming back out, because the transport is free to wrap or replace what Read
// returned. It is read from the ServeHTTP goroutine while the transport's write
// loop is what set it, so it has to be atomic.
type limitedBody struct {
	src       io.ReadCloser
	remaining int64
	exceeded  atomic.Bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.exceeded.Load() {
		return 0, errRequestTooLarge
	}
	// Read at most one byte past what remains. That byte is what distinguishes a
	// body exactly at the cap, which is allowed, from one over it.
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.src.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		b.exceeded.Store(true)
		return 0, errRequestTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.src.Close() }

// tooLarge reports whether this body was the reason the upstream request
// failed. The nil case is a request that had no cap applied.
func (b *limitedBody) tooLarge() bool { return b != nil && b.exceeded.Load() }

// rateLimiter is a per-session token bucket.
//
// A bucket rather than a fixed window: a window boundary lets a session send
// twice its ceiling across two adjacent seconds, and then refuses a whole
// minute of traffic because a burst happened to land at :59. That second half
// is what makes people switch a control off. Capacity equals the per-minute
// ceiling, so a session that has been quiet can still spend a full minute's
// worth at once, which is the shape of a normal agent turn.
//
// Keyed by session ID, which is what TokenLookup returns, so two sessions can
// never spend each other's budget and a token shared across containers is
// counted once.
type rateLimiter struct {
	perMinute int

	// now is injectable so tests can exercise refill without sleeping.
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newRateLimiter(perMinute int, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		perMinute: perMinute,
		now:       now,
		buckets:   make(map[string]*bucket),
	}
}

// allow reports whether a session may make one more upstream call right now,
// and how long to wait when it may not.
//
// A nil limiter allows everything: that is the configured-off case, and it must
// stay the cheapest path through here because it is the default.
func (l *rateLimiter) allow(sessionID string) (retryAfter time.Duration, ok bool) {
	if l == nil || l.perMinute <= 0 {
		return 0, true
	}
	now := l.now()
	perSecond := float64(l.perMinute) / 60

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, found := l.buckets[sessionID]
	if !found {
		b = &bucket{tokens: float64(l.perMinute), seen: now}
		l.buckets[sessionID] = b
	} else {
		b.tokens = math.Min(float64(l.perMinute), b.tokens+now.Sub(b.seen).Seconds()*perSecond)
		b.seen = now
	}

	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / perSecond * float64(time.Second)), false
	}
	b.tokens--
	return 0, true
}

// sweep drops buckets for sessions that have gone quiet.
//
// Without it the map grows once per session ID ever seen, and session IDs come
// from a lookup this package does not own. Only buckets old enough to have
// refilled completely are dropped, so this can never hand anyone extra budget.
// Callers hold the mutex.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for id, b := range l.buckets {
		if now.Sub(b.seen) >= idleBucketTTL {
			delete(l.buckets, id)
		}
	}
}

// retryAfterSeconds renders a wait as whole seconds, rounded up and never
// below one. A Retry-After of 0 reads as "try again immediately", which is the
// one answer that is certainly wrong here.
func retryAfterSeconds(wait time.Duration) string {
	seconds := int64(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
