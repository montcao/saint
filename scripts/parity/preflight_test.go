package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execute actual framed commands only at the ordered submission boundary.
// Raw text admission and key events alone must never execute a command.
// A real agent's interpretation of key events still requires a Mac test.
func shellRunner(t *testing.T, failPane string, sent *[]string) runner {
	t.Helper()
	outputs := map[string]string{}
	pending := map[string]string{}
	return func(ctx context.Context, _ string, args ...string) (string, error) {
		if args[0] == "--version" {
			return "fixture", nil
		}
		pane := args[2]
		switch args[1] {
		case "send-text":
			pending[pane] += args[3]
		case "send-keys":
			// Model raw text still buffered when Enter arrives: transport
			// acknowledgement alone neither flushes nor executes it.
			if args[3] == "!" && pending[pane] != "" {
				return "", errors.New("input is not empty")
			}
		case "run":
			if !strings.HasPrefix(args[3], "sh -c ") {
				t.Fatal("unexpected command outside synthetic shell fixture")
			}
			if pending[pane] != "" {
				return "", errors.New("input is not empty")
			}
			*sent = append(*sent, pane)
			if pane == failPane {
				outputs[pane] = "private model prose"
				return "{}", nil
			}
			data, err := exec.CommandContext(ctx, "sh", "-c", args[3]).CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			outputs[pane] = string(data)
		case "read":
			return outputs[pane], nil
		}
		return "{}", nil
	}
}
func TestPreflightStopsAtFirstUnframedPane(t *testing.T) {
	for _, fail := range []string{"host", "saint", ""} {
		t.Run(fail, func(t *testing.T) {
			var sent []string
			d := driver{run: shellRunner(t, fail, &sent), timeout: time.Second, prefix: "!"}
			err := preflight(t.Context(), d, "host", "saint")
			if fail == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), fail+" preflight: framing:") || strings.Contains(err.Error(), "private") {
				t.Fatalf("%v", err)
			}
			want := 2
			if fail == "host" {
				want = 1
			}
			if len(sent) != want {
				t.Fatalf("submissions: %v", sent)
			}
		})
	}
}
func TestPreflightFailureSavesUnverifiedReportWithoutComparisons(t *testing.T) {
	for _, fail := range []string{"host", "saint", ""} {
		t.Run(fail, func(t *testing.T) {
			var sent []string
			var out, errs bytes.Buffer
			path := filepath.Join(t.TempDir(), "run.json")
			o := options{host: "host", saint: "saint", agent: "claude", timeout: time.Second, runs: 1, maxRatio: 1, prefix: "!", preflightOnly: fail == ""}
			code := runLive(t.Context(), o, path, &out, &errs, shellRunner(t, fail, &sent))
			wantCode, wantSent := 2, 2
			if fail == "host" {
				wantSent = 1
			}
			if fail == "" {
				wantCode = 0
			}
			if code != wantCode || len(sent) != wantSent {
				t.Fatalf("code=%d sent=%v stderr=%s", code, sent, &errs)
			}
			var r runReport
			if err := decodeFile(path, &r); err != nil {
				t.Fatal(err)
			}
			if err := validateReport(r); err != nil {
				t.Fatal(err)
			}
			for _, v := range r.Results {
				if v.Status != "NOT VERIFIED" {
					t.Fatalf("%+v", v)
				}
			}
			out.Reset()
			if err := evaluate(path, path+".observations.json", &out); !errors.Is(err, errIncomplete) {
				t.Fatalf("%v", err)
			}
			if !strings.Contains(out.String(), "Preflight:") || !strings.Contains(out.String(), "Score: 0/29") {
				t.Fatal(out.String())
			}
		})
	}
}
func TestProbeFailureStagesDoNotLeakRunnerErrors(t *testing.T) {
	stages := []string{"shell-mode key", "submit command", "wait-output", "read"}
	for failAt, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			d := driver{timeout: time.Second, prefix: "!", run: func(context.Context, string, ...string) (string, error) {
				calls++
				if calls == failAt+1 {
					return "private output", errors.New("secret argv")
				}
				return "{}", nil
			}}
			_, err := d.probe(t.Context(), "host", "true", "fixture")
			if err == nil || failureDetail(err) != stage+": command failed" || calls != failAt+1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}
func TestComparisonStopsAfterFramingFailure(t *testing.T) {
	for _, fail := range []string{"host", "saint"} {
		var sent []string
		d := driver{timeout: time.Second, run: shellRunner(t, fail, &sent)}
		results := compareProbes(t.Context(), d, "host", "saint", []probe{{"first", "true"}, {"second", "true"}})
		if results[0].Status != "FAIL" || !strings.Contains(results[0].Detail, fail+": framing:") || results[1].Status != "NOT VERIFIED" {
			t.Fatalf("%+v", results)
		}
		want := 2
		if fail == "host" {
			want = 1
		}
		if len(sent) != want {
			t.Fatalf("%v", sent)
		}
	}
}
func TestComparisonReportsBothExitFailuresAndContinues(t *testing.T) {
	var sent []string
	d := driver{timeout: time.Second, run: shellRunner(t, "", &sent)}
	results := compareProbes(t.Context(), d, "host", "saint", []probe{{"first", "exit 7"}, {"second", "true"}})
	if results[0].Detail != "host: exit: status 7; saint: exit: status 7" || results[1].Status != "PASS" || len(sent) != 4 {
		t.Fatalf("%+v %v", results, sent)
	}
}
func TestPreflightRejectsMatchingWrongOutput(t *testing.T) {
	var sent []string
	run := shellRunner(t, "", &sent)
	d := driver{timeout: time.Second, run: func(ctx context.Context, bin string, args ...string) (string, error) {
		text, err := run(ctx, bin, args...)
		if args[1] == "read" {
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "execution-") {
					lines[i] = "wrong"
				}
			}
			text = strings.Join(lines, "\n")
		}
		return text, err
	}}
	if err := preflight(t.Context(), d, "host", "saint"); err == nil || !strings.Contains(err.Error(), "output mismatch") || len(sent) != 1 {
		t.Fatalf("%v %v", err, sent)
	}
}

func TestFailedFirstPairPreventsFurtherCreation(t *testing.T) {
	var sent []string
	base := shellRunner(t, "host", &sent)
	splits := 0
	run := func(ctx context.Context, bin string, args ...string) (string, error) {
		if len(args) > 1 {
			switch args[1] {
			case "run":
				if args[3] == quote("claude") {
					return "{}", nil
				}
			case "split":
				splits++
				return `{"result":{"pane":{"pane_id":"host"}}}`, nil
			case "pane":
				return `{"result":{"plugin_pane":{"pane":{"pane_id":"saint"}}}}`, nil
			case "read":
				if len(sent) == 0 {
					return "ready", nil
				}
			}
		}
		return base(ctx, bin, args...)
	}
	var out, errs bytes.Buffer
	o := options{target: "target", repo: "/fixture", agent: "claude", ready: "ready", runs: 5, maxRatio: 1, timeout: time.Second, prefix: "!"}
	if code := runLive(t.Context(), o, "", &out, &errs, run); code != 2 || splits != 1 || len(sent) != 1 {
		t.Fatalf("code=%d splits=%d sent=%v errs=%s", code, splits, sent, &errs)
	}
}
func TestProbeRejectsRPCErrorAndClassifiesTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
		reason    string
	}{
		{"rpc", `{"error":{"message":"private server response"}}`, nil, "command failed"},
		{"deadline", "", context.DeadlineExceeded, "timed out"},
		{"cancel", "", context.Canceled, "canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := driver{timeout: time.Second, run: func(context.Context, string, ...string) (string, error) { calls++; return tc.raw, tc.err }}
			_, err := d.probe(t.Context(), "host", "true", "fixture")
			if err == nil || err.Error() != "submit command: "+tc.reason || calls != 1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}

func TestRawTextAndEnterDoNotExecuteBufferedCommand(t *testing.T) {
	var sent []string
	run := shellRunner(t, "", &sent)
	for _, args := range [][]string{
		{"pane", "send-keys", "host", "!"},
		{"pane", "send-text", "host", probeCommand("", "printf harmless", "fixture")},
		{"pane", "send-keys", "host", "Enter"},
	} {
		if _, err := run(t.Context(), "herdr", args...); err != nil {
			t.Fatal(err)
		}
	}
	output, err := run(t.Context(), "herdr", "pane", "read", "host")
	if err != nil || output != "" || len(sent) != 0 {
		t.Fatalf("executed unsubmitted text: %q %v %v", output, err, sent)
	}
	// A retry must not silently append to the still-pending input.
	if _, err := run(t.Context(), "herdr", "pane", "run", "host", probeCommand("", "true", "retry")); err == nil {
		t.Fatal("accepted pending input")
	}
}

func TestAcknowledgedSubmissionWithoutOutputStopsBeforeGuest(t *testing.T) {
	var submitted []string
	d := driver{timeout: time.Second, prefix: "!", run: func(_ context.Context, _ string, args ...string) (string, error) {
		if args[2] != "host" {
			t.Fatal("touched guest after failed host submission")
		}
		switch args[1] {
		case "run":
			submitted = append(submitted, args[2])
		case "wait-output":
			return "", context.DeadlineExceeded
		}
		return "{}", nil
	}}
	err := preflight(t.Context(), d, "host", "guest")
	if err == nil || !strings.Contains(err.Error(), "host preflight: wait-output: timed out") || len(submitted) != 1 {
		t.Fatalf("%v %v", err, submitted)
	}
}

func TestShellKeyFailurePreventsSubmission(t *testing.T) {
	calls := 0
	d := driver{timeout: time.Second, prefix: "!", run: func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if args[1] != "send-keys" || args[3] != "!" {
			t.Fatal("submitted without shell-mode trigger")
		}
		return "", errors.New("unavailable")
	}}
	if err := preflight(t.Context(), d, "host", "guest"); err == nil || calls != 1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}
