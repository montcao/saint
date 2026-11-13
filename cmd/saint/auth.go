package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"time"

	"golang.org/x/term"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/store"
)

// credentialKind describes one secret saint can hold, in the order a user meets
// them.
type credentialKind struct {
	// Name is the argument a user types: `saint auth anthropic-login`.
	//
	// Provider first, then what kind of credential it is. The earlier names
	// mixed the two vocabularies - "claude" beside "anthropic" beside "openai"
	// read as three vendors when Claude *is* Anthropic, and one of the three
	// was actually a credential type.
	Name string

	// Provider is the company the credential authenticates to: the heading a
	// user groups by when they are looking for it.
	Provider string

	// Short is the row label beneath a provider heading, where the provider is
	// already stated and repeating it would be noise.
	Short string

	// Aliases are older names that still work, so a command someone learned
	// yesterday does not break today.
	Aliases []string
	// Config is the key under "credentials" in config.json.
	Config string
	// Account is the keychain account the default source reads.
	Account keychain.Provider
	// Summary is one line of what it is for.
	Summary string
	// Setup is what the user has to do before they have a value to paste.
	Setup string

	// Spec pins the credential source, for kinds that do not come from the
	// keychain and are not configurable. Empty means read the spec from config.
	Spec string
}

// Provider names. These are the companies a credential authenticates to, and
// they are the headings a user groups by when looking for one.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// credentialKinds are ordered provider first, then by how little setup each
// costs, so the cheapest working option is the one a new user meets first.
//
// The config keys are deliberately left alone. They are what an existing
// ~/.saint/config.json already holds, and renaming a user's stored settings to
// tidy a label would be a poor trade.
var credentialKinds = []credentialKind{
	{
		Name:     "anthropic-login",
		Aliases:  []string{"live", "claude-code"},
		Provider: ProviderAnthropic,
		Short:    "plan login",
		Config:   store.CredentialClaudeCode,
		Spec:     credential.ClaudeCodeSpec,
		Summary:  "your existing Claude Code login - nothing to set up, expires in hours",
		Setup:    "sign in to Claude Code and saint will find it",
	},
	{
		Name:     "anthropic-token",
		Aliases:  []string{"claude", "claude-subscription"},
		Provider: ProviderAnthropic,
		Short:    "plan token",
		Config:   store.CredentialClaudeSubscription,
		Account:  keychain.ProviderClaudeSubscription,
		Summary:  "Claude plan token - sessions draw on your plan instead of billing per token, and it lasts a year",
		Setup:    "run `claude setup-token`, then paste the token here",
	},
	{
		Name:     "anthropic-key",
		Aliases:  []string{"anthropic"},
		Provider: ProviderAnthropic,
		Short:    "api key",
		Config:   store.CredentialAnthropic,
		Account:  keychain.ProviderAnthropic,
		Summary:  "Anthropic API key - billed per token to the account that owns the key",
		Setup:    "create a key at https://platform.claude.com, then paste it here",
	},
	{
		Name:     "openai-key",
		Aliases:  []string{"openai"},
		Provider: ProviderOpenAI,
		Short:    "api key",
		Config:   store.CredentialOpenAI,
		Account:  keychain.ProviderOpenAI,
		Summary:  "OpenAI API key - used by Codex sessions",
		Setup:    "create a key at https://platform.openai.com, then paste it here",
	},
}

func lookupCredentialKind(name string) (credentialKind, bool) {
	name = strings.TrimSpace(name)
	for _, kind := range credentialKinds {
		if strings.EqualFold(kind.Name, name) {
			return kind, true
		}
		// An older name still resolves. Renaming a command someone learned
		// yesterday is not worth a tidier table.
		for _, alias := range kind.Aliases {
			if strings.EqualFold(alias, name) {
				return kind, true
			}
		}
	}
	return credentialKind{}, false
}

// credentialState is what saint knows about one secret right now.
type credentialState struct {
	Kind credentialKind
	// Source is where it is read from, in non-secret form.
	Source string
	// Present is whether reading it produced a value.
	Present bool
	// ExpiresAt is when it stops working, when saint knows. Zero means unknown.
	ExpiresAt time.Time
	// Detail is a short non-secret note, such as the plan a login belongs to.
	Detail string
	// Err is why it could not be read, if it could not.
	Err error
}

// readCredentialStates resolves every credential without revealing any of them.
func readCredentialStates(ctx context.Context, cfg store.Config, kc *keychain.Keychain) []credentialState {
	states := make([]credentialState, 0, len(credentialKinds))
	for _, kind := range credentialKinds {
		state := credentialState{Kind: kind}
		spec := kind.Spec
		if spec == "" {
			spec = cfg.CredentialSpec(kind.Config)
		}
		source, err := credential.Parse(spec, string(kind.Account), keychainAdapter{kc: kc}, opPath())
		if err != nil {
			state.Source = cfg.CredentialSpec(kind.Config)
			state.Err = err
			states = append(states, state)
			continue
		}
		state.Source = source.Describe()

		// Ask without taking the secret where the source can answer that way.
		// A status carries the expiry the new-session form needs, and reading
		// a credential merely to find out whether one exists is needless
		// exposure.
		if inspector, ok := source.(credential.Inspector); ok {
			status, err := inspector.Status(ctx)
			switch {
			case err != nil:
				state.Err = err
			default:
				state.Present = status.Present
				state.ExpiresAt = status.ExpiresAt
				state.Detail = status.Detail
				if status.Present && status.Expired(time.Now()) {
					state.Err = errors.New("expired")
					state.Present = false
				}
			}
			states = append(states, state)
			continue
		}

		value, err := source.Get(ctx)
		switch {
		case errors.Is(err, keychain.ErrNotFound):
			// Not an error. The user simply has not set this one yet.
		case err != nil:
			state.Err = err
		case strings.TrimSpace(value) == "":
			state.Err = errors.New("empty")
		default:
			state.Present = true
		}
		states = append(states, state)
	}
	return states
}

// ResolvedAuth reports which mode new Claude sessions will actually use, and
// why. It is the same decision session.Service makes at create time.
func resolvedAuth(cfg store.Config, states []credentialState) (mode, why string) {
	subscription := false
	for _, state := range states {
		if state.Present && store.IsSubscriptionCredential(state.Kind.Config) {
			subscription = true
		}
	}
	return store.ResolveProxyAuth(cfg.ProxyAuthOrDefault(), subscription)
}

// authStatus renders the setup as plain lines. It is separated from printing so
// the shape can be tested without a terminal.
func authStatus(cfg store.Config, states []credentialState) []string {
	lines := []string{"credentials"}
	for _, state := range states {
		status := "not set"
		switch {
		case state.Present:
			status = "set"
		case state.Err != nil:
			status = "unavailable: " + firstLine(state.Err.Error())
		}
		lines = append(lines, fmt.Sprintf("  %-16s %-34s %s", state.Kind.Name, state.Source, status))
	}

	mode, why := resolvedAuth(cfg, states)
	lines = append(lines,
		"",
		"billing",
		fmt.Sprintf("  proxy_auth   %s", cfg.ProxyAuthOrDefault()),
		fmt.Sprintf("  new sessions %s (%s)", mode, why),
	)

	if mode == "api key" && cfg.ProxyAuthOrDefault() != store.ProxyAuthKey {
		lines = append(lines, "", "  to use your Claude plan instead: saint auth claude")
	}
	return lines
}

// firstLine keeps a helper error to one short line.
func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexAny(value, "\r\n"); idx >= 0 {
		value = value[:idx]
	}
	const limit = 90
	if len(value) > limit {
		value = value[:limit] + "…"
	}
	return value
}

func runAuth(ctx context.Context, st *store.Store, kc *keychain.Keychain, args []string) error {
	cfg, err := st.EnsureConfig()
	if err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "status" {
		states := readCredentialStates(ctx, cfg, kc)
		for _, line := range authStatus(cfg, states) {
			fmt.Println(line)
		}
		return nil
	}

	switch args[0] {
	case "mode":
		if len(args) < 2 {
			return errors.New("usage: saint auth mode auto|key|subscription")
		}
		return setAuthMode(st, args[1])
	case "clear":
		if len(args) < 2 {
			return errors.New("usage: saint auth clear claude|anthropic|openai")
		}
		kind, ok := lookupCredentialKind(args[1])
		if !ok {
			return unknownCredential(args[1])
		}
		return kc.Remove(ctx, kind.Account)
	}

	kind, ok := lookupCredentialKind(args[0])
	if !ok {
		return unknownCredential(args[0])
	}
	return storeCredential(ctx, cfg, kc, kind)
}

func unknownCredential(name string) error {
	names := make([]string, 0, len(credentialKinds))
	for _, kind := range credentialKinds {
		names = append(names, kind.Name)
	}
	return fmt.Errorf("unknown credential %q: expected %s", name, strings.Join(names, ", "))
}

func setAuthMode(st *store.Store, mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case store.ProxyAuthAuto, store.ProxyAuthKey, store.ProxyAuthSubscription:
	default:
		return fmt.Errorf("unknown mode %q: expected auto, key or subscription", mode)
	}
	if _, err := st.UpdateConfig(func(cfg *store.Config) error {
		cfg.ProxyAuth = mode
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("proxy_auth is now %s\n", mode)
	return nil
}

// storeCredential prompts for one secret and stores it.
//
// The value is read with echo off and never printed back, not even truncated:
// a terminal scrollback is a file.
func storeCredential(ctx context.Context, cfg store.Config, kc *keychain.Keychain, kind credentialKind) error {
	if spec := cfg.CredentialSpec(kind.Config); spec != "" && !strings.HasPrefix(spec, "keychain") {
		return fmt.Errorf(
			"%s is configured to come from %s; change it there, or remove the entry from \"credentials\" in ~/.saint/config.json to use the keychain",
			kind.Name, spec,
		)
	}

	fmt.Fprintf(os.Stderr, "%s\n", kind.Summary)
	fmt.Fprintf(os.Stderr, "%s\n", kind.Setup)

	value, err := readSecret(fmt.Sprintf("%s: ", kind.Name))
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("nothing entered")
	}
	if err := kc.Set(ctx, kind.Account, strings.TrimSpace(value)); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "stored. run `saint doctor` to confirm it resolves.\n")
	return nil
}

// readSecret reads one line with echo off, falling back to a plain read when
// stdin is not a terminal so the command still works in a pipe.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	defer fmt.Fprintln(os.Stderr)

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		value, err := term.ReadPassword(fd)
		return string(value), err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
