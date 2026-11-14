package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

func TestTelemetryCommand(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runTelemetry(st, args, &out); err != nil {
			t.Fatalf("saint telemetry %v: %v", args, err)
		}
		return out.String()
	}
	off := func() bool {
		t.Helper()
		cfg, err := st.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.ClaudeTelemetryOff
	}

	if out := run(); !strings.Contains(out, "telemetry: on") {
		t.Errorf("default = %q, want on", out)
	}
	if out := run("off"); !strings.Contains(out, "telemetry: off") || !off() {
		t.Errorf("off = %q, saved off = %v", out, off())
	}
	if out := run("toggle"); !strings.Contains(out, "telemetry: on") || off() {
		t.Errorf("toggle from off = %q, saved off = %v", out, off())
	}
	if out := run("toggle"); !strings.Contains(out, "telemetry: off") || !off() {
		t.Errorf("toggle from on = %q, saved off = %v", out, off())
	}
	if out := run("on"); !strings.Contains(out, "telemetry: on") || off() {
		t.Errorf("on = %q, saved off = %v", out, off())
	}
	var out bytes.Buffer
	if err := runTelemetry(st, []string{"maybe"}, &out); err == nil {
		t.Error("an unknown argument was accepted")
	}
}

// Turning telemetry off refuses its hosts through the saved policy, ahead of
// a saved allow; turning it back on leaves only what the person saved.
func TestPolicyRefusesTelemetryHostsWhenOff(t *testing.T) {
	t.Parallel()

	cfg := store.Config{ProxyAllowHosts: []string{"downloads.claude.ai"}, ProxyDenyHosts: []string{"bad.example"}}
	if got := policyFromConfig(cfg).Deny; !slices.Equal(got, []string{"bad.example"}) {
		t.Errorf("telemetry on: deny = %v", got)
	}
	cfg.ClaudeTelemetryOff = true
	policy := policyFromConfig(cfg)
	for _, host := range proxy.ClaudeTelemetryHosts() {
		if !slices.Contains(policy.Deny, host) {
			t.Errorf("telemetry off: %s is not refused (deny = %v)", host, policy.Deny)
		}
	}
	if !slices.Contains(policy.Deny, "bad.example") {
		t.Errorf("telemetry off dropped a saved deny rule: %v", policy.Deny)
	}
}
