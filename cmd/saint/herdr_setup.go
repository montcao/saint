package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

// Setup is the one command a new install runs after `herdr plugin install`:
//
//	herdr plugin action invoke setup --plugin saint
//
// A plugin cannot register a key itself, so instead of telling people to edit
// config.toml, the setup pane offers to write the binding for them: it picks a
// key herdr does not already use, backs config.toml up, has herdr validate the
// result before it replaces anything, and reloads the config. It also runs the
// first-run steps a pane would (container service, image, credential), so the
// first key press goes straight to the agent. The macOS firewall is not asked
// about here: a session offers that only if the firewall turns out to block
// its proxy.

const herdrSetupEntrypoint = "setup"

// keyBinding is one key saint offers to bind to one of its actions.
type keyBinding struct {
	// Action is the plugin action's qualified id, as config.toml names it.
	Action string
	// What says what the key does, for the question and the description.
	What string
	// Candidates are the keys offered, best first; the first one free is used.
	Candidates []string
}

// saintBindings are the keys setup offers, Claude first.
var saintBindings = []keyBinding{
	{
		Action:     herdrPluginID + ".new",
		What:       "open a sandboxed Claude pane",
		Candidates: []string{"prefix+a", "prefix+alt+a", "prefix+ctrl+a"},
	},
	{
		Action:     herdrPluginID + ".new-codex",
		What:       "open a sandboxed Codex pane",
		Candidates: []string{"prefix+shift+a", "prefix+alt+shift+a"},
	},
}

// setupPaneArgs builds the `herdr plugin pane open` call for the setup pane.
func setupPaneArgs(hc herdrContext) []string {
	args := []string{
		"plugin", "pane", "open",
		"--plugin", herdrPluginID,
		"--entrypoint", herdrSetupEntrypoint,
		"--placement", "split",
		"--direction", "right",
		"--focus",
	}
	if hc.FocusedPaneID != "" {
		args = append(args, "--target-pane", hc.FocusedPaneID)
	} else if hc.WorkspaceID != "" {
		args = append(args, "--workspace", hc.WorkspaceID)
	}
	return args
}

// runHerdrSetupOpen is the setup action: it opens the setup pane, where the
// questions can be asked, and returns.
func runHerdrSetupOpen(ctx context.Context) error {
	hc, err := parseHerdrContext(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON"))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, herdrBinary(), setupPaneArgs(hc)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// herdrConfig is what setup needs from herdr to bind keys. A nil field skips
// the step that needs it.
type herdrConfig struct {
	// path is the config.toml herdr reads.
	path string
	// defaults is herdr's default config, whose commented [keys] lines are
	// the bindings herdr uses when config.toml does not override them.
	defaults func(ctx context.Context) (string, error)
	// check has herdr validate the config at path.
	check func(ctx context.Context, path string) error
	// reload asks the running herdr to read config.toml again.
	reload func(ctx context.Context) error
	// now stamps the backup's name.
	now func() time.Time
	// notify shows a herdr notification. Setup's result is sent this way too,
	// so it is still there after the pane closes.
	notify func(ctx context.Context, title, body string) error
}

// liveHerdrConfig is the herdrConfig for the herdr that invoked saint.
func liveHerdrConfig() herdrConfig {
	return herdrConfig{
		path: herdrConfigPath(),
		defaults: func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, herdrBinary(), "--default-config").Output()
			return string(out), err
		},
		check: func(ctx context.Context, path string) error {
			cmd := exec.CommandContext(ctx, herdrBinary(), "config", "check")
			cmd.Env = append(os.Environ(), "HERDR_CONFIG_PATH="+path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		},
		reload: func(ctx context.Context) error {
			return exec.CommandContext(ctx, herdrBinary(), "server", "reload-config").Run()
		},
		now: time.Now,
		notify: func(ctx context.Context, title, body string) error {
			return showHerdrNotification(ctx, title, body, io.Discard)
		},
	}
}

// herdrConfigPath is where herdr reads config.toml: HERDR_CONFIG_PATH when
// set, otherwise ~/.config/herdr/config.toml on macOS and Linux.
func herdrConfigPath() string {
	if path := os.Getenv("HERDR_CONFIG_PATH"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "herdr", "config.toml")
}

// setupLogName is the setup pane's transcript, in the state directory.
const setupLogName = "setup.log"

// runHerdrSetupPane is the setup pane. It keeps a transcript of what it
// prints in setup.log, ends with a summary of every step, and closes only on
// q, so neither the summary nor the next step is lost to a stray Enter.
func runHerdrSetupPane(ctx context.Context, deps sessionDeps, hcfg herdrConfig, in io.Reader, out io.Writer) error {
	logPath := ""
	if deps.svc != nil {
		_, _ = ensureStableBinary(deps.svc.Store)
		logPath = filepath.Join(deps.svc.Store.Paths().Root, setupLogName)
	}
	return setupPane(ctx, deps, hcfg, newPrompter(in), out, logPath)
}

// setupPane runs setup with its output copied to logPath, when set, and waits
// for q before returning.
func setupPane(ctx context.Context, deps sessionDeps, hcfg herdrConfig, in *prompter, out io.Writer, logPath string) error {
	w := out
	if logPath != "" {
		f, err := openSetupLog(logPath)
		if err != nil {
			fmt.Fprintf(out, "saint: not keeping a transcript (%v)\n", err)
			logPath = ""
		} else {
			defer f.Close()
			w = io.MultiWriter(out, bestEffort{f})
		}
	}
	err := herdrSetup(ctx, deps, hcfg, in, w, logPath)
	waitForQuit(in, w)
	return err
}

// openSetupLog replaces the last run's transcript. Only this user can read
// it: it names the files and keys saint changed.
func openSetupLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	// OpenFile keeps the mode of a file that already exists.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// bestEffort is a writer whose failures are ignored, so a full disk stops the
// transcript but never the pane's output or the build streaming through it.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p)
	return len(p), nil
}

// waitForQuit keeps the pane open until q and Enter, or until input closes.
// The terminal is line-buffered, so q alone is not seen until Enter.
func waitForQuit(in *prompter, out io.Writer) {
	in.discardPending()
	for {
		fmt.Fprint(out, "\nType q and press Enter to close this pane. ")
		line, err := in.ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(line), "q") || err != nil {
			return
		}
	}
}

func herdrSetup(ctx context.Context, deps sessionDeps, hcfg herdrConfig, in *prompter, out io.Writer, logPath string) error {
	fmt.Fprintln(out, "saint setup: one question per step; Enter accepts the default. Keys pressed before a question appears are ignored.")
	var report setupReport
	// Keys are offered even when an earlier step is declined or fails: the
	// first pane asks for whatever setup did not finish.
	prepErr := setupAgent(ctx, deps, in, out, &report)
	if prepErr != nil {
		fmt.Fprintf(out, "saint: %v\n", prepErr)
	}
	bound, keysErr := bindKeys(ctx, hcfg, saintBindings, in, out, &report)
	if keysErr != nil {
		fmt.Fprintf(out, "saint: %v\n", keysErr)
		report.add("keys", stepFailed, keysErr.Error())
	}
	err := errors.Join(prepErr, keysErr)

	key, ok := bound[saintBindings[0].Action]
	key = describeKey(key, herdrPrefix(ctx, hcfg))
	title, next := setupOutcome(err == nil, ok, key)

	fmt.Fprintln(out)
	report.print(out)
	fmt.Fprintf(out, "%s. %s\n", title, next)
	if logPath != "" {
		fmt.Fprintf(out, "saint: this transcript is in %s\n", logPath)
	}
	if hcfg.notify != nil {
		// The pane may be closed before anyone reads it; a notification
		// that cannot be shown changes nothing about setup.
		_ = hcfg.notify(ctx, title, next)
	}
	return err
}

// setupOutcome is setup's one-line result and the next thing to do.
func setupOutcome(finished, bound bool, key string) (title, next string) {
	switch {
	case !finished && bound:
		return "saint: not finished", fmt.Sprintf("Press %s in a pane inside your project and it asks for what is missing.", key)
	case !finished:
		return "saint: not finished", "Run setup again, or start a session with: herdr plugin action invoke new --plugin saint"
	case bound:
		return "saint: set up", fmt.Sprintf("Press %s in a pane inside your project to start a sandboxed Claude session.", key)
	default:
		return "saint: set up", "Start a session with: herdr plugin action invoke new --plugin saint"
	}
}

// stepState is what one setup step came to.
type stepState int

const (
	stepDone stepState = iota
	stepAlready
	stepSkipped
	stepFailed
	stepNotReached
)

func (s stepState) String() string {
	switch s {
	case stepDone:
		return "done"
	case stepAlready:
		return "already done"
	case stepSkipped:
		return "skipped"
	case stepFailed:
		return "failed"
	default:
		return "not reached"
	}
}

type setupStep struct {
	name   string
	state  stepState
	detail string
}

// setupReport collects what each step did, for the summary at the end.
type setupReport struct{ steps []setupStep }

func (r *setupReport) add(name string, state stepState, detail string) {
	if r != nil {
		r.steps = append(r.steps, setupStep{name, state, detail})
	}
}

// addResult records a step from its error: declined is skipped, any other
// error failed, and otherwise done when it did something, already done when
// there was nothing to do.
func (r *setupReport) addResult(name string, did bool, err error) {
	switch {
	case errors.Is(err, errDeclined):
		r.add(name, stepSkipped, err.Error())
	case err != nil:
		r.add(name, stepFailed, err.Error())
	case did:
		r.add(name, stepDone, "")
	default:
		r.add(name, stepAlready, "")
	}
}

func (r *setupReport) print(out io.Writer) {
	fmt.Fprintln(out, "saint setup summary:")
	for _, s := range r.steps {
		if s.detail != "" {
			fmt.Fprintf(out, "  %s: %s: %s\n", s.name, s.state, s.detail)
		} else {
			fmt.Fprintf(out, "  %s: %s\n", s.name, s.state)
		}
	}
}

// setupAgent runs what the first Claude pane would otherwise ask: the
// container service, the image and a credential. Each step needs the one
// before it, so the first that does not finish ends the run.
func setupAgent(ctx context.Context, deps sessionDeps, in *prompter, out io.Writer, r *setupReport) error {
	rt := sandbox.RuntimeClaude
	steps := []struct {
		name string
		run  func() (bool, error)
	}{
		{"container service", func() (bool, error) { return ensureService(ctx, deps, in, out) }},
		{fmt.Sprintf("%s image", rt), func() (bool, error) { return deps.firstRun.ensureImage(ctx, rt, in, out) }},
		{"credential", func() (bool, error) { return deps.firstRun.ensureCredential(ctx, rt, in, out) }},
	}
	for i, step := range steps {
		did, err := step.run()
		r.addResult(step.name, did, err)
		if err != nil {
			for _, later := range steps[i+1:] {
				r.add(later.name, stepNotReached, "needs the "+step.name)
			}
			return err
		}
	}
	return nil
}

// bindKeys offers each binding not already in config.toml and writes the ones
// accepted in one change. It returns every saint action's key, old and new,
// and records what happened to each binding in r, which may be nil.
func bindKeys(ctx context.Context, hcfg herdrConfig, wanted []keyBinding, in *prompter, out io.Writer, r *setupReport) (map[string]string, error) {
	if hcfg.path == "" {
		return nil, errors.New("cannot tell where herdr's config.toml is; set HERDR_CONFIG_PATH")
	}
	current, err := os.ReadFile(hcfg.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	existed := err == nil
	user := parseHerdrKeys(string(current), false)

	bound := map[string]string{}
	var todo []keyBinding
	for _, b := range wanted {
		if key, ok := user.actions[b.Action]; ok {
			bound[b.Action] = key
			fmt.Fprintf(out, "saint: %s is already bound to %s\n", b.Action, key)
			r.add(bindingStep(b), stepAlready, key)
			continue
		}
		todo = append(todo, b)
	}
	if len(todo) == 0 {
		return bound, nil
	}

	// A broken config.toml is the user's to fix: saint edits only one herdr
	// already accepts, so a failure afterwards is saint's alone.
	if existed && hcfg.check != nil {
		if err := hcfg.check(ctx, hcfg.path); err != nil {
			fmt.Fprintf(out, "saint: herdr reports problems in %s, so saint will not edit it:\n%s\n", hcfg.path, err)
			printManualBindings(out, todo)
			failBindings(r, todo, "herdr reports problems in "+hcfg.path+"; add it by hand as shown above")
			return bound, nil
		}
	}
	if hcfg.defaults == nil {
		printManualBindings(out, todo)
		failBindings(r, todo, "herdr's default keys are unknown; add it by hand as shown above")
		return bound, nil
	}
	defaults, err := hcfg.defaults(ctx)
	if err != nil {
		// Without herdr's defaults a chosen key could silently replace one.
		fmt.Fprintf(out, "saint: could not read herdr's default keys (%v)\n", err)
		printManualBindings(out, todo)
		failBindings(r, todo, "could not read herdr's default keys; add it by hand as shown above")
		return bound, nil
	}
	defaultKeys := parseHerdrKeys(defaults, true)
	taken := takenKeys(defaultKeys, user)
	prefix := prefixKey(defaultKeys, user)

	var chosen []chosenBinding
	for _, b := range todo {
		key, ok := chooseKey(b, taken, prefix, in, out)
		if !ok {
			r.add(bindingStep(b), stepSkipped, "")
			continue
		}
		taken[key] = b.Action
		chosen = append(chosen, chosenBinding{keyBinding: b, Key: key})
	}
	if len(chosen) == 0 {
		return bound, nil
	}

	backup, err := writeBindings(ctx, hcfg, current, existed, chosen)
	if err != nil {
		fmt.Fprintf(out, "saint: did not change %s: %v\n", hcfg.path, err)
		printManualBindings(out, todo)
		for _, c := range chosen {
			r.add(bindingStep(c.keyBinding), stepFailed, fmt.Sprintf("did not change %s: %v", hcfg.path, err))
		}
		return bound, nil
	}
	if backup != "" {
		fmt.Fprintf(out, "saint: added %d key(s) to %s; the old file is %s\n", len(chosen), hcfg.path, backup)
	} else {
		fmt.Fprintf(out, "saint: wrote %s with %d key(s)\n", hcfg.path, len(chosen))
	}
	for _, c := range chosen {
		bound[c.Action] = c.Key
		r.add(bindingStep(c.keyBinding), stepDone, describeKey(c.Key, prefix))
	}
	if hcfg.reload != nil {
		if err := hcfg.reload(ctx); err != nil {
			fmt.Fprintf(out, "saint: herdr did not reload its config (%v); press %s to reload it\n", err, describeKey("prefix+shift+r", prefix))
		} else {
			fmt.Fprintf(out, "saint: herdr reloaded its config. If a key does nothing, press %s to reload it in this window.\n", describeKey("prefix+shift+r", prefix))
		}
	}
	return bound, nil
}

// bindingStep names a binding in setup's summary.
func bindingStep(b keyBinding) string {
	return "key to " + b.What
}

// failBindings records every binding as not written, for one reason.
func failBindings(r *setupReport, todo []keyBinding, reason string) {
	for _, b := range todo {
		r.add(bindingStep(b), stepFailed, reason)
	}
}

// prefixKey is herdr's prefix key: config.toml's, or herdr's default. Empty
// when neither says, so nothing invents one.
func prefixKey(defaults, user herdrKeys) string {
	if p := user.named["prefix"]; p != "" {
		return p
	}
	return defaults.named["prefix"]
}

// herdrPrefix reads the prefix key the way bindKeys does, for messages after
// it has returned.
func herdrPrefix(ctx context.Context, hcfg herdrConfig) string {
	var user herdrKeys
	if b, err := os.ReadFile(hcfg.path); err == nil {
		user = parseHerdrKeys(string(b), false)
	}
	var defaults herdrKeys
	if hcfg.defaults != nil {
		if d, err := hcfg.defaults(ctx); err == nil {
			defaults = parseHerdrKeys(d, true)
		}
	}
	return prefixKey(defaults, user)
}

// describeKey says how to press a key, for someone who has not read herdr's
// key syntax: "prefix+shift+a" with prefix ctrl+b is "Ctrl+b, then Shift+a".
func describeKey(key, prefix string) string {
	rest, hasPrefix := strings.CutPrefix(strings.TrimSpace(key), "prefix+")
	if !hasPrefix {
		return keyChord(rest)
	}
	if prefix == "" {
		return "the herdr prefix key, then " + keyChord(rest)
	}
	return keyChord(prefix) + ", then " + keyChord(rest)
}

// keyChord capitalizes a chord's modifiers and named keys, leaving a single
// character as typed: "ctrl+alt+s" is "Ctrl+Alt+s".
func keyChord(chord string) string {
	parts := strings.Split(chord, "+")
	for i, p := range parts {
		if len(p) > 1 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "+")
}

type chosenBinding struct {
	keyBinding
	Key string
}

// chooseKey asks whether to bind the first free candidate, taking yes, no or
// a different key. It returns false when the person declines.
func chooseKey(b keyBinding, taken map[string]string, prefix string, in *prompter, out io.Writer) (string, bool) {
	key := ""
	for _, c := range b.Candidates {
		if _, used := taken[normalizeKey(c)]; !used {
			key = c
			break
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		in.discardPending()
		if key == "" {
			fmt.Fprintf(out, "saint: which key should %s? Type a key such as prefix+alt+s, or Enter to skip: ", b.What)
		} else {
			fmt.Fprintf(out, "saint: bind %s (%s in config.toml) to %s? [Y/n, or type another key] ", describeKey(key, prefix), key, b.What)
		}
		answer, err := in.ReadString('\n')
		a := strings.TrimSpace(answer)
		switch strings.ToLower(a) {
		case "":
			// No answer at all (input closed) is not consent.
			if err != nil || key == "" {
				return "", false
			}
			return key, true
		case "y", "yes":
			if key == "" {
				continue
			}
			return key, true
		case "n", "no":
			return "", false
		}
		if user, used := taken[normalizeKey(a)]; used {
			fmt.Fprintf(out, "saint: %s is already used by %s\n", a, user)
			continue
		}
		if !plausibleKey(a) {
			fmt.Fprintf(out, "saint: %q does not look like a herdr key, such as prefix+a or ctrl+alt+s\n", a)
			continue
		}
		return a, true
	}
	return "", false
}

// plausibleKey rejects answers that cannot be a key before herdr is asked,
// so a typo is not written into config.toml. herdr's own check still runs.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9+_-]+$`)

func plausibleKey(key string) bool {
	return keyPattern.MatchString(key) && !strings.HasPrefix(key, "+") && !strings.HasSuffix(key, "+")
}

func normalizeKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// writeBindings appends the chosen bindings to config.toml. herdr validates
// the result in a file beside it first, and the old file is kept as a backup
// before the new one replaces it. It returns the backup's path, if any.
func writeBindings(ctx context.Context, hcfg herdrConfig, current []byte, existed bool, chosen []chosenBinding) (string, error) {
	var b strings.Builder
	b.Write(current)
	if len(current) > 0 && !strings.HasSuffix(string(current), "\n") {
		b.WriteString("\n")
	}
	for _, c := range chosen {
		b.WriteString(bindingBlock(c.Key, c.Action, c.What))
	}

	dir := filepath.Dir(hcfg.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(hcfg.path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".config.toml.saint-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once renamed
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return "", err
	}
	if hcfg.check != nil {
		if err := hcfg.check(ctx, tmpPath); err != nil {
			return "", fmt.Errorf("herdr rejected the new config: %w", err)
		}
	}

	backup := ""
	if existed {
		backup = hcfg.path + ".saint-" + hcfg.now().Format("20060102-150405") + ".bak"
		if err := os.WriteFile(backup, current, mode); err != nil {
			return "", fmt.Errorf("back up %s: %w", hcfg.path, err)
		}
	}
	if err := os.Rename(tmpPath, hcfg.path); err != nil {
		return "", err
	}
	return backup, nil
}

// bindingBlock is one [[keys.command]] entry for a plugin action.
func bindingBlock(key, action, what string) string {
	return fmt.Sprintf("\n# Added by saint setup.\n[[keys.command]]\nkey = %s\ntype = \"plugin_action\"\ncommand = %s\ndescription = %s\n",
		strconv.Quote(key), strconv.Quote(action), strconv.Quote("saint: "+what))
}

// printManualBindings says what to add by hand when saint will not edit the
// file itself.
func printManualBindings(out io.Writer, todo []keyBinding) {
	fmt.Fprintln(out, "saint: to bind keys yourself, add this to herdr's config.toml, then press your herdr prefix key and Shift+r to reload:")
	for _, b := range todo {
		key := "prefix+alt+a"
		if len(b.Candidates) > 0 {
			key = b.Candidates[0]
		}
		fmt.Fprint(out, bindingBlock(key, b.Action, b.What))
	}
}

// herdrKeys is what a config.toml binds: [keys] actions by name and
// [[keys.command]] entries by the command they run.
type herdrKeys struct {
	// named maps a [keys] setting, such as split_vertical, to its key.
	named map[string]string
	// commands maps each [[keys.command]] key to its command.
	commands map[string]string
	// actions maps each plugin action bound in [[keys.command]] to its key.
	actions map[string]string
}

var tomlStringLine = regexp.MustCompile(`^#?\s*([A-Za-z0-9_]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// parseHerdrKeys reads the key bindings out of a herdr config. With
// commented set it reads commented [keys] lines too, because that is how
// --default-config lists herdr's defaults; in a user's file a commented line
// binds nothing.
//
// It is a line scanner, not a TOML parser: herdr's own check validates every
// file saint writes, and this only has to find which keys are in use.
func parseHerdrKeys(config string, commented bool) herdrKeys {
	keys := herdrKeys{named: map[string]string{}, commands: map[string]string{}, actions: map[string]string{}}
	table := ""
	var cmdKey, cmdType, cmdCommand string
	flush := func() {
		if table == "keys.command" && cmdKey != "" {
			keys.commands[normalizeKey(cmdKey)] = cmdCommand
			if cmdType == "plugin_action" && cmdCommand != "" {
				keys.actions[cmdCommand] = cmdKey
			}
		}
		cmdKey, cmdType, cmdCommand = "", "", ""
	}
	for _, raw := range strings.Split(config, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			flush()
			table = strings.Trim(line, "[] ")
			if i := strings.Index(table, "#"); i >= 0 {
				table = strings.TrimSpace(strings.Trim(table[:i], "[] "))
			}
			continue
		}
		m := tomlStringLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		isComment := strings.HasPrefix(line, "#")
		if isComment && !commented {
			continue
		}
		name, value := m[1], m[2]+m[3]
		switch table {
		case "keys":
			if value != "" {
				keys.named[name] = value
			}
		case "keys.command":
			if isComment {
				continue
			}
			switch name {
			case "key":
				cmdKey = value
			case "type":
				cmdType = value
			case "command":
				cmdCommand = value
			}
		}
	}
	flush()
	return keys
}

// takenKeys is every key in use: herdr's defaults, unless config.toml sets
// that action to something else, then everything config.toml binds. It maps
// the key to what uses it, for the message when someone types a taken one.
func takenKeys(defaults, user herdrKeys) map[string]string {
	taken := map[string]string{}
	for name, key := range defaults.named {
		if _, overridden := user.named[name]; overridden {
			continue
		}
		taken[normalizeKey(key)] = "herdr's " + name
	}
	for name, key := range user.named {
		taken[normalizeKey(key)] = name
	}
	for key, command := range user.commands {
		taken[key] = command
	}
	return taken
}
