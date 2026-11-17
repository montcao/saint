package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseNetworkGateway(t *testing.T) {
	t.Parallel()

	// Verbatim shape of `container network ls --format json` on container 1.3.0.
	const listJSON = `[{"configuration":{"mode":"nat","name":"default"},"id":"default","status":{"ipv4Gateway":"192.168.64.1","ipv4Subnet":"192.168.64.0/24"}},{"configuration":{"name":"saint"},"id":"saint","status":{"ipv4Gateway":"192.168.128.1","ipv4Subnet":"192.168.128.0/24"}}]`

	cases := []struct {
		name    string
		json    string
		network string
		want    string
		wantErr string
	}{
		{name: "default network", json: listJSON, network: "default", want: "192.168.64.1"},
		{name: "named network", json: listJSON, network: "saint", want: "192.168.128.1"},
		{name: "empty means default", json: listJSON, network: "", want: "192.168.64.1"},
		{name: "unknown network", json: listJSON, network: "nope", wantErr: "not found"},
		{
			name:    "gateway missing",
			json:    `[{"id":"default","status":{"ipv4Subnet":"192.168.64.0/24"}}]`,
			network: "default",
			wantErr: "no IPv4 gateway",
		},
		{
			name:    "gateway unparseable",
			json:    `[{"id":"default","status":{"ipv4Gateway":"not-an-ip"}}]`,
			network: "default",
			wantErr: "unparseable gateway",
		},
		{name: "empty list", json: `[]`, network: "default", wantErr: "not found"},
		{name: "bad json", json: `{`, network: "default", wantErr: "parse container network json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseNetworkGateway([]byte(tc.json), tc.network)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("error = nil, want one containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("gateway = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeNetworkCLI is a container CLI that keeps a list of networks in a file,
// answers `network ls --format json` from it and appends every argv to a log.
func fakeNetworkCLI(t *testing.T, existing ...string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	state := filepath.Join(dir, "networks")
	if err := os.WriteFile(state, []byte(strings.Join(append(existing, ""), "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "container")
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1 $2" in
"network ls")
  printf '['; sep=''
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    printf '%s{"id":"%s","status":{"ipv4Gateway":"192.0.2.1"}}' "$sep" "$n"; sep=','
  done < ` + state + `
  printf ']'
  ;;
"network create")
  for last; do :; done
  echo "$last" >> ` + state + `
  ;;
"network delete")
  grep -v -x "$3" ` + state + ` > ` + state + `.new; mv ` + state + `.new ` + state + `
  ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A session's network is created internal, so its guest has no route out but
// the proxy, and its gateway is read back from the runtime.
func TestCreateNetworkIsInternal(t *testing.T) {
	t.Parallel()

	bin, log := fakeNetworkCLI(t)
	gateway, err := NewAppleContainer(bin, nil).CreateNetwork(context.Background(), "saint-abc123")
	if err != nil {
		t.Fatalf("CreateNetwork() error = %v", err)
	}
	if gateway != "192.0.2.1" {
		t.Errorf("gateway = %q", gateway)
	}
	if !slices.Contains(calls(t, log), "network create --internal --label saint.session=abc123 saint-abc123") {
		t.Errorf("calls = %q, want an internal network create", calls(t, log))
	}
}

// Recreating a session's container reuses its network, so the guest keeps the
// address it was given.
func TestCreateNetworkReusesAnExistingOne(t *testing.T) {
	t.Parallel()

	bin, log := fakeNetworkCLI(t, "saint-abc123")
	if _, err := NewAppleContainer(bin, nil).CreateNetwork(context.Background(), "saint-abc123"); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls(t, log) {
		if strings.HasPrefix(call, "network create") {
			t.Errorf("created a network that already existed: %q", call)
		}
	}
}

// Removing a session twice, or after a partial failure, must not fail on a
// network that is already gone.
func TestRemoveNetworkIsRepeatable(t *testing.T) {
	t.Parallel()

	bin, log := fakeNetworkCLI(t, "saint-abc123")
	sb := NewAppleContainer(bin, nil)
	for range 2 {
		if err := sb.RemoveNetwork(context.Background(), "saint-abc123"); err != nil {
			t.Fatalf("RemoveNetwork() error = %v", err)
		}
	}
	deletes := 0
	for _, call := range calls(t, log) {
		if call == "network delete saint-abc123" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("deleted %d times, want once: %q", deletes, calls(t, log))
	}
}

// Network names reach the CLI from sessions.json, so they are validated the way
// container names are.
func TestNetworkNamesAreValidated(t *testing.T) {
	t.Parallel()

	bin, _ := fakeNetworkCLI(t)
	sb := NewAppleContainer(bin, nil)
	for _, name := range []string{"default", "--all", "saint-"} {
		if _, err := sb.CreateNetwork(context.Background(), name); err == nil {
			t.Errorf("CreateNetwork(%q) accepted", name)
		}
		if err := sb.RemoveNetwork(context.Background(), name); err == nil {
			t.Errorf("RemoveNetwork(%q) accepted", name)
		}
	}
	bin2, _ := fakeContainer(t)
	err := NewAppleContainer(bin2, nil).Create(context.Background(), "saint-abc123", RuntimeClaude,
		[]Repo{{Path: t.TempDir()}}, t.TempDir(), "", CreateOptions{Network: "--privileged"})
	if err == nil {
		t.Error("Create accepted an unvalidated network name")
	}
}

// The container joins the session's network, and its init waits, bounded, for
// the proxy to say it listens there before starting the agent.
func TestCreateRunsOnTheSessionNetwork(t *testing.T) {
	t.Parallel()

	argv := createArgv(t, CreateOptions{Network: "saint-abc123", ProxyEgress: true, ProxyToken: "t"})
	i := slices.Index(argv, "--network")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "saint-abc123" {
		t.Fatalf("argv has no --network saint-abc123: %q", argv)
	}
	script := bootstrapOf(argv)
	wait := strings.Index(script, "/saint/"+ProxyReadyFile)
	agent := strings.Index(script, "tmux new-session")
	if wait < 0 || agent < 0 || wait > agent {
		t.Fatalf("bootstrap does not wait for %s before starting the agent:\n%s", ProxyReadyFile, script)
	}
	if !strings.Contains(script, "-lt 150") {
		t.Errorf("the wait for the proxy is not bounded:\n%s", script)
	}

	plain := createArgv(t, CreateOptions{})
	if slices.Contains(plain, "--network") || strings.Contains(bootstrapOf(plain), ProxyReadyFile) {
		t.Error("a session without its own network waits for a proxy listener nothing will announce")
	}
}

// bootstrapOf is the init script in a recorded argv: everything after
// `sh -lc`, rejoined because the recording splits it on newlines.
func bootstrapOf(argv []string) string {
	i := slices.Index(argv, "-lc")
	if i < 0 {
		return ""
	}
	return strings.Join(argv[i+1:], "\n")
}
