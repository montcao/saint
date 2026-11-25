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

// Model input transitions and execute only after an inspected pending command
// receives Enter. This models a synthetic UI, never real agent routing.
func inputBox(prompt, text string) string {
	return "────────\n" + prompt + " " + text + "\n────────\n"
}
func shellRunner(t *testing.T, failPane string, sent *[]string) runner {
	t.Helper()
	outputs, pending, modes := map[string]string{}, map[string]string{}, map[string]string{}
	return func(ctx context.Context, _ string, args ...string) (string, error) {
		if args[0] == "--version" {
			return "fixture", nil
		}
		pane := args[2]
		if modes[pane] == "" {
			modes[pane] = "❯"
		}
		switch args[1] {
		case "send-text":
			pending[pane] += args[3]
		case "send-keys":
			if args[3] == "!" {
				if pending[pane] != "" {
					return "", errors.New("input is not empty")
				}
				modes[pane] = "!"
			} else if args[3] == "Enter" {
				if !strings.HasPrefix(pending[pane], "sh -c ") {
					t.Fatal("unexpected synthetic command")
				}
				*sent = append(*sent, pane)
				if pane == failPane {
					outputs[pane] = "private model prose"
				} else {
					data, err := exec.CommandContext(ctx, "sh", "-c", pending[pane]).CombinedOutput()
					if err != nil {
						t.Fatal(err)
					}
					outputs[pane] = string(data)
				}
				pending[pane], modes[pane] = "", "❯"
			}
		case "read":
			return outputs[pane] + inputBox(modes[pane], pending[pane]), nil
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
				if len(sent) == 0 && strings.Contains(strings.Join(args, " "), "recent-unwrapped") {
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
			if err == nil || err.Error() != "enter command: "+tc.reason || calls != 1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}

func TestInputChecksFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, screen string }{
		{"pending", inputBox("❯", "do something")},
		{"wrong mode", inputBox("!", "")},
		{"history hint", "! for shell mode\n"},
		{"multiline", "────────\n❯ one\ntwo\n────────\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := driver{timeout: time.Millisecond, prefix: "!", run: func(_ context.Context, _ string, args ...string) (string, error) {
				if args[1] != "read" {
					t.Fatal("submitted into uncertain input")
				}
				calls++
				return tc.screen, nil
			}}
			err := preflight(t.Context(), d, "host", "guest")
			if err == nil || !strings.Contains(err.Error(), "idle input: timed out") || calls == 0 {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestTransitionFailureStopsBeforeSubmission(t *testing.T) {
	for _, stage := range []string{"shell input", "pending input", "return to idle"} {
		t.Run(stage, func(t *testing.T) {
			var sent []string
			base := shellRunner(t, "", &sent)
			triggered, entered, submitted := false, false, false
			d := driver{timeout: 100 * time.Millisecond, prefix: "!", run: func(ctx context.Context, bin string, args ...string) (string, error) {
				if args[2] != "host" {
					t.Fatal("touched guest")
				}
				if args[1] == "send-keys" && args[3] == "!" {
					triggered = true
				}
				if args[1] == "send-text" {
					entered = true
				}
				if args[1] == "send-keys" && args[3] == "Enter" {
					submitted = true
				}
				if args[1] == "read" && ((stage == "shell input" && triggered) || (stage == "pending input" && entered) || (stage == "return to idle" && submitted)) {
					if stage == "return to idle" {
						text, err := base(ctx, bin, args...)
						return strings.ReplaceAll(text, inputBox("❯", ""), inputBox("❯", "pending")), err
					}
					return inputBox("❯", "uncertain"), nil
				}
				return base(ctx, bin, args...)
			}}
			err := preflight(t.Context(), d, "host", "guest")
			if err == nil || !strings.Contains(err.Error(), stage+": timed out") {
				t.Fatalf("%v", err)
			}
			if stage != "return to idle" && submitted {
				t.Fatal("submitted after failed transition")
			}
		})
	}
}

func TestInputObservationUsesVisibleStateAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	d := driver{timeout: time.Second, run: func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if args[1] != "read" || args[4] != "visible" {
			t.Fatal("readiness used history or mutated pane")
		}
		cancel()
		return "unknown layout", nil
	}}
	if err := d.waitInput(ctx, "host", "❯", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestInputObservationWaitsForExactPendingCommand(t *testing.T) {
	calls := 0
	d := driver{timeout: time.Second, run: func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return inputBox("!", "partial"), nil
		}
		return inputBox("!", "printf complete"), nil
	}}
	if err := d.waitInput(t.Context(), "host", "!", "printf complete"); err != nil || calls != 2 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestTransitionErrorsRemainSanitized(t *testing.T) {
	stages := []string{"idle input", "shell-mode key", "shell input", "enter command", "pending input", "submit command", "wait-output", "read", "return to idle"}
	for failAt, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			var sent []string
			base := shellRunner(t, "", &sent)
			calls := 0
			d := driver{timeout: time.Second, prefix: "!", run: func(ctx context.Context, bin string, args ...string) (string, error) {
				calls++
				if calls == failAt+1 {
					return "private screen", errors.New("private argv")
				}
				return base(ctx, bin, args...)
			}}
			_, err := d.probe(t.Context(), "host", "printf harmless", "fixture")
			if err == nil || failureDetail(err) != stage+": command failed" || calls != failAt+1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}
