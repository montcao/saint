package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const builderCheckTimeout = 2 * time.Second

func builderResult(ctx context.Context, containerPath, appRoot string) Result {
	const name = "container builder"
	check, cancel := context.WithTimeout(ctx, builderCheckTimeout)
	output, err := run(check, containerPath, "builder", "status", "--quiet")
	cancel()
	if err != nil {
		return Result{Name: name, Detail: "state unknown", Fix: "container builder status"}
	}
	switch strings.TrimSpace(output) {
	case "":
		return Result{Name: name, OK: true, Detail: "no running builder"}
	case "buildkit":
		return Result{
			Name: name, Detail: "running; " + builderDiskUsage(ctx, appRoot),
			Fix: "container builder stop (when every build is finished)",
		}
	default:
		return Result{Name: name, Detail: "state unknown", Fix: "container builder status"}
	}
}

func builderDiskUsage(ctx context.Context, appRoot string) string {
	const unknown = "builder container disk usage unknown"
	if !filepath.IsAbs(appRoot) {
		return unknown
	}
	check, cancel := context.WithTimeout(ctx, builderCheckTimeout)
	defer cancel()
	output, err := run(check, "/usr/bin/du", "-sk", filepath.Join(appRoot, "containers", "buildkit"))
	if err != nil {
		return unknown
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return unknown
	}
	blocks, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return unknown
	}
	return fmt.Sprintf("builder container disk usage %d KiB", blocks)
}
