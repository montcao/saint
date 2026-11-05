package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// newSkillsService builds a service whose home is a temp directory, so
// SkillsRootOrDefault resolves to a directory nothing outside the test owns.
func newSkillsService(t *testing.T) (*Service, string) {
	t.Helper()

	home := t.TempDir()
	st := store.New(home)
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	return &Service{Store: st}, resolved
}

// writeSkill creates a skill directory under root/name with the given files
// and registers it in cfg.Skills, returning the updated config.
func writeSkill(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	return dir
}

func TestResolveSkillsMountsAConfiguredSkill(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	root := filepath.Join(home, ".saint", "skills")
	dir := writeSkill(t, root, "review-checklist", map[string]string{"SKILL.md": "read this first"})

	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.Skills = map[string]string{"review-checklist": dir}

	mounts, err := svc.resolveSkills(cfg, []string{"review-checklist"})
	if err != nil {
		t.Fatalf("resolveSkills() error = %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("len(mounts) = %d, want 1", len(mounts))
	}
	if mounts[0].Hash == "" {
		t.Fatal("Hash is empty, want a digest")
	}

	again, err := svc.resolveSkills(cfg, []string{"review-checklist"})
	if err != nil {
		t.Fatalf("resolveSkills() second call error = %v", err)
	}
	if again[0].Hash != mounts[0].Hash {
		t.Fatalf("hash changed across calls with no content change: %q vs %q", mounts[0].Hash, again[0].Hash)
	}
}

func TestResolveSkillsRefusesAnUnconfiguredName(t *testing.T) {
	t.Parallel()

	svc, _ := newSkillsService(t)
	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}

	_, err = svc.resolveSkills(cfg, []string{"never-installed"})
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "is not configured") {
		t.Errorf("error = %v, want it to say the skill is not configured", err)
	}
}

func TestResolveSkillsRefusesAPathOutsideSkillsRoot(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	outside := writeSkill(t, filepath.Join(home, "elsewhere"), "smuggled", map[string]string{"SKILL.md": "x"})

	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.Skills = map[string]string{"smuggled": outside}

	_, err = svc.resolveSkills(cfg, []string{"smuggled"})
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "outside skills_root") {
		t.Errorf("error = %v, want it to name skills_root", err)
	}
}

func TestResolveSkillsRefusesAnInvalidName(t *testing.T) {
	t.Parallel()

	svc, _ := newSkillsService(t)
	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}

	_, err = svc.resolveSkills(cfg, []string{"../../etc"})
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "invalid skill name") {
		t.Errorf("error = %v, want it to say the name is invalid", err)
	}
}

func TestSkillDriftDetectsAChangedFile(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	root := filepath.Join(home, ".saint", "skills")
	dir := writeSkill(t, root, "release-notes", map[string]string{"SKILL.md": "v1"})

	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.Skills = map[string]string{"release-notes": dir}
	if err := svc.Store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	mounts, err := svc.resolveSkills(cfg, []string{"release-notes"})
	if err != nil {
		t.Fatalf("resolveSkills() error = %v", err)
	}
	session := store.Session{SkillHashes: map[string]string{"release-notes": mounts[0].Hash}}

	drift, err := svc.SkillDrift(session)
	if err != nil {
		t.Fatalf("SkillDrift() error = %v", err)
	}
	if drift["release-notes"] {
		t.Fatal("drift[release-notes] = true, want false before any change")
	}

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	drift, err = svc.SkillDrift(session)
	if err != nil {
		t.Fatalf("SkillDrift() error = %v", err)
	}
	if !drift["release-notes"] {
		t.Fatal("drift[release-notes] = false, want true after the file changed on disk")
	}
}

// setUpRenewableSession configures svc with a session that names one skill,
// recorded at whatever hash the skill has on disk right now, and returns the
// skill's directory so the caller can drift it before renewing.
func setUpRenewableSession(t *testing.T, svc *Service, home string) (dir string, now time.Time) {
	t.Helper()

	root := filepath.Join(home, ".saint", "skills")
	dir = writeSkill(t, root, "release-notes", map[string]string{"SKILL.md": "v1"})

	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.Skills = map[string]string{"release-notes": dir}
	if err := svc.Store.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	mounts, err := svc.resolveSkills(cfg, []string{"release-notes"})
	if err != nil {
		t.Fatalf("resolveSkills() error = %v", err)
	}
	if err := svc.Store.SaveSessions([]store.Session{{
		ID: "abc123", Runtime: sandbox.RuntimeClaude, Repos: []sandbox.Repo{{Path: home}},
		ProxyAuth: store.ProxyAuthSubscription, Credential: store.CredentialClaudeCode,
		SkillHashes: map[string]string{"release-notes": mounts[0].Hash},
	}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}

	now = time.Unix(0, 0)
	svc.Now = func() time.Time { return now }
	svc.Credentials = func(string) (credential.Source, error) {
		return inspectableStub{stubSource{
			value:  "fresh-token",
			status: credential.Status{Present: true, ExpiresAt: now.Add(4 * time.Hour)},
		}}, nil
	}
	return dir, now
}

// A running guest cannot be handed new content, only a new container - so
// renewing a credential is the one moment a drifted skill could slip into a
// session nobody reviewed it for. That must refuse, the same as an
// unreadable credential does, and for the same reason: the session is left
// exactly as it was, which can still be undone.
func TestRenewCredentialRefusesWhenASkillHasDrifted(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	sb := &recordingSandbox{}
	svc.Sandbox = sb
	dir, _ := setUpRenewableSession(t, svc, home)

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := svc.RenewCredential(context.Background(), "abc123"); err == nil {
		t.Fatal("RenewCredential() succeeded despite a drifted skill")
	} else if !strings.Contains(err.Error(), "changed on disk") {
		t.Errorf("error = %v, want it to name the drifted skill", err)
	}
	if sb.removed {
		t.Error("a working container was removed for a renewal that could not proceed")
	}
}

func TestRenewCredentialCarriesSkillMountsForwardWhenNothingDrifted(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	sb := &recordingSandbox{}
	svc.Sandbox = sb
	setUpRenewableSession(t, svc, home)

	if _, err := svc.RenewCredential(context.Background(), "abc123"); err != nil {
		t.Fatalf("RenewCredential() error = %v", err)
	}
	if !sb.removed {
		t.Error("the old container was not removed")
	}
	if len(sb.opts.SkillMounts) != 1 || sb.opts.SkillMounts[0].Name != "release-notes" {
		t.Errorf("SkillMounts = %+v, want the unchanged skill carried forward", sb.opts.SkillMounts)
	}
}
