package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type driver struct {
	run     runner
	bin     string
	timeout time.Duration
	prefix  string
	timings *[]latencyPair
}

func (d driver) call(ctx context.Context, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	text, err := d.run(bounded, d.bin, args...)
	if bounded.Err() != nil {
		return "", bounded.Err()
	}
	// A successful process exit can still carry a herdr RPC error. Readback
	// is plain text and must never be interpreted as a response envelope.
	if len(args) > 1 && args[1] != "read" {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(text), &envelope) == nil && len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return "", errors.New("herdr RPC failed")
		}
	}
	return text, err
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Markers are assembled at execution, so command echo cannot satisfy wait-output.
// Probe output stays between unique markers; an explicit exit code prevents two
// identical failures from counting as parity.
func probeCommand(prefix, script, id string) string {
	body := "printf '\\n%s%s\\n' 'SAINT_' " + quote("PARITY_"+id+"_BEGIN") + "; ( " + script + " ); _saint_rc=$?; printf '\\n%s%s:%s\\n' 'SAINT_' " + quote("PARITY_"+id+"_END") + " \"$_saint_rc\""
	return prefix + "sh -c " + quote(body)
}

// Send the shell-mode trigger as a key event, not part of a bracketed paste.
func (d driver) probe(ctx context.Context, pane, script, id string) (string, error) {
	if d.prefix != "" {
		if _, err := d.call(ctx, "pane", "send-keys", pane, d.prefix); err != nil {
			return "", commandFailure("shell-mode key", err)
		}
	}
	if _, err := d.call(ctx, "pane", "send-text", pane, probeCommand("", script, id)); err != nil {
		return "", commandFailure("send-text", err)
	}
	if _, err := d.call(ctx, "pane", "send-keys", pane, "Enter"); err != nil {
		return "", commandFailure("submit Enter", err)
	}
	end := "SAINT_PARITY_" + id + "_END:"
	if _, err := d.call(ctx, "pane", "wait-output", pane, "--match", end, "--source", "recent-unwrapped", "--lines", "1000", "--timeout", strconv.FormatInt(d.timeout.Milliseconds(), 10)); err != nil {
		return "", commandFailure("wait-output", err)
	}
	text, err := d.call(ctx, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "1000", "--format", "text")
	if err != nil {
		return "", commandFailure("read", err)
	}
	return extract(text, id)
}
func extract(text, id string) (string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	begin := "SAINT_PARITY_" + id + "_BEGIN"
	end := "SAINT_PARITY_" + id + "_END:"
	lines := strings.Split(text, "\n")
	start := -1
	for i, line := range lines {
		if line == begin {
			if start != -1 {
				return "", &probeFailure{"framing", "duplicate begin marker"}
			}
			start = i + 1
			continue
		}
		if strings.HasPrefix(line, end) && start >= 0 {
			code, err := strconv.Atoi(strings.TrimPrefix(line, end))
			if err != nil || code < 0 || code > 255 {
				return "", &probeFailure{"framing", "invalid exit status"}
			}
			if code != 0 {
				return "", &probeFailure{"exit", fmt.Sprintf("status %d", code)}
			}
			return strings.TrimSuffix(strings.Join(lines[start:i], "\n"), "\n"), nil
		}
	}
	return "", &probeFailure{"framing", "complete probe output not found"}
}

// paneID accepts the documented JSON response envelope, never arbitrary text
// output that could cause later commands to target an unrelated pane.
func paneID(raw string) (string, error) {
	var envelope struct {
		Result struct {
			PaneID string `json:"pane_id"`
			Pane   struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
			PluginPane struct {
				Pane struct {
					PaneID string `json:"pane_id"`
				} `json:"pane"`
			} `json:"plugin_pane"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return "", err
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return "", fmt.Errorf("herdr returned an error")
	}
	id := envelope.Result.PaneID
	if id == "" {
		id = envelope.Result.Pane.PaneID
	}
	if id == "" {
		id = envelope.Result.PluginPane.Pane.PaneID
	}
	if id == "" || strings.ContainsAny(id, " \t\n\r") || strings.HasPrefix(id, "-") {
		return "", fmt.Errorf("herdr response has no valid pane ID")
	}
	return id, nil
}
func (d driver) open(ctx context.Context, o options) (host, saint string, err error) {
	start := time.Now()
	raw, err := d.call(ctx, "pane", "split", o.target, "--direction", "right", "--cwd", o.repo, "--no-focus")
	if err != nil {
		return
	}
	host, err = paneID(raw)
	if err != nil {
		return
	}
	_, err = d.call(ctx, "pane", "run", host, quote(o.agent))
	if err != nil {
		return
	}
	if err = d.waitReady(ctx, host, o.ready); err != nil {
		return
	}
	hostMS := elapsedMS(start)
	start = time.Now()
	raw, err = d.call(ctx, "plugin", "pane", "open", "--plugin", "saint", "--entrypoint", "session", "--placement", "split", "--direction", "down", "--target-pane", o.target, "--no-focus", "--env", "SAINT_REPO="+o.repo, "--env", "SAINT_RUNTIME="+o.agent)
	if err != nil {
		return
	}
	saint, err = paneID(raw)
	if err != nil {
		return
	}
	if err = d.waitReady(ctx, saint, o.ready); err != nil {
		return
	}
	if d.timings != nil {
		*d.timings = append(*d.timings, latencyPair{hostMS, elapsedMS(start)})
	}
	return
}

func (d driver) waitReady(ctx context.Context, pane, ready string) error {
	if _, err := d.call(ctx, "pane", "wait-output", pane, "--match", ready, "--source", "recent-unwrapped", "--timeout", strconv.FormatInt(d.timeout.Milliseconds(), 10)); err != nil {
		return err
	}
	// Do not trust an exit code alone: some CLI versions return a JSON timeout
	// result successfully. Confirm that the observed screen contains the marker.
	text, err := d.call(ctx, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "1000", "--format", "text")
	if err != nil {
		return err
	}
	if !strings.Contains(text, ready) {
		return fmt.Errorf("pane did not reach the configured ready marker")
	}
	return nil
}

// Only fixed stages/reasons and a validated exit code reach reports. Never
// include runner errors, argv, or pane output, which may contain private data.
type probeFailure struct{ stage, reason string }

func (e *probeFailure) Error() string { return e.stage + ": " + e.reason }
func failureDetail(err error) string {
	var e *probeFailure
	if errors.As(err, &e) {
		return e.Error()
	}
	return "unknown probe failure"
}
func lostExecution(err error) bool {
	var e *probeFailure
	return err != nil && (!errors.As(err, &e) || e.stage != "exit")
}

func commandFailure(stage string, err error) error {
	reason := "command failed"
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "timed out"
	}
	if errors.Is(err, context.Canceled) {
		reason = "canceled"
	}
	return &probeFailure{stage, reason}
}
