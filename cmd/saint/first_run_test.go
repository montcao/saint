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
	"github.com/montcao/saint/internal/store"
)

func answers(lines ...string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// A missing image is built after a yes, and a missing credential is asked
// for; a later pane with both finds nothing to do and asks nothing.
func TestFirstRunBuildsTheImageAndAsksForACredential(t *testing.T) {
	t.Parallel()

	built, added := false, false
	f := firstRun{
		imagePresent: func(context.Context, sandbox.Runtime) bool { return built },
		buildImage: func(_ context.Context, rt sandbox.Runtime, out io.Writer) error {
			built = true
			return nil
		},
		credentialReady: func(context.Context, sandbox.Runtime) bool { return added },
		addCredential: func(context.Context, sandbox.Runtime, *bufio.Reader, io.Writer) error {
			added = true
			return nil
		},
	}
	var out bytes.Buffer
	if err := f.prepare(context.Background(), sandbox.RuntimeClaude, answers(""), &out); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !built || !added {
		t.Fatalf("built = %v, added = %v; want both", built, added)
	}
	if !strings.Contains(out.String(), "image is not built") {
		t.Errorf("no question about the image:\n%s", out.String())
	}

	out.Reset()
	if err := f.prepare(context.Background(), sandbox.RuntimeClaude, answers(), &out); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a set-up install was asked something:\n%s", out.String())
	}
}

// Declining the build stops before a session is created, and says how to
// build it later.
func TestFirstRunDecliningTheBuildStops(t *testing.T) {
	t.Parallel()

	f := firstRun{
		imagePresent: func(context.Context, sandbox.Runtime) bool { return false },
		buildImage: func(context.Context, sandbox.Runtime, io.Writer) error {
			t.Error("built after a no")
			return nil
		},
	}
	err := f.prepare(context.Background(), sandbox.RuntimeCodex, answers("n"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--build-images") {
		t.Errorf("prepare after no = %v, want an error naming the build command", err)
	}
}

// Claude offers the plan token and the API key, never the host login (there
// is nothing to paste); Codex has one choice and is not asked to choose.
func TestCredentialChoices(t *testing.T) {
	t.Parallel()

	names := func(kinds []credentialKind) []string {
		var out []string
		for _, k := range kinds {
			out = append(out, k.Name)
		}
		return out
	}
	if got := names(credentialChoices(sandbox.RuntimeClaude)); !slices.Equal(got, []string{"anthropic-token", "anthropic-key"}) {
		t.Errorf("claude choices = %v", got)
	}
	if got := names(credentialChoices(sandbox.RuntimeCodex)); !slices.Equal(got, []string{"openai-key"}) {
		t.Errorf("codex choices = %v", got)
	}

	kind, err := chooseCredential(credentialChoices(sandbox.RuntimeClaude), answers("2"), io.Discard)
	if err != nil || kind.Name != "anthropic-key" {
		t.Errorf("choosing 2 = %v, %v", kind.Name, err)
	}
	if _, err := chooseCredential(credentialChoices(sandbox.RuntimeClaude), answers("9"), io.Discard); err == nil {
		t.Error("an out-of-range choice was accepted")
	}
	var out bytes.Buffer
	kind, err = chooseCredential(credentialChoices(sandbox.RuntimeCodex), answers(), &out)
	if err != nil || kind.Name != "openai-key" || strings.Contains(out.String(), "Choose") {
		t.Errorf("codex = %v, %v, asked %q", kind.Name, err, out.String())
	}
}

// The proxy's copy is written once, replaced when the binary changes, and
// left alone when it already matches, so its path - which the firewall keys
// on - never moves.
func TestCopyIfDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "bin", "saint")
	write := func(s string) {
		if err := os.WriteFile(src, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	write("one")
	if err := copyIfDifferent(src, dst); err != nil || read() != "one" {
		t.Fatalf("first copy: %v, %q", err, read())
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("copy mode = %v, want 0755", info.Mode().Perm())
	}
	before := info.ModTime()
	time.Sleep(10 * time.Millisecond)
	if err := copyIfDifferent(src, dst); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dst); !info.ModTime().Equal(before) {
		t.Error("an identical binary was rewritten")
	}
	write("two")
	if err := copyIfDifferent(src, dst); err != nil || read() != "two" {
		t.Fatalf("update: %v, %q", err, read())
	}
	entries, _ := os.ReadDir(filepath.Dir(dst))
	if len(entries) != 1 {
		t.Errorf("left temporary files behind: %v", entries)
	}
}

func TestStableBinaryLivesInTheStateDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	st := store.New(root)
	if got, want := stableBinaryPath(st), filepath.Join(st.Paths().Root, "bin", "saint"); got != want {
		t.Errorf("stableBinaryPath = %s, want %s", got, want)
	}
	for _, args := range firewallArgs("/state/bin/saint") {
		if args[0] != "sudo" || args[1] != socketFilterFW || args[len(args)-1] != "/state/bin/saint" {
			t.Errorf("firewall call = %q", args)
		}
	}
}

// A listener the firewall blocks is reported as such, so the pane can offer
// to fix it rather than only print a command.
func TestServeSessionReportsTheFirewall(t *testing.T) {
	t.Parallel()

	deps := sessionDeps{
		ensureProxy:  func(context.Context) error { return nil },
		listen:       func(context.Context, string) (string, error) { return "192.0.2.1:1", nil },
		probe:        func(string) bool { return false },
		reachTimeout: 10 * time.Millisecond,
	}
	err := serveSession(context.Background(), deps, "s1")
	if !errors.Is(err, errProxyUnreachable) || !strings.Contains(err.Error(), "saint firewall") {
		t.Errorf("serveSession = %v, want the firewall error naming saint firewall", err)
	}
}
