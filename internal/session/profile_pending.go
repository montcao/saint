package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/toolprofile"
)

const pendingProfileLaunch = "saint-profile-launch-pending-v1\n"

func (s *Service) profilePendingPath(id string) (string, error) {
	if !store.ValidSessionID(id) {
		return "", errors.New("invalid session ID")
	}
	found, err := s.Find(id)
	if err != nil {
		return "", err
	}
	if found.ExperimentalProfile == "" {
		return "", errors.New("session is not an experimental profile")
	}
	if err = s.requireProfile(id); err != nil {
		return "", err
	}
	dir := filepath.Join(s.Store.Paths().Root, "tool-profiles", containerName(id))
	if err = toolprofile.PrivatePath(dir, true); err != nil {
		return "", err
	}
	return filepath.Join(dir, "attachment-pending"), nil
}

func syncPendingParent(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// MarkProfilePending must run under the pane lock before spawning a runtime
// client. It records possible asynchronous daemon work across host process death.
func (s *Service) MarkProfilePending(id string) error {
	path, err := s.profilePendingPath(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("reserve experimental launch marker: %w", err)
	}
	_, writeErr := f.WriteString(pendingProfileLaunch)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return syncPendingParent(path)
}

// RequireProfileQuiescent fails closed after an interrupted launch. A stopped
// runtime snapshot alone cannot prove a bootstrap RPC has finished. This guard
// protects both reattachment and deletion; ownership-checked Stop remains allowed.
func (s *Service) RequireProfileQuiescent(id string) error {
	path, err := s.profilePendingPath(id)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("experimental launch may still be pending; resources retained, reattachment and deletion require qualified recovery")
}

// ClearProfilePending is for completed creation, a failed local spawn that sent
// no RPC, or an ordinary completed CLI followed by an ownership-checked stop,
// under the pane lock. Never use it to force
// recovery from cancellation, timeout or uncertain daemon state.
func (s *Service) ClearProfilePending(id string) error {
	path, err := s.profilePendingPath(id)
	if err != nil {
		return err
	}
	if err = toolprofile.PrivatePath(path, false); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != pendingProfileLaunch {
		return errors.New("experimental launch marker differs; retained")
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	if err = syncPendingParent(path); err != nil {
		// A failed durability barrier must not silently permit the next launch.
		restoreErr := s.MarkProfilePending(id)
		return errors.Join(err, restoreErr)
	}
	return nil
}
