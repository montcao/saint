package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProbeCommandAndExtraction(t *testing.T) {
	t.Parallel()
	for _, script := range []string{`printf '%s\n' "a 'quoted' value"`, `printf '界\n'`, `printf ''`} {
		command := probeCommand("", script, "fixture")
		if strings.Contains(command, "SAINT_PARITY_fixture_END") {
			t.Fatal("echo could satisfy completion marker")
		}
		data, err := exec.CommandContext(t.Context(), "sh", "-c", command).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		got, err := extract("echoed: "+command+"\n"+string(data), "fixture")
		if err != nil {
			t.Fatal(err)
		}
		want, err := exec.CommandContext(t.Context(), "sh", "-c", script).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got != strings.TrimSuffix(string(want), "\n") {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}
func TestExtractionRejectsFailuresEchoAndMissingOutput(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		probeCommand("", "true", "x"),
		"SAINT_PARITY_x_BEGIN\nvalue\nSAINT_PARITY_x_END:1\n",
		"SAINT_PARITY_x_BEGIN\nvalue\n",
		"SAINT_PARITY_x_BEGIN\nSAINT_PARITY_x_BEGIN\nSAINT_PARITY_x_END:0\n",
		"SAINT_PARITY_other_BEGIN\nvalue\nSAINT_PARITY_other_END:0\n",
	} {
		if _, err := extract(text, "x"); err == nil {
			t.Fatalf("accepted incomplete/failed output: %q", text)
		}
	}
}
func TestDriverChecksEveryInputTransition(t *testing.T) {
	var calls []string
	var sent []string
	base := shellRunner(t, "", &sent)
	d := driver{timeout: time.Second, prefix: "!", run: func(ctx context.Context, bin string, args ...string) (string, error) {
		calls = append(calls, args[1])
		return base(ctx, bin, args...)
	}}
	got, err := d.probe(t.Context(), "host", "printf value", "fixture")
	if err != nil || got != "value" {
		t.Fatalf("%q %v", got, err)
	}
	want := []string{"read", "send-keys", "read", "send-text", "read", "send-keys", "wait-output", "read", "read"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%v", calls)
	}
}
func TestDriverStopsAfterCommandFailure(t *testing.T) {
	t.Parallel()
	calls := 0
	d := driver{timeout: time.Second, run: func(context.Context, string, ...string) (string, error) { calls++; return "", errors.New("offline") }}
	if _, err := d.probe(t.Context(), "p", "true", "x"); err == nil || calls != 1 {
		t.Fatal("continued after failure")
	}
}
func TestMatchingFailuresDoNotPass(t *testing.T) {
	t.Parallel()
	d := driver{timeout: time.Second, run: func(context.Context, string, ...string) (string, error) { return "", errors.New("missing command") }}
	results := compareProbes(t.Context(), d, "host", "guest", []probe{{"git", "git --version"}})
	if passing(results) != 0 || results[0].Status != "FAIL" {
		t.Fatalf("%+v", results)
	}
}
func TestScoreIncludesUnverified(t *testing.T) {
	var out bytes.Buffer
	printResults(&out, []result{{"one", "PASS", "ok"}, {"two", "FAIL", "different"}, {"three", "NOT VERIFIED", "manual"}})
	if !strings.Contains(out.String(), "Score: 1/3 (33.3%)") {
		t.Fatal(out.String())
	}
	out.Reset()
	printResults(&out, nil)
	if !strings.Contains(out.String(), "unknown") {
		t.Fatal("invented empty score")
	}
}
func TestPaneIDRequiresResponseEnvelope(t *testing.T) {
	if got, err := paneID(`{"result":{"pane":{"pane_id":"w:p"}}}`); err != nil || got != "w:p" {
		t.Fatalf("%s %v", got, err)
	}
	for _, raw := range []string{`w:p`, `{"result":{"pane_id":"-oops"}}`, `{"result":{"pane_id":"w:p"},"error":{"message":"failed"}}`} {
		if _, err := paneID(raw); err == nil {
			t.Fatal("invalid pane response accepted")
		}
	}
}
func TestValidatePaneSelection(t *testing.T) {
	valid := options{runs: 1, maxRatio: 1, host: "w:h", saint: "w:s", agent: "claude", timeout: time.Second}
	if err := validateOptions(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.saint = invalid.host
	if validateOptions(invalid) == nil {
		t.Fatal("same pane accepted")
	}
	invalid = valid
	invalid.saint = ""
	if validateOptions(invalid) == nil {
		t.Fatal("half pair accepted")
	}
	invalid = options{agent: "claude", timeout: time.Second}
	if validateOptions(invalid) == nil {
		t.Fatal("missing creation parameters accepted")
	}
}

func TestOpenUsesHerdrResponseShapes(t *testing.T) {
	var verbs []string
	d := driver{timeout: time.Second, run: func(_ context.Context, _ string, args ...string) (string, error) {
		verbs = append(verbs, strings.Join(args[:2], " "))
		switch args[1] {
		case "split":
			return `{"result":{"pane":{"pane_id":"w:h"}}}`, nil
		case "read":
			return "ready", nil
		case "pane":
			return `{"result":{"plugin_pane":{"pane":{"pane_id":"w:s"}}}}`, nil
		}
		return `{}`, nil
	}}
	host, guest, err := d.open(t.Context(), options{target: "w:p", repo: "/repo", agent: "claude", ready: "ready"})
	if err != nil || host != "w:h" || guest != "w:s" {
		t.Fatalf("%s %s %v", host, guest, err)
	}
	want := []string{"pane split", "pane run", "pane wait-output", "pane read", "plugin pane", "pane wait-output", "pane read"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("%v", verbs)
	}
}

func TestDecoratedOutputStrictlyBounded(t *testing.T) {
	for _, prefix := range []string{"  ⎿  ", "  ⎿  ", "  _ _"} {
		text := prefix + "SAINT_PARITY_x_BEGIN\n     first\n       second\n\n     SAINT_PARITY_x_END:0\n"
		got, err := extract(text, "x")
		if err != nil || got != "first\n  second" {
			t.Fatalf("%q %v", got, err)
		}
	}
	for _, text := range []string{
		"  ⎿  " + "  ⎿  SAINT_PARITY_x_BEGIN\n     a\n     SAINT_PARITY_x_END:0",
		"SAINT_PARITY_x_END:0\nSAINT_PARITY_x_BEGIN\na\nSAINT_PARITY_x_END:0",
		"SAINT_PARITY_x_BEGIN\na\n  ⎿  SAINT_PARITY_x_END:0",
		"  ⎿  SAINT_PARITY_x_BEGIN\na\n     SAINT_PARITY_x_END:0",
		"  ⎿  SAINT_PARITY_x_BEGIN\n     a\n     SAINT_PARITY_x_END:+0",
		"  ⎿  SAINT_PARITY_x_BEGIN\n     a\n     SAINT_PARITY_x_END:00",
		"SAINT_PARITY_x_BEGIN\na\nSAINT_PARITY_x_END:0\nSAINT_PARITY_x_END:0",
		"  ⎿  SAINT_PARITY_x_BEGIN\n     SAINT_PARITY_x_BEGIN\n     SAINT_PARITY_x_END:0",
		"  _ _  _ _SAINT_PARITY_x_BEGIN\n     a\n     SAINT_PARITY_x_END:0",
		"  _ _SAINT_PARITY_x_BEGIN\na\n     SAINT_PARITY_x_END:0",
		"  _ _SAINT_PARITY_x_BEGIN\n     a\n  _ _SAINT_PARITY_x_END:0",
		"quoted SAINT_PARITY_x_BEGIN\na\nSAINT_PARITY_x_END:0",
		"  ⎿  SAINT_PARITY_other_BEGIN\n     a\n     SAINT_PARITY_other_END:0",
	} {
		if _, err := extract(text, "x"); err == nil {
			t.Fatalf("accepted malformed frame %q", text)
		}
	}
}

func TestCompactProbePreservesFramingAndStatus(t *testing.T) {
	id := "0123456789abcdef01234567"
	for _, script := range []string{`printf "alpha\nbeta\n"`, `printf "execution-%s\n" "$0"`} {
		command := probeCommand("", script, id)
		// Synthetic 151-column viewport: prompt takes two cells. This budget is
		// not a runtime assumption; exact visible-input checks still gate Enter.
		if len(command) > 149 {
			t.Fatalf("compact diagnostic too long: %d", len(command))
		}
		if strings.Contains(command, "SAINT_PARITY_"+id+"_BEGIN") || strings.Contains(command, "SAINT_PARITY_"+id+"_END") {
			t.Fatal("echo contains complete marker")
		}
		data, err := exec.CommandContext(t.Context(), "sh", "-c", command).Output()
		if err != nil {
			t.Fatal(err)
		}
		got, err := extract(string(data), id)
		if err != nil || got == "" {
			t.Fatalf("output=%q err=%v", got, err)
		}
	}
	for _, script := range []string{"exit 7", "false", "exit 255"} {
		command := probeCommand("", script, id)
		data, err := exec.CommandContext(t.Context(), "sh", "-c", command).Output()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := extract(string(data), id); err == nil {
			t.Fatal("nonzero payload accepted")
		}
	}
}
