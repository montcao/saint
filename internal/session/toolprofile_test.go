package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

type profileFake struct {
	root string
	recordingSandbox
	preflightErr     error
	removeErr        error
	networkRemoveErr error
	missing          bool
	cancel           context.CancelFunc
	cleanupCanceled  bool
}

func (p *profileFake) PreflightProfile(context.Context, string) error { return p.preflightErr }
func (p *profileFake) ReserveProfile(_ context.Context, name string, _ string) error {
	return os.MkdirAll(filepath.Join(p.root, "tool-profiles", name), 0700)
}
func (p *profileFake) RequireProfile(string, string) error {
	if p.missing {
		return errors.New("missing owner record")
	}
	return nil
}
func (p *profileFake) Remove(ctx context.Context, _ string) error {
	p.cleanupCanceled = ctx.Err() != nil
	return p.removeErr
}
func (p *profileFake) RemoveNetwork(_ context.Context, name string) error {
	if p.networkRemoveErr != nil {
		return p.networkRemoveErr
	}
	p.removedNetworks = append(p.removedNetworks, name)
	return nil
}
func (p *profileFake) Create(ctx context.Context, name string, rt sandbox.Runtime, repos []sandbox.Repo, dir, url string, opts sandbox.CreateOptions) error {
	if p.cancel != nil {
		p.cancel()
	}
	return p.recordingSandbox.Create(ctx, name, rt, repos, dir, url, opts)
}
func TestProfilePreflightHasNoSessionOrNetworkEffects(t *testing.T) {
	svc, _, repo := newEgressService(t)
	sb := &profileFake{preflightErr: errors.New("unsupported runtime")}
	sb.root = svc.Store.Paths().Root
	svc.Sandbox = sb
	_, err := svc.Create(context.Background(), CreateRequest{ExperimentalProfile: "explicit", Credential: "api-key", Repos: []sandbox.Repo{{Path: repo}}})
	if err == nil {
		t.Fatal("unsupported runtime accepted")
	}
	sessions, err := svc.Store.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 || len(sb.networks) != 0 {
		t.Fatal("preflight had side effects")
	}
}
func TestProfileFailedRemovalRetainsSessionAndNetwork(t *testing.T) {
	svc, _, repo := newEgressService(t)
	sb := &profileFake{}
	sb.root = svc.Store.Paths().Root
	svc.Sandbox = sb
	s, err := svc.Create(context.Background(), CreateRequest{ExperimentalProfile: "explicit", Credential: "api-key", Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"vm", "network", "missing-owner"} {
		sb.removeErr = nil
		sb.networkRemoveErr = nil
		sb.missing = false
		switch phase {
		case "vm":
			sb.removeErr = errors.New("identity mismatch")
		case "network":
			sb.networkRemoveErr = errors.New("network owner differs")
		case "missing-owner":
			sb.missing = true
		}
		if err = svc.Delete(context.Background(), s.ID); err == nil {
			t.Fatal("cleanup should have failed")
		}
		if _, err = svc.Find(s.ID); err != nil {
			t.Fatal("lost ownership after failed cleanup", err)
		}
		if len(sb.removedNetworks) != 0 {
			t.Fatal("deleted network after failed cleanup")
		}
	}
	sb.missing = false
	sb.networkRemoveErr = nil
	if err = svc.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Find(s.ID); err == nil {
		t.Fatal("completed deletion retained session")
	}
}
func TestProfileCanceledCreateRetainsUncertainResources(t *testing.T) {
	svc, _, repo := newEgressService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := &profileFake{cancel: cancel}
	sb.createErr = errors.New("canceled create")
	sb.root = svc.Store.Paths().Root
	svc.Sandbox = sb
	if _, err := svc.Create(ctx, CreateRequest{ExperimentalProfile: "explicit", Credential: "api-key", Repos: []sandbox.Repo{{Path: repo}}}); err == nil {
		t.Fatal("create should fail")
	}
	if sb.cleanupCanceled {
		t.Fatal("cleanup reused canceled launch context")
	}
	sessions, err := svc.Store.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatal("uncertain creation lost ownership")
	}
	if err := svc.RequireProfileQuiescent(sessions[0].ID); err == nil {
		t.Fatal("uncertain creation lacks pending marker")
	}
	if err := svc.Delete(context.Background(), sessions[0].ID); err == nil {
		t.Fatal("deleted uncertain backing resources")
	}
}

func TestProfileDoesNotReadSubscriptionOrSwitchBilling(t *testing.T) {
	svc, _, repo := newEgressService(t)
	sb := &profileFake{}
	sb.root = svc.Store.Paths().Root
	svc.Sandbox = sb
	cfg, err := svc.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyAuth = "subscription"
	if err = svc.Store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Create(context.Background(), CreateRequest{ExperimentalProfile: "explicit", Repos: []sandbox.Repo{{Path: repo}}}); err == nil {
		t.Fatal("silently changed subscription billing")
	}
	if len(sb.networks) != 0 {
		t.Fatal("billing rejection created a network")
	}
	s, err := svc.Create(context.Background(), CreateRequest{ExperimentalProfile: "explicit", Credential: "api-key", Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RenewCredential(context.Background(), s.ID); err == nil {
		t.Fatal("renewal could recreate a legacy VM")
	}
	if _, err = svc.Find(s.ID); err != nil {
		t.Fatal("rejected renewal lost session")
	}
}
