package main

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only synthetic shell commands execute here; each side gets an isolated PATH.
func dependencyRunner(t *testing.T, paths map[string]string, submitted *[]string) runner {
	t.Helper()
	for _, dir := range paths {
		if err := os.Symlink("/bin/sh", filepath.Join(dir, "sh")); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	pending, output := map[string]string{}, map[string]string{}
	return func(ctx context.Context, _ string, a ...string) (string, error) {
		pane := a[2]
		switch a[1] {
		case "send-text":
			pending[pane] = a[3]
		case "send-keys":
			if a[3] == "Enter" {
				*submitted = append(*submitted, pane+":"+pending[pane])
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", pending[pane])
				cmd.Env = []string{"PATH=" + paths[pane]}
				b, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatal(err)
				}
				output[pane] = string(b)
			}
		case "read":
			return output[pane], nil
		}
		return "{}", nil
	}
}
func TestDependencyOutcomesAndSkippedPayloads(t *testing.T) {
	for _, tc := range []struct{ name, probe, tool, script, status, detail string }{
		{"metadata missing", "Claude safe settings", "python3", "printf SHOULD_NOT_EXECUTE", "NOT VERIFIED", "missing dependencies: python3"},
		{"tool missing", "git executable", "git", "printf SHOULD_NOT_EXECUTE", "FAIL", "missing dependencies: git"},
		{"version failed", "git executable", "git", "exit 7", "FAIL", "exit: status 7"},
		{"payload 127 is not missing", "Claude safe settings", "python3", "exit 127", "FAIL", "exit: status 127"},
		{"mismatch", "Claude safe settings", "python3", "printf '%s' \"$PATH\"", "FAIL", "outputs differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := t.TempDir(), t.TempDir()
			if tc.name != "metadata missing" && tc.name != "tool missing" {
				for _, dir := range []string{host, guest} {
					if err := os.WriteFile(filepath.Join(dir, tc.tool), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			var sent []string
			d := driver{timeout: time.Second, run: dependencyRunner(t, map[string]string{"host": host, "guest": guest}, &sent)}
			got := compareProbes(t.Context(), d, "host", "guest", []probe{{tc.probe, tc.script}})
			if got[0].Status != tc.status || !strings.Contains(got[0].Detail, tc.detail) {
				t.Fatal(got)
			}
			if passing(got) != 0 {
				t.Fatal("failed or blocked check counted as pass")
			}
			if tc.name == "metadata missing" || tc.name == "tool missing" {
				for _, cmd := range sent {
					if strings.Contains(cmd, "SHOULD_NOT_EXECUTE") {
						t.Fatal("dependent payload submitted")
					}
				}
			}
			if strings.Contains(got[0].Detail, host) || strings.Contains(got[0].Detail, guest) {
				t.Fatal("private output leaked")
			}
		})
	}
}
func TestAvailabilityRequiresExactKnownRecords(t *testing.T) {
	deps := []string{"python3", "git"}
	missing, err := parseAvailability("missing:python3\npresent:git", deps)
	if err != nil || len(missing) != 1 || missing[0] != "python3" {
		t.Fatal(missing, err)
	}
	for _, bad := range []string{"", "present:python3", "present:git\npresent:python3", "present:python3\npresent:git\nextra", "present:python3\nmissing:private-path", "present:python3\npresent:git\x1b[0m"} {
		if _, err := parseAvailability(bad, deps); err == nil {
			t.Fatal("ambiguous availability accepted")
		}
	}
}
func TestUTF8ByteProbeAndStrictExpectedValue(t *testing.T) {
	od, err := exec.LookPath("od")
	if err != nil {
		t.Skip("od unavailable")
	}
	var p probe
	for _, v := range defaultProbes() {
		if v.name == "UTF-8 bytes" {
			p = v
		}
	}
	if p.name == "" {
		t.Fatal("byte probe missing")
	}
	dir := t.TempDir()
	if err := os.Symlink(od, filepath.Join(dir, "od")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", p.script)
	cmd.Env = []string{"PATH=" + dir}
	b, err := cmd.Output()
	if err != nil || !validUTF8Bytes(string(b)) {
		t.Fatal(string(b), err)
	}
	for _, bad := range []string{"", utf8Payload, "3f", hex.EncodeToString([]byte(utf8Payload)), string(b) + "00", strings.Replace(string(b), "e7", "ff", 1), string(b) + "\x1b[0m"} {
		if validUTF8Bytes(bad) {
			t.Fatal("altered bytes accepted", bad)
		}
	}
	// Both sides producing the same damaged bytes is still a failure.
	var sent []string
	d := driver{timeout: time.Second, run: dependencyRunner(t, map[string]string{"host": dir, "guest": dir}, &sent)}
	got := compareProbes(t.Context(), d, "host", "guest", []probe{{"UTF-8 bytes", "printf '3f'"}})
	if got[0].Status != "FAIL" || !strings.Contains(got[0].Detail, "host:") || !strings.Contains(got[0].Detail, "saint:") {
		t.Fatal(got)
	}
	r := fixtureReport()
	o := templateFor(r)
	for _, c := range o.Checks {
		if c.Name == "UTF-8 rendered width" && (c.HostSucceeded != nil || c.SaintSucceeded != nil || c.Equivalent != nil) {
			t.Fatal("byte result populated rendering evidence")
		}
	}
}
func TestLegacyReportRetainsRenderingSemantics(t *testing.T) {
	r := fixtureReport()
	r.Version = 1
	for i := range r.Results {
		if r.Results[i].Name == "UTF-8 bytes" {
			r.Results[i].Name = "UTF-8 text roundtrip"
			r.Results[i].Status = "FAIL"
			r.Results[i].Detail = "outputs differ"
		}
	}
	if err := validateReport(r); err != nil {
		t.Fatal(err)
	}
	o := templateFor(r)
	if o.Version != 1 {
		t.Fatal("legacy observation version changed")
	}
	got, err := applyObservations(r, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range got {
		if v.Name == "UTF-8 text roundtrip" && v.Status != "FAIL" {
			t.Fatal("historical result changed")
		}
	}
	r.Version = evidenceVersion
	if validateReport(r) == nil {
		t.Fatal("legacy probe mislabeled as byte measurement")
	}
}

func TestCompletedByteFailurePrecedesOtherSideMissing(t *testing.T) {
	for _, missingSide := range []string{"host", "guest"} {
		t.Run(missingSide, func(t *testing.T) {
			paths := map[string]string{"host": t.TempDir(), "guest": t.TempDir()}
			presentSide := "host"
			if missingSide == "host" {
				presentSide = "guest"
			}
			if err := os.WriteFile(filepath.Join(paths[presentSide], "od"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			var sent []string
			d := driver{timeout: time.Second, run: dependencyRunner(t, paths, &sent)}
			got := compareProbes(t.Context(), d, "host", "guest", []probe{{"UTF-8 bytes", "printf '3f'"}})
			if got[0].Status != "FAIL" || !strings.Contains(got[0].Detail, map[string]string{"host": "host", "guest": "saint"}[missingSide]+": missing dependencies: od") || !strings.Contains(got[0].Detail, map[string]string{"host": "host", "guest": "saint"}[presentSide]+": UTF-8 bytes differ from expected") {
				t.Fatal(got)
			}
			if strings.Contains(got[0].Detail, "3f") {
				t.Fatal("raw output retained")
			}
		})
	}
}
func TestOneSideMissingMetadataDoesNotCompareOutput(t *testing.T) {
	paths := map[string]string{"host": t.TempDir(), "guest": t.TempDir()}
	os.WriteFile(filepath.Join(paths["host"], "python3"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	var sent []string
	d := driver{timeout: time.Second, run: dependencyRunner(t, paths, &sent)}
	got := compareProbes(t.Context(), d, "host", "guest", []probe{{"CLAUDE.md", "printf 'private contents'"}})
	if got[0].Status != "NOT VERIFIED" || got[0].Detail != "saint: missing dependencies: python3" {
		t.Fatal(got)
	}
	for _, cmd := range sent {
		if strings.HasPrefix(cmd, "guest:") && strings.Contains(cmd, "private contents") {
			t.Fatal("blocked payload sent")
		}
	}
}
func TestMalformedAvailabilityStopsRemainingSubmissions(t *testing.T) {
	paths := map[string]string{"host": t.TempDir(), "guest": t.TempDir()}
	var sent []string
	base := dependencyRunner(t, paths, &sent)
	d := driver{timeout: time.Second, run: func(c context.Context, b string, a ...string) (string, error) {
		out, err := base(c, b, a...)
		if a[1] == "read" {
			out = strings.ReplaceAll(out, "missing:python3", "unexpected-private-output")
		}
		return out, err
	}}
	got := compareProbes(t.Context(), d, "host", "guest", []probe{{"CLAUDE.md", "true"}, {"next", "true"}})
	if len(sent) != 1 || got[0].Status != "FAIL" || got[1].Status != "NOT VERIFIED" || strings.Contains(got[0].Detail, "private") {
		t.Fatal(got, len(sent))
	}
}
