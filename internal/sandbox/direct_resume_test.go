package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectReopenContinuesLastConversation(t *testing.T) {
	t.Parallel()
	for _, rt := range []Runtime{RuntimeClaude, RuntimeCodex} {
		t.Run(string(rt), func(t *testing.T) {
			home := t.TempDir()
			fake := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 2.1.247; exit; fi\nprintf '%s\\n' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(home, string(rt)), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			script, err := runtimeBootstrapScript(rt, home, CreateOptions{Direct: true, Prompt: "first prompt"})
			if err != nil {
				t.Fatal(err)
			}
			run := func() string {
				cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
				cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+home+":"+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v %s", err, out)
				}
				return string(out)
			}
			if got := run(); !strings.Contains(got, "first prompt") {
				t.Fatalf("first launch: %s", got)
			}
			got := run()
			want := "--continue\n"
			if rt == RuntimeCodex {
				want = "resume\n--last\n"
			}
			if !strings.Contains(got, want) || strings.Contains(got, "first prompt") {
				t.Fatalf("reopen: %s", got)
			}
		})
	}
}
