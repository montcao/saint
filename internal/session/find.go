package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/montcao/saint/internal/store"
)

// Find resolves a session from what a person types: its ID, container name,
// an ID prefix or a name prefix.
func (s *Service) Find(ref string) (store.Session, error) {
	sessions, err := s.Store.LoadSessions()
	if err != nil {
		return store.Session{}, err
	}

	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Session{}, errors.New("session reference is required")
	}

	var matches []store.Session
	for _, session := range sessions {
		// The container name is what herdr, `container ls` and the tmux
		// status line show, so it is accepted as an exact reference too.
		if session.ID == ref || containerName(session.ID) == ref {
			return session, nil
		}
		if strings.HasPrefix(session.ID, ref) || strings.HasPrefix(strings.ToLower(session.Name), strings.ToLower(ref)) {
			matches = append(matches, session)
		}
	}

	switch len(matches) {
	case 0:
		return store.Session{}, fmt.Errorf("no session matches %q", ref)
	case 1:
		return matches[0], nil
	default:
		return store.Session{}, fmt.Errorf("multiple sessions match %q", ref)
	}
}
