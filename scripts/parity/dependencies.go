package main

import (
	"context"
	"encoding/hex"
	"strings"
)

const utf8Payload = "界 ─│┌┐└┘"

// Dependencies are fixed probe metadata, never command text from a pane.
func dependenciesFor(name string) []string {
	switch name {
	case "git identity":
		return []string{"python3", "git"}
	case "Claude safe settings", "CLAUDE.md", "Claude skill names":
		return []string{"python3"}
	case "UTF-8 bytes":
		return []string{"od"}
	}
	if tool := executableTool(name); tool != "" {
		return []string{tool}
	}
	return nil
}
func executableTool(name string) string {
	for _, tool := range []string{"git", "ssh", "rg", "curl", "less", "python3"} {
		if name == tool+" executable" {
			return tool
		}
	}
	return ""
}
func availabilityScript(deps []string) string {
	var b strings.Builder
	for _, tool := range deps {
		b.WriteString("if command -v " + quote(tool) + " >/dev/null 2>&1; then printf '%s\\n' " + quote("present:"+tool) + "; else printf '%s\\n' " + quote("missing:"+tool) + "; fi; ")
	}
	return b.String()
}
func parseAvailability(output string, deps []string) ([]string, error) {
	lines := strings.Split(output, "\n")
	if len(lines) != len(deps) {
		return nil, &probeFailure{"dependencies", "invalid availability response"}
	}
	var missing []string
	for i, tool := range deps {
		switch lines[i] {
		case "present:" + tool:
		case "missing:" + tool:
			missing = append(missing, tool)
		default:
			return nil, &probeFailure{"dependencies", "invalid availability response"}
		}
	}
	return missing, nil
}

type sideProbe struct {
	invalidBytes bool
	output       string
	missing      []string
	err          error
}

func measureSide(ctx context.Context, d driver, pane string, p probe, id string) sideProbe {
	if deps := dependenciesFor(p.name); len(deps) > 0 {
		output, err := d.probe(ctx, pane, availabilityScript(deps), id+"d")
		if err != nil {
			return sideProbe{err: err}
		}
		missing, err := parseAvailability(output, deps)
		if err != nil || len(missing) > 0 {
			return sideProbe{missing: missing, err: err}
		}
	}
	output, err := d.probe(ctx, pane, p.script, id)
	return sideProbe{output: output, err: err, invalidBytes: err == nil && p.name == "UTF-8 bytes" && !validUTF8Bytes(output)}
}
func sideDetail(side string, s sideProbe) string {
	if s.err != nil {
		return side + ": " + failureDetail(s.err)
	}
	if len(s.missing) > 0 {
		return side + ": missing dependencies: " + strings.Join(s.missing, ", ")
	}
	if s.invalidBytes {
		return side + ": UTF-8 bytes differ from expected"
	}
	return ""
}

// Only hexadecimal byte pairs and ASCII whitespace from od are accepted.
// Matching wrong output must never pass, even if both renderers damage it alike.
func validUTF8Bytes(output string) bool {
	for _, c := range output {
		if !(c == ' ' || c == '\t' || c == '\n' || c == '\r' || c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	fields := strings.Fields(output)
	for _, f := range fields {
		if len(f) != 2 {
			return false
		}
	}
	return strings.Join(fields, "") == hex.EncodeToString([]byte(utf8Payload))
}
