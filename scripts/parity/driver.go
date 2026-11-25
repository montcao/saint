package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
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

// Read back each input transition; transport acknowledgement is not readiness.
func (d driver) probe(ctx context.Context, pane, script, id string) (string, error) {
	command := probeCommand("", script, id)
	if d.prefix != "" {
		if err := d.waitInput(ctx, pane, "❯", ""); err != nil {
			return "", commandFailure("idle input", err)
		}
		if _, err := d.call(ctx, "pane", "send-keys", pane, d.prefix); err != nil {
			return "", commandFailure("shell-mode key", err)
		}
		if err := d.waitInput(ctx, pane, "!", ""); err != nil {
			return "", commandFailure("shell input", err)
		}
	}
	if _, err := d.call(ctx, "pane", "send-text", pane, command); err != nil {
		return "", commandFailure("enter command", err)
	}
	if d.prefix != "" {
		if err := d.waitInput(ctx, pane, "!", command); err != nil {
			return "", commandFailure("pending input", err)
		}
	}
	if _, err := d.call(ctx, "pane", "send-keys", pane, "Enter"); err != nil {
		return "", commandFailure("submit command", err)
	}
	end := "SAINT_PARITY_" + id + "_END:"
	if _, err := d.call(ctx, "pane", "wait-output", pane, "--match", end, "--source", "recent-unwrapped", "--lines", "1000", "--timeout", strconv.FormatInt(d.timeout.Milliseconds(), 10)); err != nil {
		return "", commandFailure("wait-output", err)
	}
	text, err := d.call(ctx, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "1000", "--format", "text")
	if err != nil {
		return "", commandFailure("read", err)
	}
	output, err := extract(text, id)
	if err != nil {
		return "", err
	}
	if d.prefix != "" {
		if err := d.waitInput(ctx, pane, "❯", ""); err != nil {
			return "", commandFailure("return to idle", err)
		}
	}
	return output, nil
}

var inputSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// inputMatches accepts only the observed bounded input box. Unknown layouts
// fail closed; a shell-mode hint elsewhere in history is never readiness.
func inputMatches(text, prompt, want string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var borders []int
	for i, line := range lines {
		v := strings.TrimSpace(inputSGR.ReplaceAllString(line, ""))
		if len([]rune(v)) >= 8 && strings.Trim(v, "─") == "" {
			borders = append(borders, i)
		}
	}
	if len(borders) < 2 {
		return false
	}
	a, b := borders[len(borders)-2], borders[len(borders)-1]
	if b != a+2 {
		return false
	}
	line := strings.TrimSpace(lines[a+1])
	// Observed shell prompt color; do not strip styling from input content.
	const shellColor = "\x1b[0m\x1b[38;2;253;93;177m"
	coloredShell := prompt == "!" && strings.HasPrefix(line, shellColor)
	if coloredShell {
		line = strings.TrimPrefix(line, shellColor)
	}
	if !strings.HasPrefix(line, prompt) {
		return false
	}
	rest := strings.TrimLeft(strings.TrimPrefix(line, prompt), " \u00a0")
	if want == "" {
		if rest == "" {
			return true
		}
		// Only the observed placeholder has this exact style.
		// Never infer emptiness from its words in plain text or other styling.
		const start, end = "\x1b[0m\x1b[2mTry \"", "\"\x1b[0m"
		if (prompt != "❯" && !coloredShell) || !strings.HasPrefix(rest, start) || !strings.HasSuffix(rest, end) {
			return false
		}
		label := strings.TrimSuffix(strings.TrimPrefix(rest, start), end)
		return label != "" && !strings.ContainsRune(label, '"') && strings.IndexFunc(label, unicode.IsControl) < 0
	}
	if coloredShell {
		rest = strings.TrimPrefix(rest, "\x1b[0m")
	}
	return rest == want
}

func (d driver) waitInput(ctx context.Context, pane, prompt, want string) error {
	bounded, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	for {
		text, err := d.call(bounded, "pane", "read", pane, "--source", "visible", "--lines", "1000", "--format", "ansi")
		if err != nil {
			return err
		}
		if inputMatches(text, prompt, want) {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return bounded.Err()
		case <-timer.C:
		}
	}
}

func extract(text, id string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	begin, end := "SAINT_PARITY_"+id+"_BEGIN", "SAINT_PARITY_"+id+"_END:"
	start, finish := -1, -1
	decorated := false
	var output []string
	code := -1
	for i, raw := range lines {
		line := raw
		isDecorated := strings.HasPrefix(raw, "  ⎿  ") || strings.HasPrefix(raw, "  ⎿  ")
		if isDecorated {
			if strings.HasPrefix(raw, "  ⎿  ") {
				line = strings.TrimPrefix(raw, "  ⎿  ")
			} else {
				line = strings.TrimPrefix(raw, "  ⎿  ")
			}
		}
		if line == begin {
			if start != -1 {
				return "", &probeFailure{"framing", "duplicate begin marker"}
			}
			start, decorated = i, isDecorated
			continue
		}
		if start < 0 {
			if strings.HasPrefix(strings.TrimPrefix(line, "     "), end) {
				return "", &probeFailure{"framing", "end before begin"}
			}
			continue
		}
		if isDecorated {
			if finish >= 0 && !strings.HasPrefix(line, end) {
				continue
			}
			return "", &probeFailure{"framing", "unexpected decorated line"}
		}
		if decorated && !isDecorated && raw != "" {
			if !strings.HasPrefix(raw, "     ") {
				if finish < 0 {
					return "", &probeFailure{"framing", "inconsistent decoration"}
				}
			} else {
				line = strings.TrimPrefix(raw, "     ")
			}
		}
		if line == begin {
			return "", &probeFailure{"framing", "duplicate begin marker"}
		}
		if strings.HasPrefix(line, end) {
			if finish != -1 {
				return "", &probeFailure{"framing", "duplicate end marker"}
			}
			value := strings.TrimPrefix(line, end)
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != value {
				return "", &probeFailure{"framing", "invalid exit status"}
			}
			finish, code = i, n
			continue
		}
		if finish < 0 {
			output = append(output, line)
		}
	}
	if start < 0 || finish < 0 {
		return "", &probeFailure{"framing", "complete probe output not found"}
	}
	if code != 0 {
		return "", &probeFailure{"exit", fmt.Sprintf("status %d", code)}
	}
	return strings.TrimSuffix(strings.Join(output, "\n"), "\n"), nil
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
