package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureReport() runReport {
	r := runReport{Version: 1, RunID: "fixture-run", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Herdr: "fixture", Container: "fixture", Agent: "claude", Host: "w:h", Saint: "w:s"}
	for _, p := range defaultProbes() {
		r.Results = append(r.Results, result{p.name, "PASS", "fixture only"})
	}
	for _, name := range manualProbes {
		r.Results = append(r.Results, result{name, "NOT VERIFIED", "fixture only"})
	}
	return r
}
func boolPtr(v bool) *bool { return &v }
func completedObservations(r runReport) observations {
	o := templateFor(r)
	for i := range o.Checks {
		o.Checks[i].HostSucceeded = boolPtr(true)
		o.Checks[i].SaintSucceeded = boolPtr(true)
		o.Checks[i].Equivalent = boolPtr(true)
		o.Checks[i].Evidence = "synthetic unit-test evidence"
	}
	return o
}
func TestObservationsCanCompleteButCannotHideFailures(t *testing.T) {
	r := fixtureReport()
	o := completedObservations(r)
	got, err := applyObservations(r, o)
	if err != nil || passing(got) != len(got) {
		t.Fatalf("%v %+v", err, got)
	}
	o.Checks[0].HostSucceeded = boolPtr(false)
	o.Checks[0].SaintSucceeded = boolPtr(false)
	got, err = applyObservations(r, o)
	if err != nil || passing(got) != len(got)-1 {
		t.Fatal("matching failures passed")
	}
	r.Results[0].Status = "FAIL"
	got, err = applyObservations(r, o)
	if err != nil || got[0].Status != "FAIL" {
		t.Fatal("automated failure lost")
	}
}
func TestObservationValidation(t *testing.T) {
	r := fixtureReport()
	tests := []struct {
		name   string
		modify func(*runReport, *observations)
	}{
		{"wrong run", func(_ *runReport, o *observations) { o.RunID = "other" }},
		{"duplicate", func(_ *runReport, o *observations) { o.Checks = append(o.Checks, o.Checks[0]) }},
		{"unknown", func(_ *runReport, o *observations) { o.Checks[0].Name = "invented" }},
		{"no evidence", func(_ *runReport, o *observations) { o.Checks[0].Evidence = " " }},
		{"partial", func(_ *runReport, o *observations) { o.Checks[0].Equivalent = nil }},
		{"auto override", func(r *runReport, o *observations) { o.Checks[0].Name = r.Results[0].Name }},
		{"missing probe", func(r *runReport, _ *observations) { r.Results = r.Results[1:] }},
		{"duplicate probe", func(r *runReport, _ *observations) { r.Results[0] = r.Results[1] }},
		{"invalid status", func(r *runReport, _ *observations) { r.Results[0].Status = "SKIP" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			copy := r
			copy.Results = append([]result(nil), r.Results...)
			o := completedObservations(copy)
			tc.modify(&copy, &o)
			if _, err := applyObservations(copy, o); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	got, err := applyObservations(r, templateFor(r))
	if err != nil || passing(got) == len(got) {
		t.Fatal("empty template passed")
	}
}
func TestEvidenceFilesAndOfflineEvaluation(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "run.json")
	observationsPath := filepath.Join(dir, "observations.json")
	r := fixtureReport()
	o := completedObservations(r)
	if err := writeNewJSON(reportPath, r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(reportPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report is not private")
	}
	if err := writeNewJSON(reportPath, r); err == nil {
		t.Fatal("overwrote prior report")
	}
	if err := writeNewJSON(observationsPath, o); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := mainRun([]string{"-evaluate", reportPath, "-observations", observationsPath}, &out, &errs)
	if code != 0 || !strings.Contains(out.String(), "100.0%") {
		t.Fatalf("%d %s %s", code, &out, &errs)
	}
	if strings.Contains(out.String(), "synthetic unit-test evidence") {
		t.Fatal("printed private evidence")
	}
	o.Checks[0].Equivalent = boolPtr(false)
	data, _ := json.Marshal(o)
	if err := os.WriteFile(observationsPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if code := mainRun([]string{"-evaluate", reportPath, "-observations", observationsPath}, &out, &errs); code != 1 {
		t.Fatalf("failure exit=%d", code)
	}
}
func TestStrictEvidenceJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	for _, data := range []string{`{"unknown":true}`, `{} {}`, strings.Repeat(" ", evidenceLimit+1)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		var o observations
		if err := decodeFile(path, &o); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}
func TestNearestRankTiming(t *testing.T) {
	values := []float64{50, 10, 30, 20, 40}
	if got, err := quantile(values, .5); err != nil || got != 30 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := quantile(values, .95); err != nil || got != 50 {
		t.Fatalf("%v %v", got, err)
	}
	if values[0] != 50 {
		t.Fatal("mutated input")
	}
	pairs := []latencyPair{{10, 20}, {20, 40}, {30, 60}, {40, 80}, {50, 100}}
	if timingResult(pairs, 2).Status != "PASS" || timingResult(pairs, 1).Status != "FAIL" || timingResult(pairs[:4], 2).Status != "NOT VERIFIED" {
		t.Fatal("timing scoring incorrect")
	}
	for _, v := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := quantile([]float64{v}, .5); err == nil {
			t.Fatal("invalid timing accepted")
		}
	}
}
func TestWaitReadyRejectsSuccessfulTimeoutResponse(t *testing.T) {
	d := driver{timeout: time.Second, run: func(_ context.Context, _ string, args ...string) (string, error) {
		return `{"result":{"matched":false}}`, nil
	}}
	if err := d.waitReady(t.Context(), "w:p", "ready marker"); err == nil {
		t.Fatal("false ready accepted")
	}
}
func TestEveryManualProbeHasProcedure(t *testing.T) {
	for _, name := range manualProbes {
		if len(procedures[name]) < 50 {
			t.Fatalf("no actionable procedure for %s", name)
		}
	}
}

func TestFailedOpenDoesNotCreateTimingSample(t *testing.T) {
	var timings []latencyPair
	d := driver{timeout: time.Second, timings: &timings, run: func(_ context.Context, _ string, args ...string) (string, error) {
		if args[1] == "split" {
			return `{"result":{"pane":{"pane_id":"w:h"}}}`, nil
		}
		return "not yet ready", nil
	}}
	host, guest, err := d.open(t.Context(), options{target: "w:p", repo: "/repo", agent: "claude", ready: "unique ready criterion"})
	if err == nil || host != "w:h" || guest != "" || len(timings) != 0 {
		t.Fatalf("failed open produced sample: %s %s %v %v", host, guest, err, timings)
	}
}
