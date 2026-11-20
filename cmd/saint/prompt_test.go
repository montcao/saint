package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// fakeTerminal stands in for a pane's terminal. pending is what was typed and
// not yet read. Its prompter's drain models a person at it: what was typed
// before the question is lost, then they read the question and type the next
// of replies.
type fakeTerminal struct {
	pending []byte
	replies []string
	asked   int
}

func (t *fakeTerminal) Read(p []byte) (int, error) {
	if len(t.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

// typeAhead is keys pressed while nothing is asking.
func (t *fakeTerminal) typeAhead(s string) { t.pending = append(t.pending, s...) }

func (t *fakeTerminal) prompter() *prompter {
	p := &prompter{Reader: bufio.NewReader(t)}
	p.drain = func() {
		_, _ = p.Discard(p.Buffered())
		t.pending = nil
		if t.asked < len(t.replies) {
			t.pending = []byte(t.replies[t.asked] + "\n")
			t.asked++
		}
	}
	return p
}

// Enter pressed during a long step must not answer the next question.
func TestTypedAheadEnterDoesNotAnswer(t *testing.T) {
	term := &fakeTerminal{replies: []string{"n"}}
	term.typeAhead("\n\n\n")
	if confirm(term.prompter(), io.Discard, "change something?") {
		t.Fatal("an Enter typed before the question answered it")
	}
}

// Input that has closed is not a yes.
func TestConfirmClosedInputIsNotConsent(t *testing.T) {
	if confirm(&prompter{Reader: bufio.NewReader(strings.NewReader(""))}, io.Discard, "change something?") {
		t.Fatal("closed input was taken as yes")
	}
}

// Only a terminal has typed-ahead input to discard; a pipe's input was
// written on purpose.
func TestNewPrompterDrainsOnlyATerminal(t *testing.T) {
	if newPrompter(strings.NewReader("y\n")).drain != nil {
		t.Error("a reader that is not a terminal would be drained")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if newPrompter(r).drain != nil {
		t.Error("a pipe would be drained")
	}
}

// The setup pane end to end: Enters typed before and during the build answer
// nothing, the keys declined after their questions stay unbound, and the
// pane closes on q, not on Enter.
func TestSetupPaneIgnoresTypedAheadInputAndClosesOnQ(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	term := &fakeTerminal{replies: []string{"y", "n", "n", "\n\nq"}}
	term.typeAhead("\n\n")
	built := false
	deps := sessionDeps{
		checkService: func(context.Context) error { return nil },
		firstRun: firstRun{
			imagePresent: func(context.Context, sandbox.Runtime) bool { return built },
			buildImage: func(context.Context, sandbox.Runtime, io.Writer) error {
				built = true
				term.typeAhead("\n\n\n") // pressed while the build ran
				return nil
			},
			credentialReady: func(context.Context, sandbox.Runtime) bool { return true },
		},
	}
	f := &fakeHerdrConfig{}
	var out bytes.Buffer
	if err := setupPane(context.Background(), deps, f.config(path), term.prompter(), &out, ""); err != nil {
		t.Fatal(err)
	}
	if !built {
		t.Fatal("the image was not built after a yes")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keys were bound although both were declined: %v\n%s", err, out.String())
	}
	if term.asked != len(term.replies) {
		t.Fatalf("asked %d questions, want %d:\n%s", term.asked, len(term.replies), out.String())
	}
	if n := strings.Count(out.String(), "Type q and press Enter to close this pane."); n != 3 {
		t.Fatalf("close prompt shown %d times, want 3 (two Enters do not close):\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "nothing needs pressing") {
		t.Fatalf("the build does not say no input is needed:\n%s", out.String())
	}
}

func TestSetupSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	f := &fakeHerdrConfig{}
	started := false
	deps := sessionDeps{
		checkService: func(context.Context) error {
			if started {
				return nil
			}
			return sandbox.ErrServiceStopped
		},
		startService: func(context.Context) error { started = true; return nil },
		firstRun: firstRun{
			imagePresent: func(context.Context, sandbox.Runtime) bool { return false },
			buildImage: func(context.Context, sandbox.Runtime, io.Writer) error {
				return errors.New("no space left on device")
			},
			credentialReady: func(context.Context, sandbox.Runtime) bool { return false },
		},
	}
	var out bytes.Buffer
	err := herdrSetup(context.Background(), deps, f.config(path), answers("", "", "", "n"), &out, "/state/setup.log")
	if err == nil {
		t.Fatal("a failed build is not set up")
	}
	summary := out.String()[strings.Index(out.String(), "saint setup summary:"):]
	for _, want := range []string{
		"container service: done\n",
		"claude image: failed: build the claude image: no space left on device\n",
		"credential: not reached: needs the claude image\n",
		"key to open a sandboxed Claude pane: done: Ctrl+b, then a\n",
		"key to open a sandboxed Codex pane: skipped\n",
		"saint: not finished. Press Ctrl+b, then a in a pane inside your project and it asks for what is missing.\n",
		"saint: this transcript is in /state/setup.log\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	if len(f.notified) != 1 || f.notified[0] != "saint: not finished|Press Ctrl+b, then a in a pane inside your project and it asks for what is missing." {
		t.Errorf("notified = %q", f.notified)
	}

	// Declining a step is skipped, not failed, and a notification herdr
	// cannot show is not an error.
	out.Reset()
	f2 := &fakeHerdrConfig{notifyErr: errors.New("herdr is not running")}
	_ = herdrSetup(context.Background(), deps, f2.config(filepath.Join(t.TempDir(), "config.toml")), answers("n", "n", "n"), &out, "")
	if !strings.Contains(out.String(), "claude image: skipped: no claude image; build it with: saint doctor --build-images\n") {
		t.Errorf("a declined build is not reported as skipped:\n%s", out.String())
	}
	if len(f2.notified) != 1 {
		t.Errorf("notified = %q", f2.notified)
	}
}

// The transcript replaces the last run's, only its owner can read it, and
// a secret typed into the pane is not in it.
func TestSetupLogIsPrivateAndHoldsNoSecret(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "state", setupLogName)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("last run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-ant-oat01-not-a-real-secret"
	deps := sessionDeps{
		checkService: func(context.Context) error { return nil },
		firstRun: firstRun{
			credentialReady: func(context.Context, sandbox.Runtime) bool { return false },
			addCredential: func(_ context.Context, _ sandbox.Runtime, in *prompter, out io.Writer) error {
				_, err := in.ReadString('\n')
				return err
			},
		},
	}
	f := &fakeHerdrConfig{}
	var out bytes.Buffer
	if err := setupPane(context.Background(), deps, f.config(filepath.Join(dir, "config.toml")), answers(secret, "n", "n", "q"), &out, logPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("setup.log mode = %v, want 0600", info.Mode().Perm())
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "last run") {
		t.Error("the last run's transcript was kept")
	}
	if strings.Contains(string(log), secret) || strings.Contains(out.String(), secret) {
		t.Error("the secret was written out")
	}
	if !strings.Contains(string(log), "credential: done") || !strings.Contains(string(log), "this transcript is in "+logPath) {
		t.Errorf("setup.log lacks the summary:\n%s", log)
	}
}

// The firewall change runs in macOS's password dialog, with the path passed
// as an argument and never written into the script.
func TestFirewallArgsPassThePathAsAnArgument(t *testing.T) {
	for _, path := range []string{
		"/state/bin/saint",
		"/Users/tester/a b/.saint/bin/saint",
		`/tmp/it's "quoted" \ back$(touch x); y/saint`,
	} {
		args := firewallArgs(path)
		if args[0] != osascript || args[len(args)-1] != path {
			t.Errorf("args = %q", args)
		}
		script := strings.Join(args[1:len(args)-1], "\n")
		if strings.Contains(script, path) {
			t.Errorf("the path is written into the script:\n%s", script)
		}
		if !strings.Contains(script, "with administrator privileges") || !strings.Contains(script, socketFilterFW+" --add ") || !strings.Contains(script, socketFilterFW+" --unblockapp ") {
			t.Errorf("script:\n%s", script)
		}
	}
}

// AppleScript's quoted form of hands the shell the path exactly as given,
// whatever is in it. This runs the same script without administrator
// privileges and with printf in place of socketfilterfw.
func TestFirewallScriptQuotesThePath(t *testing.T) {
	if _, err := exec.LookPath(osascript); err != nil {
		t.Skip("osascript is not available")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	path := filepath.Join(dir, `a b`, `it's "q" \ $(touch `+marker+`); x`, "saint")
	args := []string{}
	for _, line := range firewallScript(`/usr/bin/printf '%s %s\n'`, false) {
		args = append(args, "-e", line)
	}
	got, err := exec.Command(osascript, append(args, path)...).CombinedOutput()
	if err != nil {
		t.Fatalf("osascript: %v: %s", err, got)
	}
	lines := strings.FieldsFunc(string(got), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) != 2 || lines[0] != "--add "+path || lines[1] != "--unblockapp "+path {
		t.Fatalf("the shell saw %q, want --add and --unblockapp with %q", lines, path)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("text in the path ran as a command")
	}
}

func TestRunFirewall(t *testing.T) {
	st := store.New(t.TempDir())
	var ran *exec.Cmd
	var out bytes.Buffer
	err := runFirewall(context.Background(), st, &out, func(cmd *exec.Cmd) error {
		ran = cmd
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Path != osascript || ran.Args[len(ran.Args)-1] != stableBinaryPath(st) {
		t.Fatalf("ran %q", ran.Args)
	}
	if ran.Stdin != nil {
		t.Fatal("the firewall step reads from the pane; the dialog needs nothing from it")
	}

	err = runFirewall(context.Background(), st, &out, func(cmd *exec.Cmd) error {
		io.WriteString(cmd.Stderr, "0:180: execution error: User canceled. (-128)\n")
		return errors.New("exit status 1")
	})
	if !errors.Is(err, errFirewallCanceled) {
		t.Fatalf("a canceled dialog = %v", err)
	}
	err = runFirewall(context.Background(), st, &out, func(cmd *exec.Cmd) error {
		io.WriteString(cmd.Stderr, "something else\n")
		return errors.New("exit status 1")
	})
	if err == nil || errors.Is(err, errFirewallCanceled) || !strings.Contains(err.Error(), "something else") {
		t.Fatalf("a failure = %v", err)
	}
}
