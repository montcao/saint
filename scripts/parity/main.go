// Command parity compares probes through host and saint herdr panes on a Mac.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type runner func(context.Context, string, ...string) (string, error)

func execute(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(data), nil
}

type options struct {
	host, saint, target, repo, agent, ready, prefix, herdr string
	timeout                                                time.Duration
}

func main() { os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr)) }
func mainRun(args []string, out, errs io.Writer) int {
	var o options
	fs := flag.NewFlagSet("parity", flag.ContinueOnError)
	fs.SetOutput(errs)
	fs.StringVar(&o.host, "host-pane", "", "existing idle host agent pane (paired with -saint-pane)")
	fs.StringVar(&o.saint, "saint-pane", "", "existing idle saint agent pane")
	fs.StringVar(&o.target, "target-pane", os.Getenv("HERDR_PANE_ID"), "pane beside which to open two test panes")
	fs.StringVar(&o.repo, "repo", "", "repository path for newly opened panes")
	fs.StringVar(&o.agent, "agent", "claude", "claude or codex")
	fs.StringVar(&o.ready, "ready", "", "literal ready text to wait for in newly opened agent panes")
	fs.StringVar(&o.prefix, "shell-prefix", "!", "agent shell-mode prefix; use empty only for shell panes")
	fs.StringVar(&o.herdr, "herdr", "herdr", "herdr executable")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "timeout per pane operation")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(errs, "parity requires a real Mac with Apple container and herdr; baseline not recorded")
		return 2
	}
	if err := validateOptions(o); err != nil {
		fmt.Fprintln(errs, err)
		return 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := driver{run: execute, bin: o.herdr, timeout: o.timeout, prefix: o.prefix}
	for _, bin := range []string{o.herdr, "container"} {
		bounded, done := context.WithTimeout(ctx, o.timeout)
		version, err := execute(bounded, bin, "--version")
		done()
		if err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		fmt.Fprintf(out, "%s: %s\n", bin, strings.TrimSpace(version))
	}
	fmt.Fprintf(out, "UTC: %s; agent: %s; comparisons do not establish visual or lifecycle parity\n", time.Now().UTC().Format(time.RFC3339), o.agent)
	if o.host == "" {
		host, saint, err := d.open(ctx, o)
		if host != "" {
			fmt.Fprintf(out, "created host pane: %s (retained for inspection)\n", host)
		}
		if saint != "" {
			fmt.Fprintf(out, "created saint pane: %s (retained for inspection)\n", saint)
		}
		if err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		o.host, o.saint = host, saint
	}
	fmt.Fprintf(out, "host pane: %s; saint pane: %s\n", o.host, o.saint)
	results := compareProbes(ctx, d, o.host, o.saint, defaultProbes())
	for _, name := range manualProbes {
		results = append(results, result{name: name, status: "NOT VERIFIED", detail: "requires observed Mac baseline"})
	}
	printResults(out, results)
	if passing(results) != len(results) {
		return 1
	}
	return 0
}

func validateOptions(o options) error {
	if o.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if o.agent != "claude" && o.agent != "codex" {
		return errors.New("agent must be claude or codex")
	}
	if (o.host == "") != (o.saint == "") {
		return errors.New("provide both host-pane and saint-pane")
	}
	if o.host != "" {
		if o.host == o.saint {
			return errors.New("host and saint panes must differ")
		}
		return nil
	}
	if o.target == "" || !filepath.IsAbs(o.repo) || o.ready == "" {
		return errors.New("opening panes requires target-pane, absolute repo and ready text; alternatively provide both existing pane IDs")
	}
	return nil
}

func nonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
