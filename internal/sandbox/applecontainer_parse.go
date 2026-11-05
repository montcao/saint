package sandbox

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type listEntry struct {
	ID            string `json:"id"`
	Configuration struct {
		ID       string            `json:"id"`
		Image    listImage         `json:"image"`
		Labels   map[string]string `json:"labels"`
		Mounts   []listMount       `json:"mounts"`
		Networks []struct {
			Network string `json:"network"`
		} `json:"networks"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

type listImage struct {
	Reference string `json:"reference"`
}

type listMount struct {
	Destination string   `json:"destination"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}

func ParseListJSON(data []byte) ([]Info, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}

	var entries []listEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse container ls json: %w", err)
	}

	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		name := firstNonEmpty(entry.ID, entry.Configuration.ID)
		paths := make([]string, 0, len(entry.Configuration.Mounts))
		for _, mount := range entry.Configuration.Mounts {
			if mount.Destination == "" || mount.Destination == "/saint" || mount.Destination == "/run" {
				continue
			}
			paths = append(paths, mount.Destination)
		}
		slices.Sort(paths)
		rt := runtimeFromMetadata(entry.Configuration.Labels, entry.Configuration.Image.Reference)
		infos = append(infos, Info{
			Name:    name,
			State:   stateFromString(entry.Status.State),
			Runtime: rt,
			Paths:   paths,
		})
	}

	return infos, nil
}

func runtimeFromMetadata(labels map[string]string, imageRef string) Runtime {
	if labels != nil {
		switch Runtime(labels["saint.runtime"]) {
		case RuntimeClaude:
			return RuntimeClaude
		case RuntimeCodex:
			return RuntimeCodex
		}
	}

	lower := strings.ToLower(imageRef)
	switch {
	case strings.Contains(lower, "claude"):
		return RuntimeClaude
	case strings.Contains(lower, "codex"):
		return RuntimeCodex
	default:
		return RuntimeUnknown
	}
}

func stateFromString(raw string) SandboxState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return StateRunning
	case "stopped", "exited", "created":
		return StateStopped
	default:
		return StateStopped
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
