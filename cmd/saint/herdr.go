package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
)

// herdrPluginID and herdrPaneEntrypoint must match herdr-plugin.toml at the
// repository root.
const (
	herdrPluginID       = "saint"
	herdrPaneEntrypoint = "session"
)

// Environment passed from the action to the pane through
// `herdr plugin pane open --env`. A plugin pane's command is fixed in the
// manifest, so this is the only way to tell one pane which folder to sandbox.
const (
	envRepo    = "SAINT_REPO"
	envRuntime = "SAINT_RUNTIME"
	envSession = "SAINT_SESSION"
)

// herdrContext is the part of HERDR_PLUGIN_CONTEXT_JSON an action reads.
type herdrContext struct {
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceCwd   string `json:"workspace_cwd"`
	FocusedPaneID  string `json:"focused_pane_id"`
	FocusedPaneCwd string `json:"focused_pane_cwd"`
}

func parseHerdrContext(raw string) (herdrContext, error) {
	var hc herdrContext
	if strings.TrimSpace(raw) == "" {
		return hc, errors.New("HERDR_PLUGIN_CONTEXT_JSON is not set; run this from a herdr plugin action")
	}
	if err := json.Unmarshal([]byte(raw), &hc); err != nil {
		return hc, fmt.Errorf("HERDR_PLUGIN_CONTEXT_JSON: %w", err)
	}
	return hc, nil
}

// repo is the folder a new session mounts: the focused pane's, which is where
// the user is looking, falling back to the workspace's.
func (hc herdrContext) repo() string {
	if hc.FocusedPaneCwd != "" {
		return hc.FocusedPaneCwd
	}
	return hc.WorkspaceCwd
}

func parseRuntime(name string) (sandbox.Runtime, error) {
	switch strings.TrimSpace(name) {
	case "", "claude":
		return sandbox.RuntimeClaude, nil
	case "codex":
		return sandbox.RuntimeCodex, nil
	default:
		return "", fmt.Errorf("unknown runtime %q: expected claude or codex", name)
	}
}

// paneOpenArgs builds the `herdr plugin pane open` call that puts a new
// sandboxed session beside the focused pane.
//
// HERDR_AGENT goes on the pane's own environment, not the guest's: herdr
// only reads the hint from the host process, and with it set herdr reads the
// guest's screen through `container exec` and tracks the agent's state.
func paneOpenArgs(hc herdrContext, rt sandbox.Runtime) ([]string, error) {
	repo := hc.repo()
	if repo == "" {
		return nil, errors.New("herdr did not report a folder for the focused pane or the workspace")
	}
	args := []string{
		"plugin", "pane", "open",
		"--plugin", herdrPluginID,
		"--entrypoint", herdrPaneEntrypoint,
		"--placement", "split",
		"--direction", "right",
		"--focus",
		"--env", envRepo + "=" + repo,
		"--env", envRuntime + "=" + string(rt),
		"--env", "HERDR_AGENT=" + string(rt),
	}
	if hc.FocusedPaneID != "" {
		args = append(args, "--target-pane", hc.FocusedPaneID)
	} else if hc.WorkspaceID != "" {
		args = append(args, "--workspace", hc.WorkspaceID)
	}
	return args, nil
}

// herdrBinary is the herdr that invoked us, so a plugin action never runs a
// different herdr found earlier on PATH.
func herdrBinary() string {
	if path := os.Getenv("HERDR_BIN_PATH"); path != "" {
		return path
	}
	return "herdr"
}

func runHerdr(ctx context.Context, deps herdrDeps, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: saint herdr open [claude|codex] | saint herdr pane")
	}
	switch args[0] {
	case "open":
		return runHerdrOpen(ctx, args[1:])
	case "pane":
		return runHerdrPane(ctx, deps, os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown herdr command %q", args[0])
	}
}

// runHerdrOpen is the plugin action. It returns as soon as herdr has the
// pane; the slow work (proxy, container) happens in the pane, where the user
// can see it.
func runHerdrOpen(ctx context.Context, args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	rt, err := parseRuntime(name)
	if err != nil {
		return err
	}
	hc, err := parseHerdrContext(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON"))
	if err != nil {
		return err
	}
	openArgs, err := paneOpenArgs(hc, rt)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, herdrBinary(), openArgs...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// herdrDeps is what the pane needs from the wiring in run.
type herdrDeps struct {
	svc          *session.Service
	ensureProxy  func(context.Context) error
	startService func(context.Context) error
	guestURL     func(context.Context) (string, error)
}

// runHerdrPane is the pane entrypoint. herdr closes a pane when its command
// exits, so every failure waits for Enter: an error that vanishes with its
// pane is an error nobody read.
func runHerdrPane(ctx context.Context, deps herdrDeps, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	err := herdrPane(ctx, deps, reader, out)
	if err != nil {
		fmt.Fprintf(out, "\nsaint: %v\n\nPress Enter to close this pane.", err)
		_, _ = reader.ReadString('\n')
	}
	return err
}

func herdrPane(ctx context.Context, deps herdrDeps, in *bufio.Reader, out io.Writer) error {
	if ref := os.Getenv(envSession); ref != "" {
		found, err := deps.svc.Find(ref)
		if err != nil {
			return err
		}
		_ = deps.svc.Start(ctx, found.ID)
		if err := deps.ensureProxy(ctx); err != nil {
			return err
		}
		return execAttach(deps.svc.AttachCmd(found.ID))
	}

	repo := os.Getenv(envRepo)
	if repo == "" {
		return fmt.Errorf("%s is not set; open this pane with the saint action", envRepo)
	}
	rt, err := parseRuntime(os.Getenv(envRuntime))
	if err != nil {
		return err
	}

	// The guest's proxy address comes from the runtime's network
	// configuration, so it is known before any container exists. Reading it
	// is also the first call that needs the container service.
	url, err := deps.guestURL(ctx)
	if errors.Is(err, sandbox.ErrServiceStopped) {
		// Usual after a reboot. Starting it affects every container on this
		// Mac, not just saint's, so the user says yes first.
		fmt.Fprint(out, "saint: Apple's container service is not running. Start it now? [Y/n] ")
		answer, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
			return err
		}
		fmt.Fprintln(out, "saint: starting the container service")
		if err := deps.startService(ctx); err != nil {
			return fmt.Errorf("start container service: %w", err)
		}
		url, err = deps.guestURL(ctx)
	}
	if err != nil {
		return fmt.Errorf("proxy address: %w", err)
	}
	deps.svc.ProxyURL = url

	fmt.Fprintf(out, "saint: starting a %s microVM for %s\n", rt, repo)
	created, err := deps.svc.Create(ctx, session.CreateRequest{
		Repos:           []sandbox.Repo{{Path: repo}},
		Runtime:         rt,
		SkipPermissions: true,
	})
	if err != nil {
		return err
	}

	// The proxy starts after the container, not before: the bridge address
	// it binds exists only while a container is attached to the network
	// (docs/research/spikes.md). The agent sends nothing until someone types,
	// and a bound listener outlives the bridge, so this order leaves no
	// window where a live agent has no proxy.
	fmt.Fprintln(out, "saint: starting the credential proxy")
	if err := deps.ensureProxy(ctx); err != nil {
		return fmt.Errorf("%w (session %s is running without API access; reopen it after fixing this)", err, created.Name)
	}
	return execAttach(deps.svc.AttachCmd(created.ID))
}
