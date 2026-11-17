package store

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
)

// Several processes write the same state files: the detached proxy saves an
// "Allow always" answer, `saint allow` edits the same list, and a pane records
// itself on its session while another pane creates one. A plain load, change
// and save from two of them at once loses one change, so every
// read-modify-write goes through withLock, which holds an exclusive flock on
// one file in the state directory for the length of the update.

// withLock runs fn while holding the state directory's lock.
func (s *Store) withLock(fn func() error) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// UpdateConfig loads the config, applies fn and saves the result, holding the
// state lock throughout. It returns the saved config.
func (s *Store) UpdateConfig(fn func(*Config) error) (Config, error) {
	var saved Config
	err := s.withLock(func() error {
		cfg, err := s.EnsureConfig()
		if err != nil {
			return err
		}
		if err := fn(&cfg); err != nil {
			return err
		}
		if err := s.SaveConfig(cfg); err != nil {
			return err
		}
		saved = cfg
		return nil
	})
	return saved, err
}

// ErrSessionNotFound is returned by UpdateSession for an ID with no record.
var ErrSessionNotFound = errors.New("session not found")

// UpdateSession applies fn to one session's record under the state lock.
func (s *Store) UpdateSession(id string, fn func(*Session)) error {
	return s.withLock(func() error {
		sessions, err := s.LoadSessions()
		if err != nil {
			return err
		}
		for i := range sessions {
			if sessions[i].ID == id {
				fn(&sessions[i])
				return s.SaveSessions(sessions)
			}
		}
		return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	})
}

// NormalizeHostRule checks and canonicalises one allowlist entry: a hostname,
// or a wildcard written "*.example.com" or ".example.com" for a domain and its
// subdomains. Schemes, ports, paths and whitespace inside are refused rather
// than stripped, so a typo is an error and not a rule that matches nothing.
func NormalizeHostRule(raw string) (string, error) {
	rule := strings.ToLower(strings.TrimSpace(raw))
	rule = strings.TrimSuffix(rule, ".")
	host := strings.TrimPrefix(strings.TrimPrefix(rule, "*"), ".")
	if host == "" {
		return "", fmt.Errorf("empty host %q", raw)
	}
	if strings.ContainsAny(host, ":/@?#[] \t*") {
		return "", fmt.Errorf("%q is not a hostname: give the host alone, without a scheme, port or path", raw)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("%q is not a hostname", raw)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return "", fmt.Errorf("%q is not a hostname", raw)
			}
		}
	}
	if strings.HasPrefix(rule, "*.") || strings.HasPrefix(rule, ".") {
		// A wildcard over a top-level domain would allow most of the internet.
		if !strings.Contains(host, ".") {
			return "", fmt.Errorf("%q would allow a whole top-level domain", raw)
		}
		return "*." + host, nil
	}
	return host, nil
}

// AllowHost adds a host to the saved allowlist. It reports whether the host was
// new.
func (s *Store) AllowHost(raw string) (bool, error) {
	rule, err := NormalizeHostRule(raw)
	if err != nil {
		return false, err
	}
	added := false
	_, err = s.UpdateConfig(func(cfg *Config) error {
		if slices.Contains(cfg.ProxyAllowHosts, rule) {
			return nil
		}
		cfg.ProxyAllowHosts = append(cfg.ProxyAllowHosts, rule)
		slices.Sort(cfg.ProxyAllowHosts)
		added = true
		return nil
	})
	return added, err
}

// DisallowHost removes a host from the saved allowlist. It reports whether the
// host was there.
func (s *Store) DisallowHost(raw string) (bool, error) {
	rule, err := NormalizeHostRule(raw)
	if err != nil {
		return false, err
	}
	removed := false
	_, err = s.UpdateConfig(func(cfg *Config) error {
		kept := cfg.ProxyAllowHosts[:0]
		for _, existing := range cfg.ProxyAllowHosts {
			if existing == rule {
				removed = true
				continue
			}
			kept = append(kept, existing)
		}
		cfg.ProxyAllowHosts = kept
		return nil
	})
	return removed, err
}

// AddSession appends a session record under the state lock.
func (s *Store) AddSession(session Session) error {
	return s.withLock(func() error {
		sessions, err := s.LoadSessions()
		if err != nil {
			return err
		}
		return s.SaveSessions(append(sessions, session))
	})
}

// RemoveSession deletes a session record under the state lock and returns the
// record as it was. An ID with no record returns a zero Session and no error,
// so removal can be repeated after a partial failure.
func (s *Store) RemoveSession(id string) (Session, error) {
	var removed Session
	err := s.withLock(func() error {
		sessions, err := s.LoadSessions()
		if err != nil {
			return err
		}
		kept := sessions[:0]
		for _, session := range sessions {
			if session.ID == id {
				removed = session
				continue
			}
			kept = append(kept, session)
		}
		return s.SaveSessions(kept)
	})
	return removed, err
}
