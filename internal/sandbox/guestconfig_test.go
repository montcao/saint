package sandbox

import (
	"strings"
	"testing"
)

// The configuration snapshot is mounted read-only, and outside /saint, which
// the guest can write: a guest must not be able to rewrite the copy it reads
// its instructions from on the host.
func TestCreateMountsTheConfigSnapshotReadOnly(t *testing.T) {
	t.Parallel()

	argv := createArgv(t, CreateOptions{ClaudeConfigDir: "/state/guest-config/abc123/claude"})
	want := "/state/guest-config/abc123/claude:" + guestClaudeConfigMount + ":ro"
	found := false
	for _, arg := range argv {
		if arg == want {
			found = true
		}
	}
	if !found {
		t.Errorf("argv does not mount the snapshot read-only as %q:\n%v", want, argv)
	}
	if strings.HasPrefix(guestClaudeConfigMount, "/saint/") {
		t.Errorf("snapshot mount %s is inside the guest-writable /saint", guestClaudeConfigMount)
	}

	without := strings.Join(createArgv(t, CreateOptions{}), "\n")
	if strings.Contains(without, guestClaudeConfigMount) {
		t.Error("a session without a snapshot still mounts one")
	}
}

// git reads GIT_AUTHOR_* and GIT_COMMITTER_* before any configuration file,
// so the guest's commits carry the user's name without a file being written.
func TestCreateGivesTheGuestTheUsersGitIdentity(t *testing.T) {
	t.Parallel()

	joined := strings.Join(createArgv(t, CreateOptions{GitName: "Ada Example", GitEmail: "ada@example.com"}), "\n")
	for _, want := range []string{
		"GIT_AUTHOR_NAME=Ada Example", "GIT_COMMITTER_NAME=Ada Example",
		"GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_EMAIL=ada@example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q", want)
		}
	}
	if strings.Contains(strings.Join(createArgv(t, CreateOptions{}), "\n"), "GIT_AUTHOR_NAME") {
		t.Error("an empty identity still sets GIT_AUTHOR_NAME")
	}
}

// The bootstrap links the snapshot into ~/.claude, copies settings.json so
// Claude Code can still write it, and leaves alone anything the guest has
// replaced with its own. Codex reads none of it.
func TestBootstrapLinksTheConfigSnapshot(t *testing.T) {
	t.Parallel()

	script, err := runtimeBootstrapScript(RuntimeClaude, "/work", CreateOptions{ClaudeConfigDir: "/host/snapshot"})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}
	for _, want := range []string{
		"cp " + guestClaudeConfigMount + "/settings.json",
		"CLAUDE.md skills agents commands output-styles",
		`ln -sfn "` + guestClaudeConfigMount,
		`[ -L "$HOME/.claude/$_saint_n" ]`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap is missing %q:\n%s", want, script)
		}
	}

	codex, err := runtimeBootstrapScript(RuntimeCodex, "/work", CreateOptions{ClaudeConfigDir: "/host/snapshot"})
	if err != nil {
		t.Fatalf("runtimeBootstrapScript() error = %v", err)
	}
	if strings.Contains(codex, guestClaudeConfigMount) {
		t.Error("codex bootstrap links Claude Code configuration")
	}
}
