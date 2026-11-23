package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/toolprofile"
)

// LockProfile serializes creation, attached start and deletion. The inode is
// outside all guest shares and is retained with the ownership tombstone so a
// waiting process can never acquire a replacement lock for the same session.
func (s *Service) LockProfile(id string) (*os.File, error) {
	if !store.ValidSessionID(id) {
		return nil, errors.New("invalid session ID")
	}
	dir := filepath.Join(s.Store.Paths().Root, "tool-profiles", containerName(id))
	if err := toolprofile.PrivatePath(dir, true); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "pane.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = toolprofile.PrivatePath(path, false); err != nil {
		f.Close()
		return nil, err
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("session already has an active lifecycle operation: %w", err)
	}
	return f, nil
}

func (s *Service) DirectAttachCmd(ctx context.Context, id string) (*exec.Cmd, error) {
	found, err := s.Find(id)
	if err != nil {
		return nil, err
	}
	if found.ExperimentalProfile == "" {
		return nil, errors.New("session is not an experimental profile")
	}
	if err = s.requireProfile(id); err != nil {
		return nil, err
	}
	ready := filepath.Join(s.Store.Paths().SessionsDir, id, "direct-proxy-ready")
	if err = os.Remove(ready); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clear readiness: %w", err)
	}
	if err = s.touch(id); err != nil {
		return nil, err
	}
	return s.Sandbox.AttachCmd(containerName(id), sandbox.AttachOptions{DirectContext: ctx})
}
