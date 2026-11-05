package main

import (
	"testing"
	"time"

	"github.com/montcao/saint/internal/store"
)

func TestMatchToken(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{
		"abc123": "token-for-abc",
		"def456": "token-for-def",
	}

	tests := []struct {
		name    string
		token   string
		wantID  string
		wantHit bool
	}{
		{name: "exact match", token: "token-for-abc", wantID: "abc123", wantHit: true},
		{name: "other session", token: "token-for-def", wantID: "def456", wantHit: true},
		{name: "empty", token: ""},
		{name: "unknown", token: "token-for-xyz"},
		{name: "prefix", token: "token-for-ab"},
		{name: "suffix appended", token: "token-for-abcd"},
		{name: "session id is not a token", token: "abc123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotID, gotHit := matchToken(tokens, tc.token)
			if gotHit != tc.wantHit || gotID != tc.wantID {
				t.Fatalf("matchToken(%q) = (%q, %v), want (%q, %v)", tc.token, gotID, gotHit, tc.wantID, tc.wantHit)
			}
		})
	}
}

func TestMatchTokenEmptyRegistry(t *testing.T) {
	t.Parallel()

	if _, ok := matchToken(nil, "anything"); ok {
		t.Fatal("matchToken() matched against an empty registry")
	}
	if _, ok := matchToken(map[string]string{}, ""); ok {
		t.Fatal("matchToken() matched the empty token")
	}
}

func TestTokenRegistryLookup(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if err := st.SaveSessions([]store.Session{{ID: "abc123", Name: "one"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	token, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	now := time.Now()
	registry := &tokenRegistry{store: st, interval: tokenRefreshInterval, now: func() time.Time { return now }}

	id, ok := registry.Lookup(token)
	if !ok || id != "abc123" {
		t.Fatalf("Lookup() = (%q, %v), want (abc123, true)", id, ok)
	}
	if _, ok := registry.Lookup("bogus"); ok {
		t.Fatal("Lookup() accepted an unknown token")
	}
	if _, ok := registry.Lookup(""); ok {
		t.Fatal("Lookup() accepted the empty token")
	}
}

func TestTokenRegistryPicksUpNewSessionsWithoutRestart(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if err := st.SaveSessions([]store.Session{{ID: "abc123", Name: "one"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	if _, err := st.EnsureProxyToken("abc123"); err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	now := time.Now()
	registry := &tokenRegistry{store: st, interval: tokenRefreshInterval, now: func() time.Time { return now }}
	if _, ok := registry.Lookup("nothing-yet"); ok {
		t.Fatal("Lookup() accepted an unknown token")
	}

	// A session created after the proxy started.
	if err := st.SaveSessions([]store.Session{{ID: "abc123"}, {ID: "def456"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	fresh, err := st.EnsureProxyToken("def456")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	// Still inside the cache window: not visible yet.
	if _, ok := registry.Lookup(fresh); ok {
		t.Fatal("Lookup() saw the new token before the refresh interval elapsed")
	}

	now = now.Add(tokenRefreshInterval + time.Millisecond)
	id, ok := registry.Lookup(fresh)
	if !ok || id != "def456" {
		t.Fatalf("Lookup() = (%q, %v), want (def456, true) after the refresh interval", id, ok)
	}
}

// An ambiguous token cannot be attributed to a session. Serving it would put a
// request - and its cost - against the wrong session.
func TestMatchTokenRefusesDuplicates(t *testing.T) {
	t.Parallel()

	tokens := map[string]string{
		"aaa11111": "shared-token",
		"bbb22222": "shared-token",
		"ccc33333": "unique-token",
	}

	if _, ok := matchToken(tokens, "shared-token"); ok {
		t.Error("matchToken() resolved a token held by two sessions")
	}
	id, ok := matchToken(tokens, "unique-token")
	if !ok || id != "ccc33333" {
		t.Errorf("matchToken() = %q, %v; want ccc33333, true", id, ok)
	}
	if _, ok := matchToken(tokens, "no-such-token"); ok {
		t.Error("matchToken() resolved an unknown token")
	}
}
