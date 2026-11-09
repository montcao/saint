// Package usage aggregates the token accounting the proxy observes.
//
// saint does not run the agents it manages, so these numbers come from watching
// their API traffic go past rather than from the agents themselves. That makes
// the meter authoritative about what crossed the proxy and about nothing else:
// an agent talking to a provider some other way is invisible here.
package usage

import (
	"sync"
	"time"

	"github.com/montcao/saint/internal/proxy"
)

// EndpointCount is the traffic one session sent to one upstream.
type EndpointCount struct {
	Requests int
	// Partial counts the requests to this endpoint that ended before the
	// provider finalised usage.
	Partial int
	// LastAt is when this endpoint was last reached.
	LastAt time.Time
}

// maxTrackedEndpoints bounds the table. An upstream that invented a new path
// per request would otherwise grow it without limit, and an unbounded map in a
// process that runs for days is a leak rather than a feature. Past the cap the
// totals still accumulate under the endpoints already known, so the numbers
// stay right and only the breakdown stops growing.
const maxTrackedEndpoints = 64

// Session is the running total for one session.
//
// Totals cover this saint process only. The proxy lives and dies with saint, so
// a relaunch starts the counters over - Context self-corrects on the next
// request, because the provider reports the true occupancy every time.
type Session struct {
	// Model is the last model the provider reported for this session.
	Model string
	// Provider is the last provider this session reached.
	Provider string

	Requests int

	// Endpoints is every upstream this session actually reached, with how many
	// requests went to each and how many of those ended early.
	//
	// It exists so the request and partial counts have a stated subject. A bare
	// "412 requests" invites the reader to assume it counts turns they took;
	// naming the endpoints shows it also counts an agent's background calls,
	// and shows which hosts saint saw the session talk to at all.
	Endpoints map[string]EndpointCount
	// Partial counts requests that ended before the provider finalised usage -
	// a cancelled generation, or a session detached mid-answer.
	Partial int

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	// Context is the prompt size of the most recent request: fresh input plus
	// both kinds of cached tokens. It is the session's current context window
	// occupancy, not a running total.
	Context int64

	// Reported is false until a provider has actually sent usage counters. It
	// keeps "nothing measured yet" distinguishable from "measured, and zero".
	Reported bool

	// InFlight counts requests for this session that are crossing the proxy at
	// this instant. It is the only honest answer saint has to "is this session
	// working": saint does not run the agents it manages, so a request going
	// past is the whole of what it can see.
	//
	// Zero here is a measurement, not a gap. A session with nothing outstanding
	// really has nothing outstanding, and that stays true whether or not a
	// provider has ever reported a token for it.
	//
	// What is not a measurement is this counter after a relaunch. It is live,
	// in-memory state: it starts at zero when saint starts, so every session
	// restored from disk reads 0 no matter what its agent is doing inside the
	// microVM. A caller rendering a session it did not watch start must call
	// that unknown rather than idle. Unlike the token totals, this one never
	// self-corrects on its own - a later request tells saint what is in flight
	// now, never what was in flight while it was not running.
	InFlight int

	LastAt time.Time
}

// Meter accumulates usage events by session.
//
// It is safe for concurrent use: the proxy calls Record from the goroutine
// copying a response, so several sessions can report at once while a reader
// reads a snapshot.
type Meter struct {
	mu sync.Mutex
	// sessions is keyed by session ID. Growth is bounded by the number of real
	// sessions: the proxy only ever emits an ID that its token lookup resolved.
	sessions map[string]Session

	// now is swappable so tests do not depend on the wall clock.
	now func() time.Time
}

func NewMeter() *Meter {
	return &Meter{sessions: make(map[string]Session), now: time.Now}
}

// Record folds one proxied request into its session's totals.
func (m *Meter) Record(ev proxy.UsageEvent) {
	if m == nil || ev.SessionID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.sessions[ev.SessionID]
	s.Requests++
	if ev.Partial {
		s.Partial++
	}
	s.noteEndpoint(ev, m.now())
	if ev.Provider != "" {
		s.Provider = ev.Provider
	}
	s.LastAt = m.now()

	// A request the provider did not account for still happened, so it counts
	// toward Requests - but it must not move the token totals or overwrite the
	// last known model with nothing.
	if !ev.Usage.Reported {
		m.sessions[ev.SessionID] = s
		return
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
	s.Reported = true
	s.InputTokens += int64(ev.Usage.InputTokens)
	s.OutputTokens += int64(ev.Usage.OutputTokens)
	s.CacheReadTokens += int64(ev.Usage.CacheReadTokens)
	s.CacheWriteTokens += int64(ev.Usage.CacheCreationTokens)
	s.Context = int64(ev.Usage.InputTokens) +
		int64(ev.Usage.CacheReadTokens) +
		int64(ev.Usage.CacheCreationTokens)

	m.sessions[ev.SessionID] = s
}

// Begin records that a request for this session is crossing the proxy, and
// returns the function that records its end.
//
// It creates the session's row if there is not one yet, so a session that is
// working before its first usage report is visible as working. Reported stays
// false there, which is what keeps "no tokens measured yet" distinct from
// "measured, and none" - Begin observes traffic, not tokens.
//
// The returned function is safe to call more than once: the second and later
// calls do nothing, and the counter is floored at zero regardless. Both guards
// exist for the same reason. The failure they prevent is a count that has
// drifted away from the number of live requests, and the version of that which
// hurts is a session pinned at "working" with nothing behind it - a wrong
// answer that no later request in this process ever corrects.
//
// See Session.InFlight for why a caller must not read this as idle across a
// restart.
func (m *Meter) Begin(sessionID string) (end func()) {
	if m == nil || sessionID == "" {
		return func() {}
	}

	m.mu.Lock()
	s := m.sessions[sessionID]
	s.InFlight++
	m.sessions[sessionID] = s
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()

			// Forget can land between the two halves, because a session can be
			// deleted while one of its requests is still in flight. Writing the
			// row back unconditionally would resurrect it as an empty one, and
			// the UI would show a deleted session sitting at zero rather than
			// showing nothing.
			s, ok := m.sessions[sessionID]
			if !ok {
				return
			}
			if s.InFlight > 0 {
				s.InFlight--
			}
			m.sessions[sessionID] = s
		})
	}
}

// Session returns the totals for one session.
func (m *Meter) Session(id string) (Session, bool) {
	if m == nil {
		return Session{}, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	return s, ok
}

// Snapshot copies the current totals. The caller owns the result, so a reader can
// render from it without holding the lock.
// noteEndpoint records which upstream a request went to.
func (s *Session) noteEndpoint(ev proxy.UsageEvent, now time.Time) {
	if ev.Endpoint == "" {
		return
	}
	if s.Endpoints == nil {
		s.Endpoints = map[string]EndpointCount{}
	}
	if _, known := s.Endpoints[ev.Endpoint]; !known && len(s.Endpoints) >= maxTrackedEndpoints {
		return
	}

	seen := s.Endpoints[ev.Endpoint]
	seen.Requests++
	if ev.Partial {
		seen.Partial++
	}
	seen.LastAt = now
	s.Endpoints[ev.Endpoint] = seen
}

func (m *Meter) Snapshot() map[string]Session {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]Session, len(m.sessions))
	for id, s := range m.sessions {
		// The endpoint table is a map, so copying the struct copies a reference
		// to it. A caller ranging over that while the proxy recorded another
		// request would be reading a map being written on another goroutine,
		// which is a race the -race build would find and a crash it would not.
		s.Endpoints = cloneEndpoints(s.Endpoints)
		out[id] = s
	}
	return out
}

// Forget drops a session's totals. Deleting a session should not leave its
// numbers behind to be attached to a future session that reuses the ID.
func (m *Meter) Forget(id string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, id)
}

// cloneEndpoints copies the endpoint table so a snapshot does not alias the
// meter's own.
func cloneEndpoints(src map[string]EndpointCount) map[string]EndpointCount {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]EndpointCount, len(src))
	for endpoint, count := range src {
		out[endpoint] = count
	}
	return out
}
