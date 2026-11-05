package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session directory is mounted read-write into the guest. Nothing the guest
// can write may decide which session the host thinks it is talking to.
func TestProxyTokenLivesOutsideTheGuestMount(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	token, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if token == "" {
		t.Fatal("EnsureProxyToken() returned an empty token")
	}

	paths := st.Paths()
	path := st.ProxyTokenPath("abc123")
	if strings.HasPrefix(path, paths.SessionsDir+string(filepath.Separator)) {
		t.Fatalf("token path %q is inside the mounted session directory", path)
	}
	if !strings.HasPrefix(path, paths.TokensDir+string(filepath.Separator)) {
		t.Fatalf("token path %q is not under the tokens directory", path)
	}

	// Nothing that looks like a credential may be left behind in the mount.
	sessionDir := filepath.Join(paths.SessionsDir, "abc123")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == legacyProxyTokenFile {
			t.Errorf("a token file remains in the mounted session directory")
		}
	}
}

func TestTokensDirIsPrivate(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if _, err := st.EnsureProxyToken("abc123"); err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	info, err := os.Stat(st.Paths().TokensDir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("tokens dir perms = %o, want 700", perm)
	}
}

// An install that already has tokens in the old location must keep working: a
// running container holds the old value in its environment and cannot be told a
// new one without being recreated.
func TestLegacyTokenIsMigratedNotReissued(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	dir, err := st.EnsureSessionDir("abc123")
	if err != nil {
		t.Fatalf("EnsureSessionDir() error = %v", err)
	}
	legacy := filepath.Join(dir, legacyProxyTokenFile)
	if err := os.WriteFile(legacy, []byte("existing-token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	token, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if token != "existing-token" {
		t.Errorf("token = %q, want the migrated value: reissuing breaks a running session", token)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the guest-writable copy survived migration: %v", err)
	}

	data, err := os.ReadFile(st.ProxyTokenPath("abc123"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.TrimSpace(string(data)) != "existing-token" {
		t.Errorf("host copy = %q, want the migrated value", strings.TrimSpace(string(data)))
	}
}

// A guest that tampered with its own token before migration could only achieve
// one thing: colliding with another session. Neither may then be served.
func TestDuplicateTokensAreRefused(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if err := st.SaveSessions([]Session{{ID: "aaa11111"}, {ID: "bbb22222"}, {ID: "ccc33333"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	for _, id := range []string{"aaa11111", "bbb22222"} {
		if _, err := st.EnsureProxyToken(id); err != nil {
			t.Fatalf("EnsureProxyToken(%s) error = %v", id, err)
		}
		if err := os.WriteFile(st.ProxyTokenPath(id), []byte("collided\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	unique, err := st.EnsureProxyToken("ccc33333")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	tokens, err := st.LoadProxyTokens()
	if err != nil {
		t.Fatalf("LoadProxyTokens() error = %v", err)
	}
	if _, ok := tokens["aaa11111"]; ok {
		t.Error("a session sharing a token was still served")
	}
	if _, ok := tokens["bbb22222"]; ok {
		t.Error("a session sharing a token was still served")
	}
	// The uninvolved session must not be collateral damage.
	if tokens["ccc33333"] != unique {
		t.Errorf("an unrelated session lost its token: %+v", tokens)
	}
}

func TestRemoveProxyToken(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if _, err := st.EnsureProxyToken("abc123"); err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if err := st.RemoveProxyToken("abc123"); err != nil {
		t.Fatalf("RemoveProxyToken() error = %v", err)
	}
	if _, err := os.Stat(st.ProxyTokenPath("abc123")); !os.IsNotExist(err) {
		t.Errorf("the token survived removal: %v", err)
	}
	// Removing twice is not an error: delete paths must be idempotent.
	if err := st.RemoveProxyToken("abc123"); err != nil {
		t.Errorf("RemoveProxyToken() on a missing token = %v, want nil", err)
	}
	if err := st.RemoveProxyToken("../escape"); err == nil {
		t.Error("RemoveProxyToken() accepted an invalid session id")
	}
}

// EnsureProxyToken migrates on the way past, but it only runs when a session is
// created. Sessions that already existed need migrating too, or the proxy stops
// recognising them and their agents fail every request.
func TestMigrateProxyTokensCoversExistingSessions(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if err := st.SaveSessions([]Session{{ID: "aaa11111"}, {ID: "bbb22222"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	for id, token := range map[string]string{"aaa11111": "token-a", "bbb22222": "token-b"} {
		dir, err := st.EnsureSessionDir(id)
		if err != nil {
			t.Fatalf("EnsureSessionDir() error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, legacyProxyTokenFile), []byte(token+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}

	if err := st.MigrateProxyTokens(); err != nil {
		t.Fatalf("MigrateProxyTokens() error = %v", err)
	}

	tokens, err := st.LoadProxyTokens()
	if err != nil {
		t.Fatalf("LoadProxyTokens() error = %v", err)
	}
	if tokens["aaa11111"] != "token-a" || tokens["bbb22222"] != "token-b" {
		t.Errorf("tokens = %+v, want the migrated values", tokens)
	}
	for _, id := range []string{"aaa11111", "bbb22222"} {
		if _, err := os.Stat(filepath.Join(st.Paths().SessionsDir, id, legacyProxyTokenFile)); !os.IsNotExist(err) {
			t.Errorf("%s still has a token inside the mounted session directory", id)
		}
	}
}

// Running it twice, or with nothing to migrate, must be a no-op.
func TestMigrateProxyTokensIsIdempotent(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if err := st.SaveSessions([]Session{{ID: "aaa11111"}}); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	token, err := st.EnsureProxyToken("aaa11111")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	for range 3 {
		if err := st.MigrateProxyTokens(); err != nil {
			t.Fatalf("MigrateProxyTokens() error = %v", err)
		}
	}

	tokens, err := st.LoadProxyTokens()
	if err != nil {
		t.Fatalf("LoadProxyTokens() error = %v", err)
	}
	if tokens["aaa11111"] != token {
		t.Errorf("token changed across migrations: %q, want %q", tokens["aaa11111"], token)
	}
}
