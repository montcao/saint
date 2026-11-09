package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// DefaultNetwork is the network Apple's container runtime creates for itself.
const DefaultNetwork = "default"

// networkEntry mirrors the shape of `container network ls --format json`.
type networkEntry struct {
	ID     string `json:"id"`
	Status struct {
		IPv4Gateway string `json:"ipv4Gateway"`
		IPv4Subnet  string `json:"ipv4Subnet"`
	} `json:"status"`
}

// GatewayAddr reports the host's address on a container network's bridge.
//
// This is discovered, never hardcoded. The address is a property of the user's
// container runtime - a different machine, or a second network, gets a different
// subnet - so writing one down would work on exactly one host.
//
// It matters because a guest cannot reach the host on loopback: bridging host
// loopback into a container needs `container system dns create ... --localhost`,
// which requires admin privileges. The bridge gateway is the address a guest can
// actually reach, so it is where the proxy has to listen.
func GatewayAddr(ctx context.Context, containerPath, network string) (string, error) {
	if strings.TrimSpace(network) == "" {
		network = DefaultNetwork
	}

	// `network ls --format json`, not `network inspect --format`: --format
	// support is uneven on container 1.3.0 (`image inspect` does not accept it
	// at all), and the list form is verified working.
	stdout, _, err := runContainer(ctx, containerPath, "network", "ls", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("list container networks: %w", err)
	}

	return parseNetworkGateway(stdout, network)
}

// parseNetworkGateway extracts one network's gateway from `network ls` output.
func parseNetworkGateway(data []byte, network string) (string, error) {
	if strings.TrimSpace(network) == "" {
		network = DefaultNetwork
	}

	var entries []networkEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return "", fmt.Errorf("parse container network json: %w", err)
	}

	for _, entry := range entries {
		if entry.ID != network {
			continue
		}
		gateway := strings.TrimSpace(entry.Status.IPv4Gateway)
		if gateway == "" {
			return "", fmt.Errorf("network %q has no IPv4 gateway; is the container system started?", network)
		}
		if net.ParseIP(gateway) == nil {
			return "", fmt.Errorf("network %q reported an unparseable gateway %q", network, gateway)
		}
		return gateway, nil
	}

	return "", fmt.Errorf("container network %q not found", network)
}

// Gateway reports the host address on the given network using this sandbox's
// container binary.
func (a *AppleContainer) Gateway(ctx context.Context, network string) (string, error) {
	return GatewayAddr(ctx, a.containerPath, network)
}
