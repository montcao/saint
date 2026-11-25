package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const evidenceVersion = 1
const evidenceLimit = 1 << 20

type runReport struct {
	InstalledHostClaudeVersion string        `json:"installed_host_claude_version,omitempty"`
	HostAgentVersion           string        `json:"observed_host_agent_version,omitempty"`
	SaintAgentVersion          string        `json:"observed_saint_agent_version,omitempty"`
	Version                    int           `json:"version"`
	RunID                      string        `json:"run_id"`
	Created                    time.Time     `json:"created_utc"`
	Herdr                      string        `json:"herdr_version"`
	Container                  string        `json:"container_version"`
	Agent                      string        `json:"agent"`
	Host                       string        `json:"host_pane"`
	Saint                      string        `json:"saint_pane"`
	Results                    []result      `json:"results"`
	Timings                    []latencyPair `json:"startup_samples,omitempty"`
}

type observation struct {
	Name           string `json:"name"`
	HostSucceeded  *bool  `json:"host_succeeded"`
	SaintSucceeded *bool  `json:"saint_succeeded"`
	Equivalent     *bool  `json:"equivalent"`
	Evidence       string `json:"evidence"`
	Procedure      string `json:"procedure"`
}
type observations struct {
	Version int           `json:"version"`
	RunID   string        `json:"run_id"`
	Checks  []observation `json:"checks"`
}

// The procedure stays with the result so an observer knows what a pass means.
// Null booleans in a generated template are unknown, never implicit passes.
var procedures = map[string]string{
	"UTF-8 rendered width":                      "In both agent shell modes print '界|──|'. Observe the wide character occupying two cells, aligned separators and intact box drawing. Record terminal/font and both outcomes.",
	"24-bit color rendering":                    "In both panes run printf '\033[38;2;12;140;230mRGB\033[0m\n'. Observe the same intended color in the outer terminal; text readback alone cannot establish this. Record the terminal's true-color support.",
	"resize propagation":                        "Run stty size in each agent shell mode, resize that pane through herdr, then run stty size again. Record before/after dimensions and that the agent redraw matches the new pane size on both sides; equal initial sizes alone are insufficient.",
	"Shift+Enter and Alt+arrow":                 "In an unsent multiline agent input, use herdr pane send-keys <pane> Shift+Enter and Alt+Left on both sides. Record the resulting line break and cursor movement. Clear the test input without submitting a model prompt.",
	"extended keyboard CSI-u":                   "With each agent's extended keyboard mode active, send the same modified keys through herdr pane send-keys and record decoded keys and visible editing behavior. Check the mode in each agent; identical missing support is a failure.",
	"scrollback and mouse selection":            "Print numbered lines exceeding the viewport in both agent shell modes. Scroll up in herdr and select/copy earlier lines with the mouse. Record whether the selected text is intact and whether agent output remains selectable.",
	"OSC 52 clipboard":                          "Use a dedicated clipboard: in each pane run printf '\033]52;c;c2FpbnQtcGFyaXR5\007'. Paste into a scratch host editor and verify saint-parity. Record permission prompts and results; restore the prior clipboard afterward.",
	"title and bell":                            "In each pane run printf '\033]0;saint-parity\007\007'. Observe herdr's title and configured bell response, then restore the title. Record configuration and both outcomes; screen text is insufficient.",
	"git fetch and push through allowed remote": "Use a disposable repository and authorized remote with a unique test branch. In each pane fetch and push that branch, verify it remotely, then remove only the test branch. Record auth mode, egress decisions and evidence that no private key entered the guest. Both operations must succeed; do not use production refs.",
	"Claude plugins in effect":                  "Use the same explicitly configured harmless plugin/skill in both agents and exercise a known behavior. Record plugin/version and observed behavior, not merely matching files. For another runtime record its equivalent configured extension behavior.",
	"prompt latency p50/p95":                    "For existing panes, record at least five fresh open-to-ready timings for each runtime path, the ready criterion and nearest-rank p50/p95 in milliseconds. Mark equivalent only if both quantiles meet the declared comparison budget; report the budget and raw samples. Automatically timed new-pane runs use their own results instead.",
	"default-path dialog count":                 "On a configured install open fresh host and saint agent panes. Count every onboarding, key approval, trust and other dialog before the ready prompt. Record both counts; pass only if both are zero. Do not count a setup flow as a default-path pass.",
	"pane-close conversation resume":            "In dedicated test sessions create a disposable conversation marker and guest file, close the panes, and reopen the same sessions. Record whether the marker conversation and file resume; do not infer survival from an existing layout.",
	"herdr restart conversation resume":         "On a dedicated herdr server with no unrelated work, save conversation/file markers, restart the server, and check both restored panes. Record whether automatic restoration or manual reattach was required and whether conversations continued. A plain restored shell is not automatic conversation resume.",
	"herdr update handoff":                      "On a dedicated herdr server with no unrelated work, exercise herdr update --handoff when an update is available. Record versions, pane processes, files and conversation continuity for both paths. If no update was exercised, leave this unverified.",
}

func templateFor(r runReport) observations {
	o := observations{Version: evidenceVersion, RunID: r.RunID}
	for _, v := range r.Results {
		if procedure, ok := procedures[v.Name]; ok && v.Status == "NOT VERIFIED" {
			o.Checks = append(o.Checks, observation{Name: v.Name, Procedure: procedure})
		}
	}
	return o
}

func decodeFile(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > evidenceLimit {
		return errors.New("evidence file exceeds 1 MiB")
	}
	dec := json.NewDecoder(io.LimitReader(f, evidenceLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("evidence must contain exactly one JSON document")
	}
	return nil
}
func writeNewJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

func validateReport(r runReport) error {
	if r.Version != evidenceVersion || r.RunID == "" || r.Created.IsZero() || r.Herdr == "" || r.Container == "" || r.Host == "" || r.Saint == "" || r.Host == r.Saint {
		return errors.New("incomplete or unsupported run report")
	}
	expected := map[string]bool{}
	for _, p := range defaultProbes() {
		expected[p.name] = true
	}
	for _, name := range manualProbes {
		expected[name] = true
	}
	seen := map[string]bool{}
	for _, v := range r.Results {
		if !expected[v.Name] || seen[v.Name] {
			return fmt.Errorf("unknown or duplicate probe %q", v.Name)
		}
		if v.Status != "PASS" && v.Status != "FAIL" && v.Status != "NOT VERIFIED" {
			return fmt.Errorf("invalid result status for %s", v.Name)
		}
		seen[v.Name] = true
	}
	if len(seen) != len(expected) {
		return errors.New("report omits probes; cannot score a reduced denominator")
	}
	return nil
}
func applyObservations(r runReport, o observations) ([]result, error) {
	if err := validateReport(r); err != nil {
		return nil, err
	}
	if o.Version != evidenceVersion || o.RunID != r.RunID {
		return nil, errors.New("observations must belong to this run ID and schema version")
	}
	results := append([]result(nil), r.Results...)
	index := map[string]int{}
	for i, v := range results {
		index[v.Name] = i
	}
	seen := map[string]bool{}
	for _, check := range o.Checks {
		i, ok := index[check.Name]
		if !ok || seen[check.Name] {
			return nil, fmt.Errorf("unknown or duplicate observation %q", check.Name)
		}
		seen[check.Name] = true
		if _, ok := procedures[check.Name]; !ok || results[i].Status != "NOT VERIFIED" {
			return nil, fmt.Errorf("observation cannot override measured result %q", check.Name)
		}
		if check.HostSucceeded == nil && check.SaintSucceeded == nil && check.Equivalent == nil {
			continue
		}
		if check.HostSucceeded == nil || check.SaintSucceeded == nil || check.Equivalent == nil || strings.TrimSpace(check.Evidence) == "" {
			return nil, fmt.Errorf("incomplete observation for %q", check.Name)
		}
		status := "FAIL"
		if *check.HostSucceeded && *check.SaintSucceeded && *check.Equivalent {
			status = "PASS"
		}
		results[i] = result{check.Name, status, "operator observation (see evidence file)"}
	}
	return results, nil
}
func evaluate(reportPath, observationPath string, out io.Writer) error {
	var r runReport
	var o observations
	if err := decodeFile(reportPath, &r); err != nil {
		return err
	}
	if err := decodeFile(observationPath, &o); err != nil {
		return err
	}
	results, err := applyObservations(r, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Run %s, recorded %s; herdr %s; container %s\n", r.RunID, r.Created.Format(time.RFC3339), r.Herdr, r.Container)
	printResults(out, results)
	if passing(results) != len(results) {
		return errIncomplete
	}
	return nil
}

var errIncomplete = errors.New("parity has failed or unverified probes")
