package usage

import "testing"

func TestContextLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model     string
		wantLimit int64
		wantOK    bool
	}{
		{model: "claude-opus-5", wantLimit: 200_000, wantOK: true},
		{model: "claude-opus-5-20260101", wantLimit: 200_000, wantOK: true},
		{model: "CLAUDE-SONNET-5", wantLimit: 200_000, wantOK: true},
		{model: "claude-haiku-4-5-20251001", wantLimit: 200_000, wantOK: true},
		// Unknown models get no denominator rather than a guessed one.
		{model: "gpt-5", wantOK: false},
		{model: "some-future-model", wantOK: false},
		{model: "", wantOK: false},
		{model: "   ", wantOK: false},
	}

	for _, tc := range cases {
		limit, ok := ContextLimit(tc.model)
		if ok != tc.wantOK {
			t.Errorf("ContextLimit(%q) ok = %v, want %v", tc.model, ok, tc.wantOK)
			continue
		}
		if ok && limit != tc.wantLimit {
			t.Errorf("ContextLimit(%q) = %d, want %d", tc.model, limit, tc.wantLimit)
		}
	}
}

// A more specific entry must win regardless of map iteration order.
func TestContextLimitPrefersTheLongestPrefix(t *testing.T) {
	t.Parallel()

	// A local table, not the package one: swapping a global out from under
	// tests that run in parallel is a data race, and -race will say so.
	table := map[string]int64{
		"claude-":             111,
		"claude-opus-5":       222,
		"claude-opus-5-turbo": 333,
	}

	for model, want := range map[string]int64{
		"claude-haiku-9":            111,
		"claude-opus-5-20260101":    222,
		"claude-opus-5-turbo-later": 333,
	} {
		got, ok := contextLimitIn(table, model)
		if !ok || got != want {
			t.Errorf("contextLimitIn(%q) = %d (ok=%v), want %d", model, got, ok, want)
		}
	}
}
