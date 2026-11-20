package main

import (
	"bufio"
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
// first-run steps a pane would (container service, image, credential,
// firewall), so the first key press goes straight to the agent.

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

// runHerdrSetupPane is the setup pane. Like the session pane it waits for
// Enter before closing, so the last thing it says is read.
func runHerdrSetupPane(ctx context.Context, deps sessionDeps, hcfg herdrConfig, in io.Reader, out io.Writer) error {
	if deps.svc != nil {
		_, _ = ensureStableBinary(deps.svc.Store)
	}
	reader := bufio.NewReader(in)
	err := herdrSetup(ctx, deps, hcfg, reader, out)
	if err != nil {
		fmt.Fprintf(out, "\nsaint: %v\n", err)
	}
	fmt.Fprint(out, "\nPress Enter to close this pane.")
	_, _ = reader.ReadString('\n')
	return err
}

func herdrSetup(ctx context.Context, deps sessionDeps, hcfg herdrConfig, in *bufio.Reader, out io.Writer) error {
	fmt.Fprintln(out, "saint setup: one question per step; Enter accepts the default.")
	// Keys are offered even when an earlier step is declined or fails: the
	// first pane asks for whatever setup did not finish.
	prepErr := setupAgent(ctx, deps, in, out)
	if prepErr != nil {
		fmt.Fprintf(out, "saint: %v\n", prepErr)
	}
	bound, err := bindKeys(ctx, hcfg, saintBindings, in, out)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	key, ok := bound[saintBindings[0].Action]
	key = describeKey(key, herdrPrefix(ctx, hcfg))
	switch {
	case prepErr != nil && ok:
		fmt.Fprintf(out, "saint: not finished. Press %s in a pane inside your project and it asks for what is missing.\n", key)
	case prepErr != nil:
		fmt.Fprintln(out, "saint: not finished. Run setup again, or start a session with: herdr plugin action invoke new --plugin saint")
	case ok:
		fmt.Fprintf(out, "saint: set up. Press %s in a pane inside your project to start a sandboxed Claude session.\n", key)
	default:
		fmt.Fprintln(out, "saint: set up. Start a session with: herdr plugin action invoke new --plugin saint")
	}
	return prepErr
}

// setupAgent runs what the first Claude pane would otherwise ask: the
// container service, the image, a credential, and the firewall if it is on.
func setupAgent(ctx context.Context, deps sessionDeps, in *bufio.Reader, out io.Writer) error {
	if err := ensureService(ctx, deps, in, out); err != nil {
		return err
	}
	if err := deps.firstRun.prepare(ctx, sandbox.RuntimeClaude, in, out); err != nil {
		return err
	}
	f := deps.firstRun
	if f.firewallOn != nil && f.fixFirewall != nil && f.firewallOn(ctx) &&
		confirm(in, out, "the macOS firewall is on. Let saint's proxy through it now? sudo asks for your password, once.") {
		if err := f.fixFirewall(ctx, in, out); err != nil {
			return fmt.Errorf("allow saint through the firewall: %w", err)
		}
	}
	return nil
}

// bindKeys offers each binding not already in config.toml and writes the ones
// accepted in one change. It returns every saint action's key, old and new.
func bindKeys(ctx context.Context, hcfg herdrConfig, wanted []keyBinding, in *bufio.Reader, out io.Writer) (map[string]string, error) {
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
			return bound, nil
		}
	}
	if hcfg.defaults == nil {
		printManualBindings(out, todo)
		return bound, nil
	}
	defaults, err := hcfg.defaults(ctx)
	if err != nil {
		// Without herdr's defaults a chosen key could silently replace one.
		fmt.Fprintf(out, "saint: could not read herdr's default keys (%v)\n", err)
		printManualBindings(out, todo)
		return bound, nil
	}
	defaultKeys := parseHerdrKeys(defaults, true)
	taken := takenKeys(defaultKeys, user)
	prefix := prefixKey(defaultKeys, user)

	var chosen []chosenBinding
	for _, b := range todo {
		key, ok := chooseKey(b, taken, prefix, in, out)
		if !ok {
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
		return bound, nil
	}
	if backup != "" {
		fmt.Fprintf(out, "saint: added %d key(s) to %s; the old file is %s\n", len(chosen), hcfg.path, backup)
	} else {
		fmt.Fprintf(out, "saint: wrote %s with %d key(s)\n", hcfg.path, len(chosen))
	}
	for _, c := range chosen {
		bound[c.Action] = c.Key
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
func chooseKey(b keyBinding, taken map[string]string, prefix string, in *bufio.Reader, out io.Writer) (string, bool) {
	key := ""
	for _, c := range b.Candidates {
		if _, used := taken[normalizeKey(c)]; !used {
			key = c
			break
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
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
