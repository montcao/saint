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
	"time"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/session"
	"github.com/montcao/saint/internal/store"
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

func runHerdr(ctx context.Context, deps sessionDeps, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: saint herdr open [claude|codex] | saint herdr pane | saint herdr egress")
	}
	switch args[0] {
	case "open":
		return runHerdrOpen(ctx, args[1:])
	case "pane":
		return runHerdrPane(ctx, deps, os.Stdin, os.Stdout)
	case "egress":
		return runHerdrEgress(deps.svc.Store.Paths().ControlSocket, os.Stdin, os.Stdout)
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

// sessionDeps is what starting or reopening a session needs from the wiring
// in run, for `saint new`, `saint attach` and the herdr pane alike.
type sessionDeps struct {
	svc         *session.Service
	ensureProxy func(context.Context) error
	// listen asks the running proxy to listen on a session's network and
	// returns the address.
	listen func(ctx context.Context, sessionID string) (string, error)
	// probe reports whether an address accepts connections.
	probe        func(addr string) bool
	reachTimeout time.Duration
	startService func(context.Context) error
	// checkService reports whether the container service answers, so a
	// stopped one is offered a start before anything is created.
	checkService func(context.Context) error
	// pane is the herdr pane this process runs in, if any.
	pane func() *store.HerdrPane
}

// runHerdrPane is the pane entrypoint. herdr closes a pane when its command
// exits, so every failure waits for Enter: an error that vanishes with its
// pane is an error nobody read.
func runHerdrPane(ctx context.Context, deps sessionDeps, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	err := herdrPane(ctx, deps, reader, out)
	if err != nil {
		fmt.Fprintf(out, "\nsaint: %v\n\nPress Enter to close this pane.", err)
		_, _ = reader.ReadString('\n')
	}
	return err
}

func herdrPane(ctx context.Context, deps sessionDeps, in *bufio.Reader, out io.Writer) error {
	if ref := os.Getenv(envSession); ref != "" {
		return reattach(ctx, deps, ref)
	}
	repo := os.Getenv(envRepo)
	if repo == "" {
		return fmt.Errorf("%s is not set; open this pane with the saint action", envRepo)
	}
	rt, err := parseRuntime(os.Getenv(envRuntime))
	if err != nil {
		return err
	}
	return startAndAttach(ctx, deps, in, out, session.CreateRequest{
		Repos:           []sandbox.Repo{{Path: repo}},
		Runtime:         rt,
		SkipPermissions: true,
	})
}

// reattach starts a stopped session if needed, makes sure its proxy is up,
// and attaches.
func reattach(ctx context.Context, deps sessionDeps, ref string) error {
	found, err := deps.svc.Find(ref)
	if err != nil {
		return err
	}
	// Start fails for a session that is already running, which is fine.
	_ = deps.svc.Start(ctx, found.ID)
	if err := serveSession(ctx, deps, found.ID); err != nil {
		return err
	}
	return attachHere(deps, found.ID)
}

// attachHere attaches to a session, recording on it the herdr pane this runs
// in for as long as the attach lasts, so the proxy knows where to ask about a
// host for it.
func attachHere(deps sessionDeps, sessionID string) error {
	var pane *store.HerdrPane
	if deps.pane != nil {
		pane = deps.pane()
	}
	if pane != nil {
		if err := deps.svc.Store.UpdateSession(sessionID, func(s *store.Session) { s.Herdr = pane }); err != nil {
			return err
		}
		defer func() {
			// Only this pane's own record is cleared: another pane may have
			// attached since and recorded itself.
			_ = deps.svc.Store.UpdateSession(sessionID, func(s *store.Session) {
				if s.Herdr != nil && *s.Herdr == *pane {
					s.Herdr = nil
				}
			})
		}()
	}
	return execAttach(deps.svc.AttachCmd(sessionID))
}

// currentHerdrPane is the herdr pane this process runs in, from the variables
// herdr sets in every pane, or nil outside herdr.
func currentHerdrPane() *store.HerdrPane {
	id := os.Getenv("HERDR_PANE_ID")
	if id == "" {
		return nil
	}
	return &store.HerdrPane{
		PaneID:     id,
		SocketPath: os.Getenv("HERDR_SOCKET_PATH"),
		BinPath:    os.Getenv("HERDR_BIN_PATH"),
	}
}

// startAndAttach creates a session behind the proxy and attaches to it.
func startAndAttach(ctx context.Context, deps sessionDeps, in *bufio.Reader, out io.Writer, req session.CreateRequest) error {
	// The first call that needs the container service.
	err := deps.checkService(ctx)
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
		err = deps.checkService(ctx)
	}
	if err != nil {
		return fmt.Errorf("container service: %w", err)
	}

	rt := req.Runtime
	if rt == "" {
		rt = sandbox.RuntimeClaude
	}
	fmt.Fprintf(out, "saint: starting a %s microVM on its own network\n", rt)
	created, err := deps.svc.Create(ctx, req)
	if err != nil {
		return err
	}

	// The proxy listens after the container starts, not before: the
	// network's gateway address exists only while a container is attached
	// (docs/research/spikes.md). The guest's init waits for the proxy before
	// starting the agent, so the agent's first request finds a listener.
	fmt.Fprintln(out, "saint: starting the credential proxy")
	if err := serveSession(ctx, deps, created.ID); err != nil {
		return fmt.Errorf("%w (session %s has no network access; reopen it after fixing this)", err, created.Name)
	}
	return attachHere(deps, created.ID)
}
