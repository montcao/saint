package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type probe struct{ name, script string }
type result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func defaultProbes() []probe {
	probes := []probe{
		{"TERM", `test -n "${TERM-}" && printf '%s\n' "$TERM"`},
		{"COLORTERM", `printf '%s\n' "${COLORTERM-}"`},
		{"UTF-8 text roundtrip", `printf '%s\n' '界 ─│┌┐└┘'`},
		{"repository path", `pwd -P`},
		{"git identity", `python3 -c 'import hashlib,subprocess; print(hashlib.sha256(b"\0".join(subprocess.check_output(["git","config","--get",k]).strip() for k in ["user.name","user.email"])).hexdigest())'`},
		{"Claude safe settings", `python3 -c 'import hashlib,json,pathlib; p=pathlib.Path.home()/".claude/settings.json"; s=json.loads(p.read_text()) if p.exists() else {}; safe={k:s[k] for k in ["model","outputStyle","permissions","enabledPlugins"] if k in s}; print(hashlib.sha256(json.dumps(safe,sort_keys=True).encode()).hexdigest())'`},
		{"CLAUDE.md", `python3 -c 'import hashlib,pathlib; p=pathlib.Path.home()/".claude/CLAUDE.md"; print(hashlib.sha256(p.read_bytes()).hexdigest() if p.exists() else "absent")'`},
		{"Claude skill names", `python3 -c 'import pathlib; p=pathlib.Path.home()/".claude/skills"; print("\n".join(sorted(x.name for x in p.iterdir())) if p.exists() else "absent")'`},
	}
	for _, tool := range []string{"git", "ssh", "rg", "curl", "less", "python3"} {
		version := "--version"
		if tool == "ssh" {
			version = "-V"
		}
		probes = append(probes, probe{tool + " executable", fmt.Sprintf("command -v %s >/dev/null && %s %s >/dev/null 2>&1 && printf 'available\\n'", tool, tool, version)})
	}
	return probes
}

// Screen text cannot prove these properties. Keep them in the denominator and
// label them explicitly instead of granting a pass for matching escape bytes.
var manualProbes = []string{
	"UTF-8 rendered width", "24-bit color rendering", "resize propagation",
	"Shift+Enter and Alt+arrow", "extended keyboard CSI-u", "scrollback and mouse selection",
	"OSC 52 clipboard", "title and bell", "git fetch and push through allowed remote",
	"Claude plugins in effect", "prompt latency p50/p95", "default-path dialog count",
	"pane-close conversation resume", "herdr restart conversation resume", "herdr update handoff",
}

// A preflight is execution evidence, not proof that the model never saw input.
// Stop at the first failure; do not send anything else to an uncertain pane.
func preflight(ctx context.Context, d driver, host, saint string) error {
	for _, side := range []struct{ name, pane string }{{"host", host}, {"saint", saint}} {
		id := nonce()
		want := "execution-" + id
		got, err := d.probe(ctx, side.pane, "printf '%s%s\\n' 'execution-' "+quote(id), id)
		if err != nil {
			return fmt.Errorf("%s preflight: %s; remaining submissions stopped", side.name, failureDetail(err))
		}
		if got != want {
			return fmt.Errorf("%s preflight: output mismatch; remaining submissions stopped", side.name)
		}
	}
	return nil
}
func unverifiedProbes(probes []probe, detail string) []result {
	results := make([]result, 0, len(probes))
	for _, p := range probes {
		results = append(results, result{p.name, "NOT VERIFIED", detail})
	}
	return results
}
func compareProbes(ctx context.Context, d driver, host, saint string, probes []probe) []result {
	results := make([]result, 0, len(probes))
	for i, p := range probes {
		id := nonce()
		a, ea := d.probe(ctx, host, p.script, id+"h")
		if lostExecution(ea) {
			results = append(results, result{p.name, "FAIL", "host: " + failureDetail(ea) + "; saint: not attempted"})
			return append(results, unverifiedProbes(probes[i+1:], "stopped after execution failure")...)
		}
		b, eb := d.probe(ctx, saint, p.script, id+"s")
		r := result{Name: p.name, Status: "PASS", Detail: "matching successful output"}
		if ea != nil || eb != nil {
			r.Status = "FAIL"
			parts := []string{}
			if ea != nil {
				parts = append(parts, "host: "+failureDetail(ea))
			}
			if eb != nil {
				parts = append(parts, "saint: "+failureDetail(eb))
			}
			r.Detail = strings.Join(parts, "; ")
		} else if a != b {
			r.Status, r.Detail = "FAIL", "outputs differ"
		}
		results = append(results, r)
		if lostExecution(eb) {
			return append(results, unverifiedProbes(probes[i+1:], "stopped after execution failure")...)
		}
	}
	return results
}

func passing(results []result) int {
	n := 0
	for _, r := range results {
		if r.Status == "PASS" {
			n++
		}
	}
	return n
}
func printResults(out io.Writer, results []result) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROBE\tRESULT\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, r.Status, strings.ReplaceAll(r.Detail, "\n", " "))
	}
	_ = w.Flush()
	if len(results) == 0 {
		fmt.Fprintln(out, "Score: unknown (no probes)")
		return
	}
	fmt.Fprintf(out, "Score: %d/%d (%.1f%%); NOT VERIFIED probes remain in the denominator\n", passing(results), len(results), 100*float64(passing(results))/float64(len(results)))
}
