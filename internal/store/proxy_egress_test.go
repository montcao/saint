package store

import (
	"encoding/json"
	"os"
	"testing"
)

// Off by default, and off for every config file written before the field
// existed. Both matter: turning a guest's traffic through a recording proxy is
// not something an upgrade may do on a user's behalf, and an install whose
// proxy is not serving forward traffic would have every clone in every session
// start failing at once.
func TestProxyGuestEgressDefaultsOff(t *testing.T) {
	t.Parallel()

	if DefaultConfig().ProxyGuestEgressOrDefault() {
		t.Error("a fresh config reroutes guest traffic through saint without being asked")
	}
	if (Config{}).ProxyGuestEgressOrDefault() {
		t.Error("an absent proxy_guest_egress must mean off")
	}
}

// A config file from before this feature parses, and parses as off.
func TestExistingConfigFileParsesAsEgressOff(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	if err := st.Ensure(); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	const old = `{
  "default_runtime": "claude",
  "default_memory_gb": 2,
  "default_cpus": 2,
  "proxy_port": 8787,
  "proxy_bind": "auto",
  "proxy_auth": "auto",
  "proxy_log_requests": true
}`
	if err := os.WriteFile(st.Paths().ConfigPath, []byte(old), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := st.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.ProxyGuestEgressOrDefault() {
		t.Error("a config written before proxy_guest_egress existed came back with it on")
	}
}

// Turning it on has to survive a save/load round trip, or a user would find the
// setting silently reverted.
func TestProxyGuestEgressRoundTrips(t *testing.T) {
	t.Parallel()

	st := New(t.TempDir())
	cfg, err := st.EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	cfg.ProxyGuestEgress = true
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	loaded, err := st.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !loaded.ProxyGuestEgressOrDefault() {
		t.Error("proxy_guest_egress did not survive a save and load")
	}

	data, err := os.ReadFile(st.Paths().ConfigPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("the saved config is not valid json: %v", err)
	}
	if raw["proxy_guest_egress"] != true {
		t.Errorf("config.json holds proxy_guest_egress = %v, want true", raw["proxy_guest_egress"])
	}
}
