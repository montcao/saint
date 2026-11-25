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
	"math"
	"os"
	"os/exec"
	"os/signal"
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
	runs                                                   int
	maxRatio                                               float64
	preflightOnly                                          bool
}

func main() { os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr)) }
func mainRun(args []string, out, errs io.Writer) int {
	var o options
	var reportPath, observationsPath, evaluatePath string
	fs := flag.NewFlagSet("parity", flag.ContinueOnError)
	fs.SetOutput(errs)
	fs.BoolVar(&o.preflightOnly, "preflight-only", false, "check execution on one pane pair without running comparisons")
	fs.StringVar(&o.host, "host-pane", "", "existing idle host agent pane (paired with -saint-pane)")
	fs.StringVar(&o.saint, "saint-pane", "", "existing idle saint agent pane")
	fs.StringVar(&o.target, "target-pane", os.Getenv("HERDR_PANE_ID"), "pane beside which to open two test panes")
	fs.StringVar(&o.repo, "repo", "", "repository path for newly opened panes")
	fs.StringVar(&o.agent, "agent", "claude", "claude or codex")
	fs.StringVar(&o.ready, "ready", "", "literal ready text to wait for in newly opened agent panes")
	fs.StringVar(&o.prefix, "shell-prefix", "!", "shell-mode key (!); use empty only for dedicated shell panes")
	fs.StringVar(&o.herdr, "herdr", "herdr", "herdr executable")
	fs.IntVar(&o.runs, "runs", 1, "fresh pane pairs for startup timing (1-20; every pane and session is retained)")
	fs.Float64Var(&o.maxRatio, "max-startup-ratio", 1, "maximum saint/host p50 and p95 startup ratio")
	fs.StringVar(&reportPath, "report", "", "write a new private JSON run report and .observations.json template")
	fs.StringVar(&evaluatePath, "evaluate", "", "score a saved run report without driving panes")
	fs.StringVar(&observationsPath, "observations", "", "operator evidence file for -evaluate")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "timeout per pane operation")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errs, "unexpected positional arguments")
		return 2
	}
	if evaluatePath != "" {
		if observationsPath == "" {
			fmt.Fprintln(errs, "evaluate requires observations")
			return 2
		}
		err := evaluate(evaluatePath, observationsPath, out)
		if errors.Is(err, errIncomplete) {
			return 1
		}
		if err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		return 0
	}
	if observationsPath != "" {
		fmt.Fprintln(errs, "observations requires evaluate")
		return 2
	}
	if reportPath != "" {
		for _, path := range []string{reportPath, reportPath + ".observations.json"} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(errs, "report output already exists or cannot be inspected:", path)
				return 2
			}
		}
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(errs, "parity requires a real Mac with Apple container and herdr; baseline not recorded")
		return 2
	}
	if err := validateOptions(o); err != nil {
		fmt.Fprintln(errs, err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return runLive(ctx, o, reportPath, out, errs, execute)
}

func runLive(ctx context.Context, o options, reportPath string, out, errs io.Writer, run runner) int {
	var timings []latencyPair
	d := driver{run: run, bin: o.herdr, timeout: o.timeout, prefix: o.prefix, timings: &timings}
	report := runReport{Version: evidenceVersion, RunID: nonce(), Created: time.Now().UTC(), Agent: o.agent}
	for _, bin := range []string{o.herdr, "container"} {
		bounded, done := context.WithTimeout(ctx, o.timeout)
		version, err := run(bounded, bin, "--version")
		done()
		if err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		fmt.Fprintf(out, "%s: %s\n", bin, strings.TrimSpace(version))
		if bin == o.herdr {
			report.Herdr = strings.TrimSpace(version)
		} else {
			report.Container = strings.TrimSpace(version)
		}
	}
	fmt.Fprintf(out, "UTC: %s; agent: %s; comparisons do not establish visual or lifecycle parity\n", time.Now().UTC().Format(time.RFC3339), o.agent)
	fmt.Fprintln(out, "Execution preflight sends a harmless command to dedicated idle panes; unexpected model handling remains possible. Stop and inspect locally if it fails.")
	var preflightErr error
	if o.host == "" {
		for range o.runs {
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
			preflightErr = preflight(ctx, d, host, saint)
			if preflightErr != nil {
				break
			}
		}
	} else {
		preflightErr = preflight(ctx, d, o.host, o.saint)
	}
	fmt.Fprintf(out, "host pane: %s; saint pane: %s\n", o.host, o.saint)
	report.Preflight = "PASS: framed execution confirmed on both panes; shell-only routing not established"
	results := unverifiedProbes(defaultProbes(), "preflight-only; comparison not run")
	if preflightErr != nil {
		report.Preflight = preflightErr.Error()
		results = unverifiedProbes(defaultProbes(), report.Preflight)
	} else if !o.preflightOnly {
		results = compareProbes(ctx, d, o.host, o.saint, defaultProbes())
	}
	fmt.Fprintln(out, "Preflight:", report.Preflight)
	for _, name := range manualProbes {
		if name == "prompt latency p50/p95" {
			results = append(results, timingResult(timings, o.maxRatio))
		} else {
			results = append(results, result{Name: name, Status: "NOT VERIFIED", Detail: "requires run-bound operator observation"})
		}
	}
	report.Host, report.Saint, report.Results, report.Timings = o.host, o.saint, results, timings
	if reportPath != "" {
		if err := writeNewJSON(reportPath, report); err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		if err := writeNewJSON(reportPath+".observations.json", templateFor(report)); err != nil {
			fmt.Fprintln(errs, err)
			return 2
		}
		fmt.Fprintf(out, "Saved run %s to %s; fill %s.observations.json and use -evaluate with -observations\n", report.RunID, reportPath, reportPath)
	}
	printResults(out, results)
	if preflightErr != nil {
		return 2
	}
	if o.preflightOnly {
		return 0
	}
	if passing(results) != len(results) {
		return 1
	}
	return 0
}

func validateOptions(o options) error {
	if o.prefix != "" && o.prefix != "!" {
		return errors.New("shell-prefix must be ! or empty for dedicated shell panes")
	}
	if o.preflightOnly && o.runs != 1 {
		return errors.New("preflight-only requires one pane pair")
	}
	if o.runs < 1 || o.runs > 20 {
		return errors.New("runs must be between 1 and 20")
	}
	if o.maxRatio < 1 || math.IsNaN(o.maxRatio) || math.IsInf(o.maxRatio, 0) {
		return errors.New("max-startup-ratio must be finite and at least 1")
	}
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
		if o.runs != 1 {
			return errors.New("repeated startup timing requires newly opened panes")
		}
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
