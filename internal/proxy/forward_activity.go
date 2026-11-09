package proxy

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// HostActivity is what one session did with one host, as seen on the
// forward-proxy path.
//
// Bytes are what crossed the tunnel, in each direction, including TLS framing.
// They are not payload sizes and there is no way for this proxy to know payload
// sizes without decrypting, which it does not do.
type HostActivity struct {
	// Host is the hostname the guest named, lowercased. It is not resolved and
	// not verified: it is what the guest asked for.
	Host string

	// Port is the port the guest named, with the scheme default filled in when
	// the request omitted it - 443 for CONNECT, 80 for plain HTTP.
	Port string

	// Tunnels counts CONNECT tunnels opened, Requests counts absolute-form
	// plain HTTP requests forwarded. They are separate because they see
	// different amounts: a tunnel's contents are opaque, a plain request's are
	// not, and collapsing them into one "connections" number would hide that.
	Tunnels  int64
	Requests int64

	// Denials counts attempts the host policy refused. A host with denials and
	// no tunnels was never connected to by saint.
	Denials int64

	BytesToHost   int64
	BytesFromHost int64

	FirstSeen time.Time
	LastSeen  time.Time
}

// SessionForwardActivity groups one session's hosts.
type SessionForwardActivity struct {
	// SessionID is empty when the traffic carried no session credential, which
	// only happens when ForwardConfig.AllowAnonymous is on. An empty ID means
	// "saint does not know who this was", never "no session".
	SessionID string

	// Hosts is sorted by host then port so a renderer never has to decide.
	Hosts []HostActivity
}

// ForwardActivity is the whole of what the forward-proxy path observed.
type ForwardActivity struct {
	// Sessions is sorted by session ID.
	Sessions []SessionForwardActivity

	// Tracked and Max describe the table itself.
	Tracked int
	Max     int

	// Dropped counts observations discarded because the table was full.
	//
	// At the cap, hosts already in the table keep counting and new
	// (session, host) pairs are not added. Dropped is how a reader tells a
	// complete table from a truncated one, and it must be shown wherever this
	// data is: a table that silently stopped recording is exactly the confident
	// wrong answer this codebase forbids.
	Dropped int64
}

// ForwardActivity reports the hosts this proxy was asked to reach on the
// forward-proxy path, per session.
//
// It covers only traffic that honoured HTTP_PROXY/HTTPS_PROXY and reached this
// proxy. Anything a session sent straight out of the microVM - a tool that
// ignores the variables, a static binary, raw sockets, non-HTTP protocols - is
// not here and cannot be. Render it as what came through saint, never as
// everywhere a session went.
//
// Safe to call at any time, including while tunnels are live; the byte counters
// of a live tunnel are as of the moment of the call.
func (p *Proxy) ForwardActivity() ForwardActivity {
	if p == nil || p.forward == nil {
		return ForwardActivity{}
	}
	return p.forward.table.snapshot()
}

// ForgetSession drops a session's rows from the activity table.
//
// The table is otherwise append-only within its cap, and session IDs outlive
// nothing here, so the package that owns session lifecycle calls this when a
// session ends. Doing so is the only way the table shrinks.
func (p *Proxy) ForgetSession(sessionID string) {
	if p == nil || p.forward == nil {
		return
	}
	p.forward.table.forget(sessionID)
}

// observation kinds, kept as a type so a caller cannot pass the wrong string.
type observation int

const (
	observeTunnel observation = iota
	observeRequest
	observeDenial
)

// forwardTable is the activity table.
//
// Both halves of its key come from outside: session IDs from a lookup this
// package does not own, hostnames from the guest. So it is capped, and the
// number of observations the cap cost is kept rather than discarded - a
// truncated table that looks complete would be worse than no table.
type forwardTable struct {
	max int

	mu       sync.Mutex
	sessions map[string]map[string]*hostEntry
	tracked  int
	dropped  int64
}

// hostEntry is one (session, host) row. Its own mutex covers every field: the
// byte counters are written by two tunnel goroutines per connection, by every
// concurrent tunnel to the same host, and read by ForwardActivity at any time.
type hostEntry struct {
	host string
	port string

	mu            sync.Mutex
	tunnels       int64
	requests      int64
	denials       int64
	bytesToHost   int64
	bytesFromHost int64
	firstSeen     time.Time
	lastSeen      time.Time
}

func (e *hostEntry) addToHost(n int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.bytesToHost += n
	e.mu.Unlock()
}

func (e *hostEntry) addFromHost(n int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.bytesFromHost += n
	e.mu.Unlock()
}

func newForwardTable(max int) *forwardTable {
	if max <= 0 {
		max = DefaultMaxTrackedHosts
	}
	return &forwardTable{max: max, sessions: make(map[string]map[string]*hostEntry)}
}

// observe records one event against a (session, host) row and returns the row,
// which is nil when the table was full. Every caller of the returned entry has
// to tolerate nil, because "the table was full" must not be able to break a
// tunnel that is otherwise fine.
func (t *forwardTable) observe(sessionID string, tgt target, kind observation, now time.Time) *hostEntry {
	key := tgt.addr()

	t.mu.Lock()
	hosts, ok := t.sessions[sessionID]
	if !ok {
		hosts = make(map[string]*hostEntry)
		t.sessions[sessionID] = hosts
	}
	entry, ok := hosts[key]
	if !ok {
		if t.tracked >= t.max {
			t.dropped++
			t.mu.Unlock()
			return nil
		}
		entry = &hostEntry{host: tgt.host, port: tgt.port, firstSeen: now}
		hosts[key] = entry
		t.tracked++
	}
	t.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	switch kind {
	case observeTunnel:
		entry.tunnels++
	case observeRequest:
		entry.requests++
	case observeDenial:
		entry.denials++
	}
	entry.lastSeen = now
	return entry
}

// touch marks a row as active now. It is what a tunnel calls when it closes, so
// LastSeen means "last byte crossed" rather than "connection opened".
func (t *forwardTable) touch(entry *hostEntry, now time.Time) {
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.lastSeen = now
	entry.mu.Unlock()
}

func (t *forwardTable) forget(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tracked -= len(t.sessions[sessionID])
	delete(t.sessions, sessionID)
}

func (t *forwardTable) snapshot() ForwardActivity {
	t.mu.Lock()
	defer t.mu.Unlock()

	report := ForwardActivity{
		Sessions: make([]SessionForwardActivity, 0, len(t.sessions)),
		Tracked:  t.tracked,
		Max:      t.max,
		Dropped:  t.dropped,
	}
	for sessionID, hosts := range t.sessions {
		row := SessionForwardActivity{SessionID: sessionID, Hosts: make([]HostActivity, 0, len(hosts))}
		for _, entry := range hosts {
			entry.mu.Lock()
			row.Hosts = append(row.Hosts, HostActivity{
				Host:          entry.host,
				Port:          entry.port,
				Tunnels:       entry.tunnels,
				Requests:      entry.requests,
				Denials:       entry.denials,
				BytesToHost:   entry.bytesToHost,
				BytesFromHost: entry.bytesFromHost,
				FirstSeen:     entry.firstSeen,
				LastSeen:      entry.lastSeen,
			})
			entry.mu.Unlock()
		}
		slices.SortFunc(row.Hosts, func(a, b HostActivity) int {
			if by := cmp.Compare(a.Host, b.Host); by != 0 {
				return by
			}
			return cmp.Compare(a.Port, b.Port)
		})
		report.Sessions = append(report.Sessions, row)
	}
	slices.SortFunc(report.Sessions, func(a, b SessionForwardActivity) int {
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return report
}
