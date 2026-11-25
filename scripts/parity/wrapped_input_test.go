package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// This fixture contains only a synthetic command and its ANSI input box.
// Both visible and recent-unwrapped returned the same two application rows.
func TestWrappedPendingInputStopsSubmission(t *testing.T) {
	data, err := os.ReadFile("testdata/wrapped-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Screen string `json:"screen"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	const script = "printf '%s\\n' 'alpha' 'beta'"
	const id = "diag_host_dac1"
	command := probeCommand("", script, id)
	// A hypothetical unwrapped input remains subject to full exact comparison.
	exact := inputBox("\x1b[0m\x1b[38;2;253;93;177m!", "\x1b[0m"+command)
	if !inputMatches(exact, "!", command) {
		t.Fatal("exact single-row command rejected")
	}
	for _, tc := range []struct{ name, screen string }{
		{"captured ambiguous wrap", fixture.Screen},
		{"altered wrapped text", strings.Replace(fixture.Screen, "alpha", "other", 1)},
		{"missing wrapped text", strings.Replace(fixture.Screen, "alpha", "", 1)},
		{"extra wrapped text", strings.Replace(fixture.Screen, "alpha", "alpha extra", 1)},
		{"missing continuation", strings.Split(fixture.Screen, "\n  _saint_rc")[0]},
		{"genuine newline", inputBox("!", strings.Replace(command, "; (", ";\n (", 1))},
		{"ambiguous indentation", strings.Replace(fixture.Screen, "\n  _saint_rc", "\n _saint_rc", 1)},
		{"extra row", strings.Replace(fixture.Screen, "\n  _saint_rc", "\nextra\n  _saint_rc", 1)},
		{"altered unwrapped", strings.Replace(exact, "alpha", "other", 1)},
		{"missing unwrapped", strings.Replace(exact, "alpha", "", 1)},
		{"extra unwrapped", strings.Replace(exact, "alpha", "alpha extra", 1)},
		{"leading whitespace", strings.Replace(exact, "sh -c", " sh -c", 1)},
		{"trailing whitespace", strings.Replace(exact, command, command+" ", 1)},
		{"extra prompt separator", strings.Replace(exact, "! ", "!  ", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, shell := false, false
			d := driver{timeout: time.Millisecond, prefix: "!", run: func(_ context.Context, _ string, args ...string) (string, error) {
				if args[2] != "host" {
					t.Fatal("touched another pane")
				}
				switch args[1] {
				case "read":
					if entered {
						return tc.screen, nil
					}
					if shell {
						return inputBox("!", ""), nil
					}
					return inputBox("❯", ""), nil
				case "send-keys":
					if args[3] != "!" || shell {
						t.Fatal("submitted or retried uncertain input")
					}
					shell = true
				case "send-text":
					if entered || args[3] != command {
						t.Fatal("unexpected text")
					}
					entered = true
				default:
					t.Fatalf("unexpected call: %s", args[1])
				}
				return "", nil
			}}
			_, err := d.probe(t.Context(), "host", script, id)
			if !entered || err == nil || !strings.Contains(err.Error(), "pending input: timed out") {
				t.Fatalf("entered=%v err=%v", entered, err)
			}
		})
	}
}
