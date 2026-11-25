package main

import (
	"strings"
	"testing"

	"github.com/montcao/saint/internal/store"
)

// Sharing starts off, and turning it on says what the guest will be able to
// read and send on, at the place the choice is made.
func TestClaudeConfigIsOffUntilTurnedOnAndStatesWhatItShares(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	var out strings.Builder
	if err := runClaudeConfig(st, nil, &out); err != nil {
		t.Fatalf("runClaudeConfig() error = %v", err)
	}
	if !strings.Contains(out.String(), ": off") {
		t.Errorf("default state = %q, want off", out.String())
	}

	out.Reset()
	if err := runClaudeConfig(st, []string{"on"}, &out); err != nil {
		t.Fatalf("runClaudeConfig(on) error = %v", err)
	}
	cfg, err := st.EnsureConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ShareClaudeConfig {
		t.Error("on did not turn sharing on")
	}
	for _, want := range []string{"read-only copy", "deny rules only", "send what it reads out", "git name and email", "Never shared"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("turning sharing on does not say %q:\n%s", want, out.String())
		}
	}

	if err := runClaudeConfig(st, []string{"off"}, &out); err != nil {
		t.Fatalf("runClaudeConfig(off) error = %v", err)
	}
	if cfg, _ := st.EnsureConfig(); cfg.ShareClaudeConfig {
		t.Error("off did not turn sharing off")
	}
	if err := runClaudeConfig(st, []string{"maybe"}, &out); err == nil {
		t.Error("an unknown argument was accepted")
	}
}
