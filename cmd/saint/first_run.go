package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/montcao/saint/internal/doctor"
	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// First run happens in the pane.
//
// Everything a new install is missing - the agent's image, a credential, the
// firewall exception - is asked for in the first pane that needs it, one
// question each, instead of being steps in a README. Each check is cheap when
// the answer is already yes, so a later pane goes straight to the agent.

// firstRun is what a pane needs to set a new install up as it goes. A nil
// field skips that step.
type firstRun struct {
	// imagePresent reports whether the runtime's image is built.
	imagePresent func(ctx context.Context, rt sandbox.Runtime) bool
	// buildImage builds it, writing the build's output to out.
	buildImage func(ctx context.Context, rt sandbox.Runtime, out io.Writer) error
	// credentialReady reports whether a credential for the runtime resolves.
	credentialReady func(ctx context.Context, rt sandbox.Runtime) bool
	// addCredential asks for one and saves it.
	addCredential func(ctx context.Context, rt sandbox.Runtime, in *prompter, out io.Writer) error
	// fixFirewall lets the proxy through the macOS firewall, once a session's
	// proxy has turned out to be unreachable. macOS asks for the password in
	// its own dialog, so it reads nothing from the pane.
	fixFirewall func(ctx context.Context, out io.Writer) error
}

// errDeclined marks a step the person said no to, so setup can report it as
// skipped rather than failed. Its message is the wrapped error's alone.
var errDeclined = errors.New("declined")

type declinedError struct{ err error }

func (e declinedError) Error() string        { return e.err.Error() }
func (e declinedError) Unwrap() error        { return e.err }
func (e declinedError) Is(target error) bool { return target == errDeclined }

func declined(err error) error { return declinedError{err} }

// confirm asks a yes/no question that defaults to yes. Input typed before the
// question is discarded first, so an Enter pressed during a long step does
// not answer it.
func confirm(in *prompter, out io.Writer, question string) bool {
	in.discardPending()
	fmt.Fprintf(out, "saint: %s [Y/n] ", question)
	answer, err := in.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(answer))
	if a == "" && err != nil {
		// No answer at all (input closed) is not consent.
		return false
	}
	return a == "" || a == "y" || a == "yes"
}

// prepare runs the image and credential steps for a new session.
func (f firstRun) prepare(ctx context.Context, rt sandbox.Runtime, in *prompter, out io.Writer) error {
	if _, err := f.ensureImage(ctx, rt, in, out); err != nil {
		return err
	}
	_, err := f.ensureCredential(ctx, rt, in, out)
	return err
}

// ensureImage builds the runtime's image if it is missing and the person
// agrees. It reports whether there was anything to do.
func (f firstRun) ensureImage(ctx context.Context, rt sandbox.Runtime, in *prompter, out io.Writer) (built bool, err error) {
	if f.imagePresent == nil || f.buildImage == nil || f.imagePresent(ctx, rt) {
		return false, nil
	}
	if !confirm(in, out, fmt.Sprintf("the %s agent image is not built yet. Build it now? It takes a few minutes, once.", rt)) {
		return false, declined(fmt.Errorf("no %s image; build it with: saint doctor --build-images", rt))
	}
	fmt.Fprintf(out, "saint: building the %s image. This takes a few minutes; nothing needs pressing until saint asks again.\n", rt)
	if err := f.buildImage(ctx, rt, out); err != nil {
		return false, fmt.Errorf("build the %s image: %w", rt, err)
	}
	fmt.Fprintf(out, "saint: the %s image is built\n", rt)
	return true, nil
}

// ensureCredential asks for a credential if none resolves for the runtime. It
// reports whether there was anything to do.
func (f firstRun) ensureCredential(ctx context.Context, rt sandbox.Runtime, in *prompter, out io.Writer) (added bool, err error) {
	if f.credentialReady == nil || f.addCredential == nil || f.credentialReady(ctx, rt) {
		return false, nil
	}
	if err := f.addCredential(ctx, rt, in, out); err != nil {
		return false, err
	}
	return true, nil
}

// runtimeImageExists asks the container CLI whether a runtime's image exists.
func runtimeImageExists(containerPath string) func(context.Context, sandbox.Runtime) bool {
	return func(ctx context.Context, rt sandbox.Runtime) bool {
		if containerPath == "" {
			return true // the service check reports a missing runtime
		}
		return exec.CommandContext(ctx, containerPath, "image", "inspect", "saint/"+string(rt)+":latest").Run() == nil
	}
}

// buildRuntimeImage builds one runtime's image from the plugin's own checkout,
// streaming the build so a pane that takes minutes is visibly working.
func buildRuntimeImage(containerPath string) func(context.Context, sandbox.Runtime, io.Writer) error {
	return func(ctx context.Context, rt sandbox.Runtime, out io.Writer) error {
		root := doctor.ResolveRepoRoot(doctor.DefaultRepoRootCandidates()...)
		if root == "" {
			return errors.New("cannot find saint's images/ directory beside this binary")
		}
		return sandbox.BuildRuntimeImage(ctx, containerPath, root, string(rt), out)
	}
}

// runtimeProvider is the provider whose credential a runtime uses.
func runtimeProvider(rt sandbox.Runtime) string {
	if rt == sandbox.RuntimeCodex {
		return ProviderOpenAI
	}
	return ProviderAnthropic
}

// credentialChoices are the credentials a person can add for a runtime, in
// the order offered. A kind found rather than stored (the host's Claude Code
// login) is not offered: there is nothing to paste.
func credentialChoices(rt sandbox.Runtime) []credentialKind {
	var kinds []credentialKind
	for _, kind := range credentialKinds {
		if kind.Provider == runtimeProvider(rt) && kind.Account != "" {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// credentialResolves reports whether any credential for the runtime's
// provider is present, without reading a secret where the source can say so.
func credentialResolves(st *store.Store, kc *keychain.Keychain) func(context.Context, sandbox.Runtime) bool {
	return func(ctx context.Context, rt sandbox.Runtime) bool {
		cfg, err := st.EnsureConfig()
		if err != nil {
			return true // let session creation report the real error
		}
		for _, state := range readCredentialStates(ctx, cfg, kc) {
			if state.Kind.Provider == runtimeProvider(rt) && state.Present && state.Err == nil {
				return true
			}
		}
		return false
	}
}

// askForCredential offers the runtime's credentials and saves the one chosen
// in the keychain, once per install.
func askForCredential(st *store.Store, kc *keychain.Keychain) func(context.Context, sandbox.Runtime, *prompter, io.Writer) error {
	return func(ctx context.Context, rt sandbox.Runtime, in *prompter, out io.Writer) error {
		choices := credentialChoices(rt)
		if len(choices) == 0 {
			return fmt.Errorf("no credential for %s; add one with: saint auth", rt)
		}
		kind, err := chooseCredential(choices, in, out)
		if err != nil {
			return err
		}
		cfg, err := st.EnsureConfig()
		if err != nil {
			return err
		}
		return storeCredential(ctx, cfg, kc, kind)
	}
}

// chooseCredential asks which credential to add. One choice is not a question,
// but the secret is asked for next, so pending input is discarded either way.
func chooseCredential(choices []credentialKind, in *prompter, out io.Writer) (credentialKind, error) {
	in.discardPending()
	if len(choices) == 1 {
		fmt.Fprintf(out, "saint: no credential yet. Add one now; it is asked once per install.\n")
		return choices[0], nil
	}
	fmt.Fprintln(out, "saint: no credential yet. Which one should sessions use? It is asked once per install.")
	for i, kind := range choices {
		fmt.Fprintf(out, "  %d. %s\n", i+1, kind.Summary)
	}
	fmt.Fprintf(out, "Choose 1-%d: ", len(choices))
	answer, _ := in.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(choices) {
		return credentialKind{}, declined(errors.New("no credential chosen; add one with: saint auth"))
	}
	return choices[n-1], nil
}
