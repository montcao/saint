package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

func pendingProfileFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	svc, _, repo := newEgressService(t)
	svc.Sandbox = &profileFake{root: svc.Store.Paths().Root}
	found, err := svc.Create(context.Background(), CreateRequest{ExperimentalProfile: "synthetic-profile", Credential: "api-key", Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(svc.Store.Paths().Root, "tool-profiles", containerName(found.ID))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := svc.profilePendingPath(found.ID)
	if err != nil {
		t.Fatal(err)
	}
	return svc, found.ID, path
}

func TestProfileLaunchMarkerBlocksReuseUntilOrdinaryCompletion(t *testing.T) {
	svc, id, path := pendingProfileFixture(t)
	if err := svc.RequireProfileQuiescent(id); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkProfilePending(id); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkProfilePending(id); err == nil {
		t.Fatal("replaced pending launch")
	}
	if err := svc.RequireProfileQuiescent(id); err == nil {
		t.Fatal("pending launch accepted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("launch marker is not private", err)
	}
	if err := svc.ClearProfilePending(id); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequireProfileQuiescent(id); err != nil {
		t.Fatal(err)
	}
}

func TestProfileLaunchMarkerNeverFollowsAliases(t *testing.T) {
	svc, id, path := pendingProfileFixture(t)
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("synthetic sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkProfilePending(id); err == nil {
		t.Fatal("followed marker alias")
	}
	if err := svc.ClearProfilePending(id); err == nil {
		t.Fatal("cleared marker alias")
	}
	if err := svc.RequireProfileQuiescent(id); err == nil {
		t.Fatal("accepted ambiguous marker")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "synthetic sentinel" {
		t.Fatal("modified unrelated target")
	}
}
