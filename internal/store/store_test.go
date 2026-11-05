package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
)

func TestValidSessionID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "lowercase base32", id: "abc123xy", want: true},
		{name: "single char", id: "a", want: true},
		{name: "with hyphens", id: "a-b-c", want: true},
		{name: "32 chars", id: "a" + strings.Repeat("b", 31), want: true},
		{name: "empty", id: "", want: false},
		{name: "leading hyphen smuggles a flag", id: "-rm", want: false},
		{name: "leading hyphen after prefix", id: "-force", want: false},
		{name: "uppercase", id: "ABC", want: false},
		{name: "underscore", id: "a_b", want: false},
		{name: "path traversal", id: "../../etc", want: false},
		{name: "slash", id: "a/b", want: false},
		{name: "space", id: "a b", want: false},
		{name: "newline", id: "a\nb", want: false},
		{name: "too long", id: "a" + strings.Repeat("b", 32), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ValidSessionID(tc.id); got != tc.want {
				t.Fatalf("ValidSessionID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestLoadSessionsSkipsInvalidIDs(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	st := New(home)
	if err := st.Ensure(); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	raw := `[
	  {"id": "good1", "name": "keep"},
	  {"id": "--privileged", "name": "flag smuggle"},
	  {"id": "../../escape", "name": "traversal"},
	  {"id": "good2", "name": "keep too"}
	]`
	if err := os.WriteFile(st.Paths().SessionsPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sessions, warnings, err := st.LoadSessionsWithWarnings()
	if err != nil {
		t.Fatalf("LoadSessionsWithWarnings() error = %v", err)
	}
	if len(sessions) != 2 || sessions[0].ID != "good1" || sessions[1].ID != "good2" {
		t.Fatalf("sessions = %#v", sessions)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, warning := range warnings {
		if !strings.Contains(warning, "invalid id") {
			t.Fatalf("warning = %q", warning)
		}
	}

	plain, err := st.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions() error = %v", err)
	}
	if len(plain) != 2 {
		t.Fatalf("LoadSessions() = %#v", plain)
	}
}

func TestEnsureSessionDirRejectsInvalidID(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	for _, id := range []string{"", "../escape", "-rf", "Upper"} {
		if _, err := st.EnsureSessionDir(id); err == nil {
			t.Fatalf("EnsureSessionDir(%q) error = nil, want non-nil", id)
		}
	}
}

func TestEnsureProxyTokenIsIdempotentAndPrivate(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	first, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if len(first) < 40 {
		t.Fatalf("token = %q, want >=32 bytes of entropy encoded", first)
	}
	if strings.ContainsAny(first, " \n\t/+=") {
		t.Fatalf("token = %q, want base64url with no padding", first)
	}

	second, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() second call error = %v", err)
	}
	if second != first {
		t.Fatalf("token changed across calls: %q then %q", first, second)
	}

	other, err := st.EnsureProxyToken("def456")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if other == first {
		t.Fatal("two sessions got the same proxy token")
	}

	info, err := os.Stat(st.ProxyTokenPath("abc123"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy-token perms = %o, want 600", info.Mode().Perm())
	}
}

func TestEnsureProxyTokenRepairsLoosePermissions(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	token, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	path := st.ProxyTokenPath("abc123")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	again, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	if again != token {
		t.Fatalf("token changed: %q then %q", token, again)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy-token perms = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadProxyTokensOnlyReturnsKnownSessions(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	sessions := []Session{{ID: "abc123", Name: "one"}, {ID: "def456", Name: "two"}}
	if err := st.SaveSessions(sessions); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	want, err := st.EnsureProxyToken("abc123")
	if err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}
	// A session directory that exists but belongs to no recorded session must
	// not yield a usable token.
	if _, err := st.EnsureProxyToken("ghi789"); err != nil {
		t.Fatalf("EnsureProxyToken() error = %v", err)
	}

	tokens, err := st.LoadProxyTokens()
	if err != nil {
		t.Fatalf("LoadProxyTokens() error = %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens = %#v", tokens)
	}
	if tokens["abc123"] != want {
		t.Fatalf("tokens[abc123] = %q, want %q", tokens["abc123"], want)
	}
}

func TestStoreRoundTripSessions(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	st := New(home)
	sessions := []Session{{
		ID:       "abc123",
		Name:     "demo",
		Runtime:  sandbox.RuntimeClaude,
		MemoryGB: 2,
		CPUs:     2,
		Repos: []sandbox.Repo{{
			Path: "/tmp/repo",
		}},
	}}

	if err := st.SaveSessions(sessions); err != nil {
		t.Fatalf("SaveSessions() error = %v", err)
	}
	got, err := st.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "abc123" {
		t.Fatalf("got %#v", got)
	}

	info, err := os.Stat(st.Paths().SessionsPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json perms = %o, want 600", info.Mode().Perm())
	}
}

func TestEnsureConfigWritesDefaults(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	cfg, err := st.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	if cfg.DefaultRuntime != sandbox.RuntimeClaude {
		t.Fatalf("DefaultRuntime = %q", cfg.DefaultRuntime)
	}
	if _, err := os.Stat(st.Paths().ConfigPath); err != nil {
		t.Fatalf("config file missing: %v", err)
	}
}

func TestEnsureSessionDirPermissions(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	path, err := st.EnsureSessionDir("xyz")
	if err != nil {
		t.Fatalf("EnsureSessionDir() error = %v", err)
	}
	if filepath.Base(path) != "xyz" {
		t.Fatalf("session dir = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("session dir perms = %o, want 700", info.Mode().Perm())
	}
}
