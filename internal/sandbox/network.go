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

// Each session runs on a network of its own, created with --internal.
//
// Measured (docs/research/spikes.md): a guest on an internal network has no
// route out and no resolver, and a CONNECT through a proxy on the host side of
// its bridge works. So the proxy is the guest's only way out, and the host
// policy the proxy applies is a real boundary rather than a convention a guest
// can ignore. One network per session, not one per install, because
// containers on the same network can reach each other.

// SessionNetworkLabel marks the networks saint created, so a person reading
// `container network ls` can tell them from their own.
const SessionNetworkLabel = "saint.session"

// networkExists reports whether a network is in `network ls` output, and its
// gateway when it has one.
func networkExists(data []byte, name string) (bool, error) {
	var entries []networkEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return false, fmt.Errorf("parse container network json: %w", err)
	}
	for _, entry := range entries {
		if entry.ID == name {
			return true, nil
		}
	}
	return false, nil
}

// CreateNetwork creates a session's internal network and returns the host's
// address on it. A network that already exists is reused, so recreating a
// session's container keeps the address its guest was given.
//
// The gateway is known as soon as the network exists: `network ls` reports it
// from configuration. The address itself only comes up on the host once a
// container is attached, which is why the proxy binds it after the container
// starts rather than here.
func (a *AppleContainer) CreateNetwork(ctx context.Context, name string) (string, error) {
	if err := ValidateContainerName(name); err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}
	stdout, _, err := a.run(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("list container networks: %w", err)
	}
	exists, err := networkExists(stdout, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if _, _, err := a.run(ctx, "network", "create", "--internal",
			"--label", SessionNetworkLabel+"="+strings.TrimPrefix(name, "saint-"), name); err != nil {
			return "", fmt.Errorf("create network %s: %w", name, err)
		}
		if stdout, _, err = a.run(ctx, "network", "ls", "--format", "json"); err != nil {
			return "", fmt.Errorf("list container networks: %w", err)
		}
	}
	return parseNetworkGateway(stdout, name)
}

// RemoveNetwork deletes a session's network. A network that is already gone is
// not an error: removing a session must be repeatable after a partial failure.
// The container has to be removed first; the runtime refuses to delete a
// network a container is attached to.
func (a *AppleContainer) RemoveNetwork(ctx context.Context, name string) error {
	if err := ValidateContainerName(name); err != nil {
		return fmt.Errorf("remove network: %w", err)
	}
	stdout, _, err := a.run(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return fmt.Errorf("list container networks: %w", err)
	}
	exists, err := networkExists(stdout, name)
	if err != nil || !exists {
		return err
	}
	if _, _, err := a.run(ctx, "network", "delete", name); err != nil {
		return fmt.Errorf("delete network %s: %w", name, err)
	}
	return nil
}
