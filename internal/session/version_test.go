package session

import (
	"context"
	"errors"
	"github.com/montcao/saint/internal/claudeimage"
	"github.com/montcao/saint/internal/sandbox"
	"strings"
	"testing"
)

type versionSandbox struct {
	*recordingSandbox
	version   string
	calls     int
	err       error
	verified  int
	verifyErr error
}

func (s *versionSandbox) PrepareClaude(context.Context) (claudeimage.Selection, error) {
	s.calls++
	v, e := claudeimage.Select(s.version, "arm64", []byte("recipe"))
	v.Digest = "sha256:" + strings.Repeat("a", 64)
	if s.err != nil {
		e = s.err
	}
	return v, e
}
func TestNewVersionsResolvedButExistingSessionRetained(t *testing.T) {
	svc, base, repo := newEgressService(t)
	sb := &versionSandbox{recordingSandbox: base, version: "2.1.3"}
	svc.Sandbox = sb
	first, e := svc.Create(t.Context(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if e != nil {
		t.Fatal(e)
	}
	sb.version = "2.1.4"
	if e := svc.Start(t.Context(), first.ID); e != nil {
		t.Fatal(e)
	}
	if sb.calls != 1 {
		t.Fatal("restart resolved new version")
	}
	opts := svc.attachOptions(first.ID)
	if opts.Command == "" {
		t.Fatal("retained attach command absent")
	}
	if _, e := svc.RenewCredential(t.Context(), first.ID); e != nil {
		t.Fatal(e)
	}
	if sb.verified != 1 || sb.calls != 1 || base.opts.AgentVersion != "2.1.3" {
		t.Fatal("renew changed stored version")
	}
	second, e := svc.Create(t.Context(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if e != nil {
		t.Fatal(e)
	}
	all, e := svc.Store.LoadSessions()
	if e != nil {
		t.Fatal(e)
	}
	versions := map[string]string{}
	for _, s := range all {
		versions[s.ID] = s.AgentVersion
	}
	if versions[first.ID] != "2.1.3" || versions[second.ID] != "2.1.4" || sb.calls != 2 || base.opts.AgentVersion != "2.1.4" {
		t.Fatal(versions, sb.calls)
	}
}
func TestUnavailableVersionAllocatesNoSessionResources(t *testing.T) {
	svc, base, repo := newEgressService(t)
	svc.Sandbox = &versionSandbox{recordingSandbox: base, version: "2.1.3", err: errors.New("exact version unavailable")}
	if _, e := svc.Create(t.Context(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}}); e == nil {
		t.Fatal("failure ignored")
	}
	if len(base.networks) != 0 {
		t.Fatal("network allocated before image verified")
	}
	all, e := svc.Store.LoadSessions()
	if e != nil || len(all) != 0 {
		t.Fatal(all, e)
	}
}

func (s *versionSandbox) VerifyClaude(_ context.Context, v claudeimage.Selection) error {
	s.verified++
	if v.Version != "2.1.3" {
		return errors.New("changed retained version")
	}
	return s.verifyErr
}

// Embedding only the Sandbox interface intentionally hides version capabilities.
type noVersionSandbox struct{ sandbox.Sandbox }

func TestMissingVersionCapabilitiesFailClosed(t *testing.T) {
	svc, base, repo := newEgressService(t)
	svc.Sandbox = noVersionSandbox{base}
	if _, err := svc.Create(t.Context(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}}); err == nil {
		t.Fatal("missing preparer accepted")
	}
	if len(base.networks) != 0 {
		t.Fatal("allocated before preparation")
	}
	svc.Sandbox = base
	first, err := svc.Create(t.Context(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	svc.Sandbox = noVersionSandbox{base}
	if _, err := svc.RenewCredential(t.Context(), first.ID); err == nil {
		t.Fatal("missing verifier accepted")
	}
	if base.removed {
		t.Fatal("removed before verification")
	}
	failing := &versionSandbox{recordingSandbox: base, verifyErr: errors.New("mismatch")}
	svc.Sandbox = failing
	if _, err := svc.RenewCredential(t.Context(), first.ID); err == nil {
		t.Fatal("failed verification accepted")
	}
	if base.removed || failing.verified != 1 {
		t.Fatal("verification did not precede removal")
	}
}
