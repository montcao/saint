package main

import (
	"context"
	"encoding/json"
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
}

func (d driver) call(ctx context.Context, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.run(bounded, d.bin, args...)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Markers are assembled at execution, so command echo cannot satisfy wait-output.
// Probe output stays between unique markers; an explicit exit code prevents two
// identical failures from counting as parity.
func probeCommand(prefix, script, id string) string {
	body := "printf '\\n%s%s\\n' 'SAINT_' " + quote("PARITY_"+id+"_BEGIN") + "; ( " + script + " ); _saint_rc=$?; printf '\\n%s%s:%s\\n' 'SAINT_' " + quote("PARITY_"+id+"_END") + " \"$_saint_rc\""
	return prefix + "sh -c " + quote(body)
}
func (d driver) probe(ctx context.Context, pane, script, id string) (string, error) {
	if _, err := d.call(ctx, "pane", "send-text", pane, probeCommand(d.prefix, script, id)); err != nil {
		return "", err
	}
	if _, err := d.call(ctx, "pane", "send-keys", pane, "Enter"); err != nil {
		return "", err
	}
	end := "SAINT_PARITY_" + id + "_END:"
	if _, err := d.call(ctx, "pane", "wait-output", pane, "--match", end, "--source", "recent-unwrapped", "--lines", "1000", "--timeout", strconv.FormatInt(d.timeout.Milliseconds(), 10)); err != nil {
		return "", err
	}
	text, err := d.call(ctx, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "1000", "--format", "text")
	if err != nil {
		return "", err
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
				return "", fmt.Errorf("duplicate begin marker")
			}
			start = i + 1
			continue
		}
		if strings.HasPrefix(line, end) && start >= 0 {
			code, err := strconv.Atoi(strings.TrimPrefix(line, end))
			if err != nil || code != 0 {
				return "", fmt.Errorf("probe exit status is not zero")
			}
			return strings.TrimSuffix(strings.Join(lines[start:i], "\n"), "\n"), nil
		}
	}
	return "", fmt.Errorf("complete probe output not found")
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
	raw, err = d.call(ctx, "plugin", "pane", "open", "--plugin", "saint", "--entrypoint", "session", "--placement", "split", "--direction", "down", "--target-pane", o.target, "--no-focus", "--env", "SAINT_REPO="+o.repo, "--env", "SAINT_RUNTIME="+o.agent)
	if err != nil {
		return
	}
	saint, err = paneID(raw)
	if err != nil {
		return
	}
	for _, pane := range []string{host, saint} {
		_, err = d.call(ctx, "pane", "wait-output", pane, "--match", o.ready, "--source", "recent-unwrapped", "--timeout", strconv.FormatInt(o.timeout.Milliseconds(), 10))
		if err != nil {
			return
		}
	}
	return
}
