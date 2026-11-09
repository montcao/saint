package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// legacyProxyTokenFile is where per-session proxy tokens used to live: inside
// the session directory, which is mounted read-write into the guest at /saint.
//
// That was a mistake. The host treated a guest-writable file as the
// authoritative answer to "which session is this?", so a guest could rewrite the
// value the host would accept - including setting it to another session's token
// and taking over that session's identity in the logs and the metering.
//
// Tokens now live under Paths.TokensDir, which is never mounted. This constant
// remains only so existing installs can be migrated off it.
const legacyProxyTokenFile = "proxy-token"

// ProxyTokenPath is the host-authoritative location of a session's token. It is
// outside every mount, so only the host can write it.
func (s *Store) ProxyTokenPath(id string) string {
	return filepath.Join(s.paths.TokensDir, id)
}

// legacyProxyTokenPath is the guest-writable location tokens used to live in.
func (s *Store) legacyProxyTokenPath(id string) string {
	return filepath.Join(s.paths.SessionsDir, id, legacyProxyTokenFile)
}

// EnsureProxyToken returns the session's proxy credential, creating it on first
// call and returning the same value on every call after that.
//
// The token is what the guest presents to the local proxy in place of a real
// provider key: the proxy authenticates it, strips it, and injects the user's
// actual key. It lives at 0600 inside the session directory, which is mounted
// into that one guest at /saint.
func (s *Store) EnsureProxyToken(id string) (string, error) {
	if _, err := s.EnsureSessionDir(id); err != nil {
		return "", err
	}
	path := s.ProxyTokenPath(id)

	if err := s.migrateProxyToken(id); err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if token := strings.TrimSpace(string(data)); token != "" {
			// Repair permissions on a token written before this ran, or loosened since.
			if err := os.Chmod(path, 0o600); err != nil {
				return "", fmt.Errorf("chmod proxy token: %w", err)
			}
			return token, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read proxy token: %w", err)
	}

	token, err := newProxyToken()
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write proxy token: %w", err)
	}
	return token, nil
}

// migrateProxyToken moves a token out of the guest-writable session directory.
//
// The value is carried over rather than replaced so a container that is already
// running keeps working: its environment holds the old token and cannot be
// changed without recreating it. A carried-over value may have been tampered
// with by that guest, which is why LoadProxyTokens refuses to serve duplicates -
// the one thing tampering could achieve is colliding with another session.
func (s *Store) migrateProxyToken(id string) error {
	legacy := s.legacyProxyTokenPath(id)
	data, err := os.ReadFile(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy proxy token: %w", err)
	}

	current := s.ProxyTokenPath(id)
	if _, err := os.Stat(current); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat proxy token: %w", err)
		}
		if token := strings.TrimSpace(string(data)); token != "" {
			if err := writeAtomic(current, []byte(token+"\n"), 0o600); err != nil {
				return fmt.Errorf("migrate proxy token: %w", err)
			}
		}
	}

	// Remove the guest-writable copy either way. Leaving it behind leaves a
	// second, weaker source of the same secret lying inside a mount.
	if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove legacy proxy token: %w", err)
	}
	return nil
}

// MigrateProxyTokens moves every existing session's token out of the
// guest-writable session directory.
//
// EnsureProxyToken migrates on the way past, but it only runs when a session is
// created. Without this, sessions that already existed keep their token in the
// old location, where LoadProxyTokens no longer looks - so the proxy rejects
// them and their agents fail every request.
func (s *Store) MigrateProxyTokens() error {
	if err := s.Ensure(); err != nil {
		return err
	}
	sessions, err := s.LoadSessions()
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if !ValidSessionID(session.ID) {
			continue
		}
		if err := s.migrateProxyToken(session.ID); err != nil {
			return err
		}
	}
	return nil
}

// RemoveProxyToken deletes a session's credential.
//
// Tokens no longer live in the session directory, so deleting that directory no
// longer takes the token with it.
func (s *Store) RemoveProxyToken(id string) error {
	if !ValidSessionID(id) {
		return fmt.Errorf("invalid session id %s", quoteTruncated(id, 32))
	}
	if err := os.Remove(s.ProxyTokenPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove proxy token: %w", err)
	}
	return nil
}

// LoadProxyTokens maps session ID to proxy token for every session that has
// one. Sessions without a token file simply cannot authenticate.
func (s *Store) LoadProxyTokens() (map[string]string, error) {
	sessions, err := s.LoadSessions()
	if err != nil {
		return nil, err
	}
	tokens := make(map[string]string, len(sessions))
	seen := make(map[string]string, len(sessions))
	for _, session := range sessions {
		data, err := os.ReadFile(s.ProxyTokenPath(session.ID))
		if err != nil {
			continue
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			continue
		}
		// Two sessions sharing a token cannot happen when the host mints them,
		// so it means a token migrated from the old guest-writable location was
		// tampered with. Serve neither: a request attributed to the wrong
		// session is worse than a request that fails.
		if other, clash := seen[token]; clash {
			delete(tokens, other)
			continue
		}
		seen[token] = session.ID
		tokens[session.ID] = token
	}
	return tokens, nil
}

func newProxyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
