package sandbox

import (
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
