package session

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hostClaude builds a ~/.claude that holds everything a real one can, the
// configuration the guest should get and the data it must never see.
func hostClaude(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".claude")
	writeFile(t, filepath.Join(dir, "settings.json"), `{
		"permissions": {"allow": ["Bash(make check)"]},
		"model": "opus",
		"env": {"ANTHROPIC_API_KEY": "sk-ant-fixture"},
		"apiKeyHelper": "/usr/local/bin/print-key",
		"hooks": {"Stop": []},
		"statusLine": {"type": "command", "command": "status.sh"},
		"enabledPlugins": {"p@m": true},
		"enableAllProjectMcpServers": true,
		"mcpServers": {"db": {"command": "db-mcp", "env": {"TOKEN": "fixture"}}}
	}`)
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "be terse")
	writeFile(t, filepath.Join(dir, "history.jsonl"), `{"display":"a private prompt"}`)
	writeFile(t, filepath.Join(dir, ".credentials.json"), `{"claudeAiOauth":{}}`)
	writeFile(t, filepath.Join(dir, "projects", "p", "transcript.jsonl"), "private transcript")

	writeFile(t, filepath.Join(dir, "skills", "review", "SKILL.md"), "review skill")
	writeFile(t, filepath.Join(dir, "skills", "review", "notes", "checklist.md"), "checklist")
	writeFile(t, filepath.Join(dir, "skills", "review", ".env"), "TOKEN=fixture")
	writeFile(t, filepath.Join(dir, "skills", "review", "deploy.pem"), "fixture")
	writeFile(t, filepath.Join(dir, "skills", "notaskill", "README.md"), "no SKILL.md here")
	writeFile(t, filepath.Join(dir, "agents", "planner.md"), "planner agent")

	// A secret elsewhere on the host, reachable only through symlinks.
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, filepath.Join(outside, "id_ed25519"), "fixture key")
	if err := os.Symlink(filepath.Join(outside, "id_ed25519"), filepath.Join(dir, "skills", "review", "key")); err != nil {
		t.Fatal(err)
	}
	// A skill installed as a symlink to its own directory is common, and is
	// copied.
	linked := filepath.Join(t.TempDir(), "linked-skill")
	writeFile(t, filepath.Join(linked, "SKILL.md"), "linked skill")
	if err := os.Symlink(linked, filepath.Join(dir, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("snapshot contains a symlink: %s", path)
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The guest gets the user's configuration and nothing else from ~/.claude:
// not prompts, transcripts or credentials, not commands that run on the
// user's behalf, and no MCP server (an owner decision on #36).
func TestTheGuestConfigSnapshotCopiesOnlyTheAllowlist(t *testing.T) {
	t.Parallel()

	svc, _, _ := newEgressService(t)
	svc.HostClaudeDir = hostClaude(t)

	snap, err := svc.snapshotClaudeConfig("abc123")
	if err != nil {
		t.Fatalf("snapshotClaudeConfig() error = %v", err)
	}
	files := snapshotFiles(t, snap.Dir)

	for _, want := range []string{"settings.json", "CLAUDE.md", "skills/review/SKILL.md", "skills/review/notes/checklist.md", "skills/linked/SKILL.md", "agents/planner.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("snapshot is missing %s; have %v", want, keys(files))
		}
	}
	for name, content := range files {
		for _, leaked := range []string{"sk-ant-fixture", "fixture key", "TOKEN=fixture", "a private prompt", "private transcript", "claudeAiOauth"} {
			if strings.Contains(content, leaked) {
				t.Errorf("snapshot file %s contains %q", name, leaked)
			}
		}
	}
	for _, banned := range []string{"history.jsonl", ".credentials.json", "projects/p/transcript.jsonl", "skills/notaskill/README.md", "skills/review/.env", "skills/review/deploy.pem", "skills/review/key"} {
		if _, ok := files[banned]; ok {
			t.Errorf("snapshot contains %s", banned)
		}
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(files["settings.json"]), &settings); err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	for key := range settings {
		if !guestSettingsKeys[key] {
			t.Errorf("settings.json kept %q, which is not on the allowlist", key)
		}
	}
	for _, want := range []string{"permissions", "model"} {
		if _, ok := settings[want]; !ok {
			t.Errorf("settings.json dropped %q", want)
		}
	}
	for _, key := range []string{"mcpServers", "enableAllProjectMcpServers", "env", "apiKeyHelper", "hooks", "statusLine", "enabledPlugins"} {
		if guestSettingsKeys[key] {
			t.Errorf("%q is on the settings allowlist; it must never reach the guest", key)
		}
	}
	if len(snap.Skipped) == 0 {
		t.Error("nothing was reported as skipped; the symlink, .env and .pem should have been")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A large file in a skill must not be copied into every session.
func TestTheGuestConfigSnapshotIsBounded(t *testing.T) {
	t.Parallel()

	svc, _, _ := newEgressService(t)
	dir := filepath.Join(t.TempDir(), ".claude")
	writeFile(t, filepath.Join(dir, "skills", "big", "SKILL.md"), "big")
	writeFile(t, filepath.Join(dir, "skills", "big", "data.bin"), strings.Repeat("x", guestConfigMaxFile+1))
	svc.HostClaudeDir = dir

	snap, err := svc.snapshotClaudeConfig("abc123")
	if err != nil {
		t.Fatalf("snapshotClaudeConfig() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(snap.Dir, "skills", "big", "data.bin")); err == nil {
		t.Error("a file over the per-file limit was copied")
	}
}

// No ~/.claude is an ordinary install, not an error.
func TestAMissingHostClaudeDirectoryIsNotAnError(t *testing.T) {
	t.Parallel()

	svc, _, _ := newEgressService(t)
	svc.HostClaudeDir = filepath.Join(t.TempDir(), "absent")
	if _, err := svc.snapshotClaudeConfig("abc123"); err != nil {
		t.Fatalf("snapshotClaudeConfig() error = %v", err)
	}
}

// Creating a Claude session hands the sandbox the snapshot and the user's git
// identity; deleting it removes the snapshot.
func TestCreateCarriesTheConfigSnapshotAndGitIdentity(t *testing.T) {
	t.Parallel()

	svc, sb, repo := newEgressService(t)
	svc.HostClaudeDir = hostClaude(t)
	svc.GitIdentity = func() (string, string) { return "Ada Example", "ada@example.com\nInjected: x" }

	created, err := svc.Create(context.Background(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if sb.opts.ClaudeConfigDir == "" {
		t.Fatal("Create() passed no configuration snapshot")
	}
	if !strings.HasPrefix(sb.opts.ClaudeConfigDir, svc.Store.Paths().GuestConfigDir) {
		t.Errorf("snapshot %s is outside the host-only guest-config directory", sb.opts.ClaudeConfigDir)
	}
	if strings.HasPrefix(sb.opts.ClaudeConfigDir, svc.Store.Paths().SessionsDir) {
		t.Errorf("snapshot %s is inside the guest-writable sessions directory", sb.opts.ClaudeConfigDir)
	}
	if sb.opts.GitName != "Ada Example" {
		t.Errorf("GitName = %q", sb.opts.GitName)
	}
	if sb.opts.GitEmail != "" {
		t.Errorf("GitEmail = %q; a value with a newline must be dropped", sb.opts.GitEmail)
	}

	if err := svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.Store.Paths().GuestConfigDir, created.ID)); !os.IsNotExist(err) {
		t.Errorf("Delete() left the snapshot behind: %v", err)
	}
}

// Renewing keeps the snapshot the session was created with. A host change
// made since then reaches new sessions, not this one, for the same reason a
// drifted skill blocks renewal: nobody saw it for this session.
func TestRenewKeepsTheOriginalConfigSnapshot(t *testing.T) {
	t.Parallel()

	svc, home := newSkillsService(t)
	sb := &recordingSandbox{}
	svc.Sandbox = sb
	setUpRenewableSession(t, svc, home)
	host := hostClaude(t)
	svc.HostClaudeDir = host

	snap, err := svc.snapshotClaudeConfig("abc123")
	if err != nil {
		t.Fatalf("snapshotClaudeConfig() error = %v", err)
	}
	writeFile(t, filepath.Join(host, "CLAUDE.md"), "changed on the host")

	if _, err := svc.RenewCredential(context.Background(), "abc123"); err != nil {
		t.Fatalf("RenewCredential() error = %v", err)
	}
	if sb.opts.ClaudeConfigDir != snap.Dir {
		t.Errorf("renew used %q, want the original snapshot %q", sb.opts.ClaudeConfigDir, snap.Dir)
	}
	data, err := os.ReadFile(filepath.Join(snap.Dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "be terse" {
		t.Errorf("renew refreshed the snapshot: CLAUDE.md = %q", data)
	}
}
