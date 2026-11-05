package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestValidateContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "generated id", value: "saint-abc123xy"},
		{name: "digits first", value: "saint-2abcdefg"},
		{name: "hyphenated id", value: "saint-a-b-c"},
		{name: "max length id", value: "saint-a" + strings.Repeat("b", 31)},
		{name: "empty", value: "", wantErr: true},
		{name: "missing prefix", value: "abc123", wantErr: true},
		{name: "flag smuggled as id", value: "saint---privileged", wantErr: true},
		{name: "bare flag", value: "--all", wantErr: true},
		{name: "leading hyphen", value: "-saint-abc", wantErr: true},
		{name: "uppercase", value: "saint-ABC", wantErr: true},
		{name: "path traversal", value: "saint-../../etc", wantErr: true},
		{name: "space", value: "saint-abc def", wantErr: true},
		{name: "newline", value: "saint-abc\n--force", wantErr: true},
		{name: "too long", value: "saint-a" + strings.Repeat("b", 32), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateContainerName(tc.value)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateContainerName(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
		})
	}
}

// TestLifecycleRejectsUnvalidatedNames pins the defensive check that sits
// immediately before every exec, so a hostile sessions.json entry can never
// reach the container CLI even if the store-level filter is bypassed.
func TestLifecycleRejectsUnvalidatedNames(t *testing.T) {
	t.Parallel()

	// A path that does not exist: if validation is ever removed these calls
	// would fail on exec instead, so assert on the error text.
	a := NewAppleContainer("/nonexistent/container", nil)
	ctx := context.Background()
	const bad = "saint---privileged"

	ops := map[string]func() error{
		"stop":   func() error { return a.Stop(ctx, bad) },
		"start":  func() error { return a.Start(ctx, bad) },
		"remove": func() error { return a.Remove(ctx, bad) },
		"attach": func() error { _, err := a.AttachCmd(bad, AttachOptions{}); return err },
		"create": func() error {
			return a.Create(ctx, bad, RuntimeClaude, []Repo{{Path: "/tmp/repo"}}, "/tmp/session", "", CreateOptions{})
		},
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := op()
			if err == nil {
				t.Fatal("error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), "invalid container name") {
				t.Fatalf("error = %v, want invalid container name", err)
			}
		})
	}
}

func TestAttachCmdBuildsExpectedArgv(t *testing.T) {
	t.Parallel()

	a := NewAppleContainer("/opt/homebrew/bin/container", nil)
	cmd, err := a.AttachCmd("saint-abc123", AttachOptions{})
	if err != nil {
		t.Fatalf("AttachCmd() error = %v", err)
	}
	if cmd.Path != "/opt/homebrew/bin/container" {
		t.Fatalf("Path = %q", cmd.Path)
	}
	// new-session -A, not attach: a container whose tmux server has died must
	// still be recoverable.
	want := []string{"/opt/homebrew/bin/container", "exec", "-it", "saint-abc123", "tmux", "new-session", "-A", "-D", "-s", "agent"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("Args = %#v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("Args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}
