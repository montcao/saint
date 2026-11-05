package usage

import "strings"

// ContextLimit reports a model's context window in tokens.
//
// This table becomes the denominator of a percentage the user reads as fact, so
// it is deliberately small and conservative. An unknown model reports ok=false
// and the UI shows raw token counts with no bar - a missing percentage is a
// smaller failure than a confident wrong one.
//
// Checked against Anthropic's published model documentation on 2026-08-26.
// Adding an entry is one line; do not add one without checking the number.
//
// OpenAI models are deliberately absent. Nobody has verified those windows for
// this table, and Codex sessions are better served by raw counts than by a
// guessed denominator.
var contextLimits = map[string]int64{
	// Every generally available Anthropic model ships a 200k window by default.
	// Some accept a larger one behind a beta header; a session using that will
	// exceed 100%, which renderers must treat as "window unknown" rather than
	// clamping to a full bar.
	"claude-opus-5":   200_000,
	"claude-sonnet-5": 200_000,
	"claude-haiku-4":  200_000,
	"claude-opus-4":   200_000,
	"claude-sonnet-4": 200_000,
	"claude-3-7":      200_000,
	"claude-3-5":      200_000,
	"claude-3-opus":   200_000,
}

func ContextLimit(model string) (int64, bool) {
	return contextLimitIn(contextLimits, model)
}

// contextLimitIn is the lookup, separated from the table so it can be tested
// without swapping a package-level map out from under concurrent tests.
func contextLimitIn(table map[string]int64, model string) (int64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return 0, false
	}

	// Longest prefix wins so a future family-specific entry overrides a broader
	// one rather than losing to map iteration order.
	var (
		best      int64
		bestMatch string
	)
	for prefix, limit := range table {
		if !strings.HasPrefix(model, prefix) {
			continue
		}
		if len(prefix) > len(bestMatch) {
			best, bestMatch = limit, prefix
		}
	}
	if bestMatch == "" {
		return 0, false
	}
	return best, true
}
