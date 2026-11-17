package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

const telemetryUsage = "usage: saint telemetry [on|off|toggle]"

// runTelemetry shows or changes whether Claude Code's own background traffic
// (metrics, error reports, the auto-updater, other non-essential requests) is
// allowed out of a session.
//
//	saint telemetry          say which it is
//	saint telemetry off      refuse it: the proxy denies its hosts for every
//	                         session at once, and new Claude sessions start
//	                         with Claude Code's own switches for it set
//	saint telemetry on       allow it again
//	saint telemetry toggle   flip it, which is what the herdr action runs
func runTelemetry(st *store.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		cfg, err := st.EnsureConfig()
		if err != nil {
			return err
		}
		fmt.Fprint(out, telemetryState(cfg.ClaudeTelemetryOff))
		return nil
	}
	if len(args) != 1 {
		return errors.New(telemetryUsage)
	}
	var set func(bool) bool
	switch args[0] {
	case "on":
		set = func(bool) bool { return false }
	case "off":
		set = func(bool) bool { return true }
	case "toggle":
		set = func(off bool) bool { return !off }
	default:
		return errors.New(telemetryUsage)
	}
	cfg, err := st.UpdateConfig(func(cfg *store.Config) error {
		cfg.ClaudeTelemetryOff = set(cfg.ClaudeTelemetryOff)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprint(out, telemetryState(cfg.ClaudeTelemetryOff))
	return nil
}

// telemetryState says what is in force and what a change does to sessions
// that are already running, because the two halves of it apply differently.
func telemetryState(off bool) string {
	hosts := strings.Join(proxy.ClaudeTelemetryHosts(), ", ")
	if off {
		return "Claude Code telemetry: off\n" +
			"  Refused for every session from the next request: " + hosts + "\n" +
			"  New Claude sessions start with telemetry, error reporting, the auto-updater\n" +
			"  and non-essential traffic switched off. Turn it back on with saint telemetry on.\n"
	}
	return "Claude Code telemetry: on\n" +
		"  Allowed for every session: " + hosts + "\n" +
		"  Turn it off with saint telemetry off.\n"
}

// runHerdrTelemetry is the herdr action: it flips the setting and says the
// result in a herdr notification, since an action has no pane to print to.
func runHerdrTelemetry(ctx context.Context, st *store.Store) error {
	cfg, err := st.UpdateConfig(func(cfg *store.Config) error {
		cfg.ClaudeTelemetryOff = !cfg.ClaudeTelemetryOff
		return nil
	})
	if err != nil {
		return err
	}
	title, body := telemetryNotification(cfg.ClaudeTelemetryOff)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, herdrBinary(), "notification", "show", title, "--body", body)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func telemetryNotification(off bool) (title, body string) {
	if off {
		return "saint: Claude Code telemetry off",
			"Its hosts are refused for every session now. New Claude sessions start with it switched off."
	}
	return "saint: Claude Code telemetry on", "Its hosts are allowed for every session again."
}
