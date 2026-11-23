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
func TestDriverUsesSendKeysWaitAndRead(t *testing.T) {
	t.Parallel()
	var calls [][]string
	d := driver{bin: "herdr", timeout: time.Second, prefix: "!", run: func(ctx context.Context, bin string, args ...string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded command")
		}
		calls = append(calls, args)
		if args[1] == "read" {
			return "SAINT_PARITY_x_BEGIN\nvalue\n\nSAINT_PARITY_x_END:0\n", nil
		}
		return "{}", nil
	}}
	got, err := d.probe(t.Context(), "w:p", "printf value", "x")
	if err != nil || got != "value" {
		t.Fatalf("%q %v", got, err)
	}
	var verbs []string
	for _, call := range calls {
		verbs = append(verbs, call[1])
		if call[2] != "w:p" {
			t.Fatal("wrong pane")
		}
	}
	if !reflect.DeepEqual(verbs, []string{"send-text", "send-keys", "wait-output", "read"}) {
		t.Fatalf("%v", verbs)
	}
	if calls[1][3] != "Enter" {
		t.Fatal("probe not submitted")
	}
	if !strings.HasPrefix(calls[0][3], "!sh -c ") {
		t.Fatal("missing shell prefix")
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
