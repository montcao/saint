package store

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
)

func readConfigFile(t *testing.T, st *Store) map[string]any {
	t.Helper()
	data, err := os.ReadFile(st.Paths().ConfigPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return raw
}

func writeConfigFile(t *testing.T, st *Store, raw map[string]any) {
	t.Helper()
	if err := st.Ensure(); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(st.Paths().ConfigPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// Every guest has the proxy URL, port included, written into its environment
// at creation, so the port chosen for a state directory must survive restarts.
func TestEnsureConfigChoosesProxyPortOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := New(dir).EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	if !validProxyPort(first.ProxyPort) {
		t.Fatalf("ProxyPort = %d, want a usable port", first.ProxyPort)
	}

	// A fresh Store on the same directory stands in for a later process.
	st := New(dir)
	again, err := st.EnsureConfig()
	if err != nil {
		t.Fatalf("second EnsureConfig() error = %v", err)
	}
	if again.ProxyPort != first.ProxyPort {
		t.Fatalf("ProxyPort changed from %d to %d between loads", first.ProxyPort, again.ProxyPort)
	}
	if got := readConfigFile(t, st)["proxy_port"]; got != float64(first.ProxyPort) {
		t.Fatalf("config.json proxy_port = %v, want %d", got, first.ProxyPort)
	}
}

// A second state directory must not be handed the port the first copy's proxy
// is listening on.
func TestEnsureConfigAvoidsAPortInUse(t *testing.T) {
	t.Parallel()

	first, err := New(t.TempDir()).EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(first.ProxyPort)))
	if err != nil {
		t.Skipf("cannot hold port %d for the test: %v", first.ProxyPort, err)
	}
	defer ln.Close()

	second, err := New(t.TempDir()).EnsureConfig()
	if err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	if second.ProxyPort == first.ProxyPort {
		t.Fatalf("second state directory got port %d, which the first is using", second.ProxyPort)
	}
}

// A port the user wrote, including the old fixed default an existing install
// already has on disk, is kept as it is.
func TestEnsureConfigKeepsConfiguredPort(t *testing.T) {
	t.Parallel()

	for _, port := range []int{8787, 9999} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			t.Parallel()

			st := New(t.TempDir())
			writeConfigFile(t, st, map[string]any{"proxy_port": port})
			cfg, err := st.EnsureConfig()
			if err != nil {
				t.Fatalf("EnsureConfig() error = %v", err)
			}
			if cfg.ProxyPort != port {
				t.Fatalf("ProxyPort = %d, want %d", cfg.ProxyPort, port)
			}
			if got := readConfigFile(t, st)["proxy_port"]; got != float64(port) {
				t.Fatalf("config.json proxy_port = %v, want %d", got, port)
			}
		})
	}
}

// A config without a usable port would otherwise bind port 0, an ephemeral
// port no guest could find. It gets a chosen port, written back.
func TestEnsureConfigFillsMissingPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  map[string]any
	}{
		{name: "absent", raw: map[string]any{"default_runtime": "claude"}},
		{name: "zero", raw: map[string]any{"proxy_port": 0}},
		{name: "negative", raw: map[string]any{"proxy_port": -1}},
		{name: "too large", raw: map[string]any{"proxy_port": 70000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := New(t.TempDir())
			writeConfigFile(t, st, tc.raw)
			cfg, err := st.EnsureConfig()
			if err != nil {
				t.Fatalf("EnsureConfig() error = %v", err)
			}
			if !validProxyPort(cfg.ProxyPort) {
				t.Fatalf("ProxyPort = %d, want a usable port", cfg.ProxyPort)
			}
			if got := readConfigFile(t, st)["proxy_port"]; got != float64(cfg.ProxyPort) {
				t.Fatalf("config.json proxy_port = %v, want %d", got, cfg.ProxyPort)
			}
		})
	}
}

// A pane and the proxy it starts can both create the config on a fresh state
// directory. Whoever loses must adopt the winner's port, or the pane would hand
// guests a port the proxy is not listening on.
func TestEnsureConfigConcurrentFirstRunAgrees(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const n = 8
	ports := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := New(dir).EnsureConfig()
			ports[i], errs[i] = cfg.ProxyPort, err
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("EnsureConfig() #%d error = %v", i, errs[i])
		}
		if ports[i] != ports[0] {
			t.Fatalf("ports disagree: %v", ports)
		}
	}
}
