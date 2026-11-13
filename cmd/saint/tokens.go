package main

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/montcao/saint/internal/store"
)

// tokenRefreshInterval bounds how stale the proxy's view of the session tokens
// can be. A session created while the proxy is running authenticates within
// this window without a proxy restart.
const tokenRefreshInterval = 2 * time.Second

// tokenRegistry answers the proxy's authentication lookups from the per-session
// token files under ~/.saint/tokens, caching them briefly so the hot
// path does not hit the filesystem on every request.
type tokenRegistry struct {
	store    *store.Store
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	cache    map[string]string
	loadedAt time.Time

	authMu       sync.Mutex
	authCache    map[string]string
	netCache     map[string]string
	authLoadedAt time.Time

	// legacyNetwork is the shared network a session with no network of its
	// own runs on.
	legacyNetwork string
}

func newTokenRegistry(st *store.Store) *tokenRegistry {
	return &tokenRegistry{
		store:    st,
		interval: tokenRefreshInterval,
		now:      time.Now,
	}
}

// Lookup resolves an inbound token to the session that owns it.
func (t *tokenRegistry) Lookup(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	return matchToken(t.snapshot(), token)
}

// Auth reports a session's recorded auth mode, which decides whether the proxy
// injects a credential or forwards the guest's own.
//
// It reads the stored value rather than the current config: a running
// container's environment is fixed at create time, so recomputing the mode here
// would tell the proxy to strip a credential the guest is still sending.
func (t *tokenRegistry) Auth(sessionID string) string {
	if sessionID == "" {
		return store.ProxyAuthKey
	}
	modes, _ := t.authSnapshot()
	if mode, ok := modes[sessionID]; ok {
		return mode
	}
	// Unknown sessions get the mode that injects nothing extra into the guest.
	return store.ProxyAuthKey
}

// invalidate drops the cached tokens and session records, so a session created
// a moment ago authenticates on its first request.
func (t *tokenRegistry) invalidate() {
	t.mu.Lock()
	t.cache = nil
	t.mu.Unlock()
	t.authMu.Lock()
	t.authCache = nil
	t.netCache = nil
	t.authMu.Unlock()
}

// Network reports the container network a session runs on, which decides the
// one listener its token is accepted on. An unknown session has none, so it
// matches no listener.
func (t *tokenRegistry) Network(sessionID string) string {
	_, networks := t.authSnapshot()
	network, ok := networks[sessionID]
	if !ok {
		return ""
	}
	if network == "" {
		return t.legacyNetwork
	}
	return network
}

func (t *tokenRegistry) authSnapshot() (modes, networks map[string]string) {
	t.authMu.Lock()
	defer t.authMu.Unlock()

	now := t.now()
	if t.authCache != nil && now.Sub(t.authLoadedAt) < t.interval {
		return t.authCache, t.netCache
	}

	sessions, err := t.store.LoadSessions()
	if err != nil {
		if t.authCache != nil {
			return t.authCache, t.netCache
		}
		return map[string]string{}, map[string]string{}
	}
	modes = make(map[string]string, len(sessions))
	networks = make(map[string]string, len(sessions))
	for _, session := range sessions {
		modes[session.ID] = session.ProxyAuthOrDefault()
		networks[session.ID] = session.Network
	}
	t.authCache = modes
	t.netCache = networks
	t.authLoadedAt = now
	return modes, networks
}

func (t *tokenRegistry) snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if t.cache != nil && now.Sub(t.loadedAt) < t.interval {
		return t.cache
	}

	tokens, err := t.store.LoadProxyTokens()
	if err != nil {
		// Keep serving the last good set rather than locking every session out
		// because sessions.json was momentarily unreadable.
		if t.cache != nil {
			return t.cache
		}
		return map[string]string{}
	}
	t.cache = tokens
	t.loadedAt = now
	return tokens
}

// matchToken compares token against every candidate with a constant-time
// compare and deliberately does not break out of the loop on a hit.
//
// A plain map lookup keyed by the raw secret, or an early return on the first
// mismatch, would leak through timing how much of a guessed token was correct.
func matchToken(tokens map[string]string, token string) (string, bool) {
	matched := ""
	found := 0
	candidate := []byte(token)
	for sessionID, known := range tokens {
		if subtle.ConstantTimeCompare(candidate, []byte(known)) == 1 {
			matched = sessionID
			found++
		}
	}
	// Exactly one, or none. An ambiguous token cannot be attributed, and
	// attributing a request to the wrong session is worse than refusing it.
	// LoadProxyTokens already drops duplicates; this is the second lock.
	if found == 1 {
		return matched, true
	}
	return "", false
}
