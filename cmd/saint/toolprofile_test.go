package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHerdrProfileIsExplicitAndForwarded(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "herdr")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", helper)
	t.Setenv("CAPTURE", capture)
	t.Setenv("HERDR_PLUGIN_CONTEXT_JSON", `{"focused_pane_id":"w1:p1","focused_pane_cwd":"/disposable/repo"}`)
	ref := "/private/tmp/reviewed.json#" + strings.Repeat("a", 64)
	t.Setenv(envProfile, ref)
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"claude"}, envProfile + "=\n"}, {[]string{"claude", "--experimental-profile", ref}, envProfile + "=" + ref + "\n"}} {
		if err := runHerdrOpen(context.Background(), tc.args); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Fatalf("missing explicit profile handoff: %s", b)
		}
	}
	if err := runHerdrOpen(context.Background(), []string{"claude", "--experimental-profile", "raw"}); err == nil {
		t.Fatal("unverified profile accepted")
	}
}
