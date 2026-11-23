package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/montcao/saint/internal/store"
)

type profileSandbox interface {
	PreflightProfile(context.Context, string) error
	RequireProfile(string, string) error
	ReserveProfile(context.Context, string, string) error
}

func (s *Service) PreflightProfile(ctx context.Context, ref string) error {
	p, ok := s.Sandbox.(profileSandbox)
	if !ok {
		return errors.New("sandbox does not support experimental tool profiles")
	}
	return p.PreflightProfile(ctx, ref)
}

func (s *Service) requireProfile(id string) error {
	found, err := s.Find(id)
	if err != nil {
		return nil
	} // Preserve the legacy path for unrecorded names.
	if found.ExperimentalProfile == "" {
		return nil
	}
	p, ok := s.Sandbox.(profileSandbox)
	if !ok {
		return errors.New("experimental profile backend unavailable")
	}
	return p.RequireProfile(s.ContainerName(id), found.ExperimentalProfile)
}

// All cleanup is retryable; session metadata is the last resource released.
func (s *Service) deleteProfile(ctx context.Context, found store.Session) error {
	if err := s.Sandbox.Remove(ctx, containerName(found.ID)); err != nil {
		return err
	}
	if found.Network != "" {
		if err := s.Sandbox.RemoveNetwork(ctx, found.Network); err != nil {
			return err
		}
	}
	if err := s.Store.RemoveProxyToken(found.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.Store.Paths().SessionsDir, found.ID)); err != nil {
		return err
	}
	_, err := s.Store.RemoveSession(found.ID)
	return err
}
