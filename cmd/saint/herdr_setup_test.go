package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

// defaultsSample has the shape of `herdr --default-config`: [keys] defaults
// are commented lines. Only the shape matters; the real list is read from
// herdr at run time.
const defaultsSample = `[keys]
# prefix = "ctrl+b"
# settings = "prefix+s"
# split_vertical = "prefix+v"
# focus_pane_left = "prefix+h"
# open_worktree = ""    # optional, unset by default

# [[keys.command]]
# key = "prefix+alt+g"
# type = "popup"
# command = "lazygit"

[keys.indexed]
# tabs = ""
`

func TestParseHerdrKeysReadsDefaultsFromComments(t *testing.T) {
	keys := parseHerdrKeys(defaultsSample, true)
	if keys.named["settings"] != "prefix+s" || keys.named["split_vertical"] != "prefix+v" {
		t.Fatalf("named = %v", keys.named)
	}
	if _, ok := keys.named["open_worktree"]; ok {
		t.Fatal("an unset default is not a key in use")
	}
	if len(keys.commands) != 0 {
		t.Fatalf("a commented example command is not a binding: %v", keys.commands)
	}
}

func TestParseHerdrKeysIgnoresCommentsInAUserFile(t *testing.T) {
	user := `[keys]
# settings = "prefix+s"
split_vertical = "prefix+backslash"

[[keys.command]]
key = "prefix+alt+g"
type = "popup"
command = "lazygit"

[[keys.command]]
key = 'prefix+a'
type = "plugin_action"
command = "saint.new"

[ui]
key = "not a binding"
`
	keys := parseHerdrKeys(user, false)
	if _, ok := keys.named["settings"]; ok {
		t.Fatal("a commented line in a user's file binds nothing")
	}
	if keys.named["split_vertical"] != "prefix+backslash" {
		t.Fatalf("named = %v", keys.named)
	}
	if keys.commands["prefix+alt+g"] != "lazygit" || keys.actions["saint.new"] != "prefix+a" {
		t.Fatalf("commands = %v, actions = %v", keys.commands, keys.actions)
	}
	if _, ok := keys.commands["not a binding"]; ok {
		t.Fatal("a key outside [[keys.command]] is not a binding")
	}
}

// A default the user moved elsewhere frees its key; the user's own keys are
// taken.
func TestTakenKeys(t *testing.T) {
	defaults := parseHerdrKeys(defaultsSample, true)
	user := parseHerdrKeys("[keys]\nsplit_vertical = \"prefix+backslash\"\n[[keys.command]]\nkey = \"Prefix+Alt+G\"\ncommand = \"lazygit\"\n", false)
	taken := takenKeys(defaults, user)
	for _, key := range []string{"prefix+s", "prefix+h", "ctrl+b", "prefix+backslash", "prefix+alt+g"} {
		if _, ok := taken[key]; !ok {
			t.Errorf("%s should be taken: %v", key, taken)
		}
	}
	if _, ok := taken["prefix+v"]; ok {
		t.Error("prefix+v was moved away from split_vertical, so it is free")
	}
}

func TestChooseKey(t *testing.T) {
	b := keyBinding{Action: "saint.new", What: "open a pane", Candidates: []string{"prefix+s", "prefix+a"}}
	taken := map[string]string{"prefix+s": "herdr's settings", "prefix+x": "herdr's close_pane"}
	cases := []struct {
		name    string
		answers []string
		key     string
		ok      bool
	}{
		{"enter takes the first free candidate", []string{""}, "prefix+a", true},
		{"yes takes it too", []string{"y"}, "prefix+a", true},
		{"no skips", []string{"n"}, "", false},
		{"another key", []string{"ctrl+alt+s"}, "ctrl+alt+s", true},
		{"a taken key asks again", []string{"prefix+x", "prefix+alt+q"}, "prefix+alt+q", true},
		{"something that is not a key asks again", []string{"open it please", ""}, "prefix+a", true},
		{"three bad answers give up", []string{"prefix+x", "prefix+x", "prefix+x", "prefix+a"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			key, ok := chooseKey(b, taken, "ctrl+b", answers(tc.answers...), &out)
			if key != tc.key || ok != tc.ok {
				t.Fatalf("got (%q, %v), want (%q, %v); output:\n%s", key, ok, tc.key, tc.ok, out.String())
			}
		})
	}
}

func TestChooseKeyClosedInputIsNotConsent(t *testing.T) {
	var out bytes.Buffer
	b := keyBinding{Action: "saint.new", What: "open a pane", Candidates: []string{"prefix+a"}}
	if key, ok := chooseKey(b, map[string]string{}, "ctrl+b", bufio.NewReader(strings.NewReader("")), &out); ok {
		t.Fatalf("bound %q with no answer", key)
	}
}

func TestChooseKeyAsksWhenEveryCandidateIsTaken(t *testing.T) {
	var out bytes.Buffer
	b := keyBinding{Action: "saint.new", What: "open a pane", Candidates: []string{"prefix+a"}}
	taken := map[string]string{"prefix+a": "something"}
	if _, ok := chooseKey(b, taken, "ctrl+b", answers(""), &out); ok {
		t.Fatal("Enter with nothing free skips")
	}
	if key, ok := chooseKey(b, taken, "ctrl+b", answers("prefix+alt+z"), &out); !ok || key != "prefix+alt+z" {
		t.Fatalf("got (%q, %v)", key, ok)
	}
}

// fakeHerdrConfig is a herdrConfig over a temporary config.toml that records what
// saint asked herdr to do.
type fakeHerdrConfig struct {
	checked  []string
	reloaded int
	reject   func(path string) error
}

func (f *fakeHerdrConfig) config(path string) herdrConfig {
	return herdrConfig{
		path:     path,
		defaults: func(context.Context) (string, error) { return defaultsSample, nil },
		check: func(_ context.Context, p string) error {
			f.checked = append(f.checked, p)
			if f.reject != nil {
				return f.reject(p)
			}
			return nil
		},
		reload: func(context.Context) error { f.reloaded++; return nil },
		now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
}

var testBindings = []keyBinding{
	{Action: "saint.new", What: "open a Claude pane", Candidates: []string{"prefix+s", "prefix+a"}},
	{Action: "saint.new-codex", What: "open a Codex pane", Candidates: []string{"prefix+shift+a"}},
}

func TestBindKeysAppendsBacksUpAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[ui]\nsidebar_width = 30"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdrConfig{}
	var out bytes.Buffer
	bound, err := bindKeys(context.Background(), f.config(path), testBindings, answers("", "n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if bound["saint.new"] != "prefix+a" {
		t.Fatalf("bound = %v; prefix+s is herdr's settings key and must be skipped", bound)
	}
	if _, ok := bound["saint.new-codex"]; ok {
		t.Fatal("the Codex key was declined")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), original+"\n") {
		t.Fatalf("the user's file must be kept as it was, with saint's lines after it:\n%s", got)
	}
	again := parseHerdrKeys(string(got), false)
	if again.actions["saint.new"] != "prefix+a" || again.named["sidebar_width"] != "" {
		t.Fatalf("written config reads back as %+v", again)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the original's 0600", info.Mode().Perm())
	}

	backup := path + ".saint-20260102-030405.bak"
	if b, err := os.ReadFile(backup); err != nil || string(b) != original {
		t.Fatalf("backup = %q, %v", b, err)
	}
	// herdr checked the original, then the new file before it replaced it.
	if len(f.checked) != 2 || f.checked[0] != path || f.checked[1] == path {
		t.Fatalf("checked = %v", f.checked)
	}
	if f.reloaded != 1 {
		t.Fatalf("reloaded %d times", f.reloaded)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.toml.saint-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}

	// Run again: both actions are known, nothing is asked and nothing written.
	out.Reset()
	f2 := &fakeHerdrConfig{}
	bound, err = bindKeys(context.Background(), f2.config(path), testBindings[:1], answers(), &out)
	if err != nil || bound["saint.new"] != "prefix+a" {
		t.Fatalf("bound = %v, %v", bound, err)
	}
	if len(f2.checked) != 0 || f2.reloaded != 0 || strings.Contains(out.String(), "[Y/n") {
		t.Fatalf("a bound key must not be asked again:\n%s", out.String())
	}
}

func TestBindKeysCreatesAMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr", "config.toml")
	f := &fakeHerdrConfig{}
	var out bytes.Buffer
	if _, err := bindKeys(context.Background(), f.config(path), testBindings[:1], answers(""), &out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || parseHerdrKeys(string(got), false).actions["saint.new"] != "prefix+a" {
		t.Fatalf("config = %q, %v", got, err)
	}
	backups, _ := filepath.Glob(path + ".saint-*")
	if len(backups) != 0 {
		t.Fatalf("there was nothing to back up: %v", backups)
	}
}

// A config herdr already rejects is the user's to fix: saint leaves it alone
// and prints what to add.
func TestBindKeysLeavesABrokenConfigAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "bad = [\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdrConfig{reject: func(string) error { return errors.New("config parse error") }}
	var out bytes.Buffer
	if _, err := bindKeys(context.Background(), f.config(path), testBindings[:1], answers(""), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("config changed to %q", got)
	}
	if !strings.Contains(out.String(), `command = "saint.new"`) || strings.Contains(out.String(), "[Y/n") {
		t.Fatalf("want the lines to add by hand and no question:\n%s", out.String())
	}
}

// When herdr rejects the new file, the old one stays and no backup is made.
func TestBindKeysKeepsTheOldFileWhenHerdrRejectsTheNewOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[ui]\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdrConfig{}
	f.reject = func(p string) error {
		if p == path {
			return nil
		}
		return errors.New("prefix+a: kept keys.command[1].key, disabled keys.command[2].key")
	}
	var out bytes.Buffer
	if _, err := bindKeys(context.Background(), f.config(path), testBindings[:1], answers(""), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("config changed to %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("want only config.toml left, got %v", entries)
	}
	if f.reloaded != 0 || !strings.Contains(out.String(), "herdr rejected the new config") {
		t.Fatalf("reloaded %d; output:\n%s", f.reloaded, out.String())
	}
}

// Without herdr's defaults a key could silently replace one of them, so
// nothing is written.
func TestBindKeysNeedsHerdrsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	f := &fakeHerdrConfig{}
	hcfg := f.config(path)
	hcfg.defaults = func(context.Context) (string, error) { return "", errors.New("exit status 2") }
	var out bytes.Buffer
	if _, err := bindKeys(context.Background(), hcfg, testBindings[:1], answers(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config written without herdr's defaults: %v", err)
	}
}

func TestBindingBlockQuotes(t *testing.T) {
	block := bindingBlock("prefix+a", "saint.new", `say "hi"`)
	keys := parseHerdrKeys("[[keys.command]]"+strings.SplitN(block, "[[keys.command]]", 2)[1], false)
	if keys.actions["saint.new"] != "prefix+a" {
		t.Fatalf("block reads back as %+v:\n%s", keys, block)
	}
	if !strings.Contains(block, `description = "saint: say \"hi\""`) {
		t.Fatalf("description not quoted:\n%s", block)
	}
}

func TestParseFirewallState(t *testing.T) {
	cases := map[string]bool{
		"Firewall is disabled. (State = 0)\n":                 false,
		"Firewall is enabled. (State = 1)\n":                  true,
		"Firewall is blocking all non-essential. (State = 2)": true,
		"something else entirely":                             true,
	}
	for in, want := range cases {
		if got := parseFirewallState(in); got != want {
			t.Errorf("parseFirewallState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSetupPaneArgsSplitsTheFocusedPane(t *testing.T) {
	args := setupPaneArgs(herdrContext{FocusedPaneID: "p1", WorkspaceID: "w1"})
	for _, want := range [][]string{{"--entrypoint", herdrSetupEntrypoint}, {"--target-pane", "p1"}, {"--plugin", herdrPluginID}} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Fatalf("args %v lack %v", args, want)
		}
	}
	if slices.Contains(args, "--workspace") {
		t.Fatalf("a focused pane is targeted, not the workspace: %v", args)
	}
}

// The manifest's ids are what the Go code opens and binds; a rename on one
// side alone breaks setup without a compile error.
func TestManifestDeclaresWhatSetupUses(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m := string(manifest)
	if !strings.Contains(m, `id = "`+herdrPluginID+`"`) {
		t.Fatalf("manifest id is not %q", herdrPluginID)
	}
	for _, id := range []string{herdrSetupEntrypoint, herdrPaneEntrypoint} {
		if !strings.Contains(m, "[[panes]]\nid = \""+id+"\"") {
			t.Errorf("manifest has no pane %q", id)
		}
	}
	for _, b := range saintBindings {
		action := strings.TrimPrefix(b.Action, herdrPluginID+".")
		if !strings.Contains(m, "[[actions]]\nid = \""+action+"\"") {
			t.Errorf("setup binds %s, but the manifest has no action %q", b.Action, action)
		}
	}
	if !strings.Contains(m, "[[actions]]\nid = \"setup\"") {
		t.Error("manifest has no setup action")
	}
}

// Declining the image still binds keys, and the end says what is left.
func TestHerdrSetupBindsKeysWhenAStepIsDeclined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	f := &fakeHerdrConfig{}
	built := false
	deps := sessionDeps{
		checkService: func(context.Context) error { return nil },
		firstRun: firstRun{
			imagePresent: func(context.Context, sandbox.Runtime) bool { return false },
			buildImage: func(context.Context, sandbox.Runtime, io.Writer) error {
				built = true
				return nil
			},
			firewallOn:  func(context.Context) bool { return false },
			fixFirewall: func(context.Context, io.Reader, io.Writer) error { t.Fatal("firewall is off"); return nil },
		},
	}
	var out bytes.Buffer
	err := herdrSetup(context.Background(), deps, f.config(path), answers("n", "", "n"), &out)
	if err == nil || built {
		t.Fatalf("err = %v, built = %v; declining the build is not set up", err, built)
	}
	got, _ := os.ReadFile(path)
	if parseHerdrKeys(string(got), false).actions["saint.new"] != "prefix+a" {
		t.Fatalf("keys not bound after a declined step:\n%s\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "not finished. Press Ctrl+b, then a") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// With everything in place, setup asks only about keys and the firewall.
func TestHerdrSetupOffersTheFirewallWhenItIsOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	f := &fakeHerdrConfig{}
	fixed := false
	deps := sessionDeps{
		checkService: func(context.Context) error { return nil },
		firstRun: firstRun{
			imagePresent:    func(context.Context, sandbox.Runtime) bool { return true },
			credentialReady: func(context.Context, sandbox.Runtime) bool { return true },
			firewallOn:      func(context.Context) bool { return true },
			fixFirewall: func(context.Context, io.Reader, io.Writer) error {
				fixed = true
				return nil
			},
		},
	}
	var out bytes.Buffer
	if err := herdrSetup(context.Background(), deps, f.config(path), answers("", "", ""), &out); err != nil {
		t.Fatal(err)
	}
	if !fixed || !strings.Contains(out.String(), "set up. Press Ctrl+b, then a") {
		t.Fatalf("fixed = %v; output:\n%s", fixed, out.String())
	}
}

func TestDescribeKey(t *testing.T) {
	cases := []struct{ key, prefix, want string }{
		{"prefix+a", "ctrl+b", "Ctrl+b, then a"},
		{"prefix+shift+a", "ctrl+b", "Ctrl+b, then Shift+a"},
		{"prefix+alt+a", "f12", "F12, then Alt+a"},
		{"ctrl+alt+s", "ctrl+b", "Ctrl+Alt+s"},
		{"prefix+a", "", "the herdr prefix key, then a"},
		{"prefix+tab", "ctrl+space", "Ctrl+Space, then Tab"},
	}
	for _, tc := range cases {
		if got := describeKey(tc.key, tc.prefix); got != tc.want {
			t.Errorf("describeKey(%q, %q) = %q, want %q", tc.key, tc.prefix, got, tc.want)
		}
	}
}

// A prefix set in config.toml wins over herdr's default.
func TestPrefixKey(t *testing.T) {
	defaults := parseHerdrKeys(defaultsSample, true)
	if got := prefixKey(defaults, parseHerdrKeys("", false)); got != "ctrl+b" {
		t.Fatalf("default prefix = %q", got)
	}
	if got := prefixKey(defaults, parseHerdrKeys("[keys]\nprefix = \"ctrl+space\"\n", false)); got != "ctrl+space" {
		t.Fatalf("user prefix = %q", got)
	}
}
