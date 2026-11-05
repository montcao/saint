package main

import (
	"context"
	"errors"
	"strings"

	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/tui"
)

// tuiCredentials lets the TUI's auth screen read and write the same secrets
// `saint auth` does, without the TUI having to know about the keychain.
type tuiCredentials struct {
	store    *store.Store
	keychain *keychain.Keychain
}

func newTUICredentials(st *store.Store, kc *keychain.Keychain) *tuiCredentials {
	return &tuiCredentials{store: st, keychain: kc}
}

func (c *tuiCredentials) List(ctx context.Context) ([]tui.CredentialState, error) {
	cfg, err := c.store.EnsureConfig()
	if err != nil {
		return nil, err
	}

	states := make([]tui.CredentialState, 0, len(credentialKinds))
	for _, state := range readCredentialStates(ctx, cfg, c.keychain) {
		kind := state.Kind
		spec := cfg.CredentialSpec(state.Kind.Config)
		states = append(states, tui.CredentialState{
			Kind: tui.CredentialKind{
				Name:     state.Kind.Name,
				Provider: state.Kind.Provider,
				Short:    state.Kind.Short,
				Config:   state.Kind.Config,
				Summary:  state.Kind.Summary,
				Setup:    state.Kind.Setup,
			},
			Source:    state.Source,
			Present:   state.Present,
			ExpiresAt: state.ExpiresAt,
			Detail:    state.Detail,
			Err:       state.Err,
			// Only keychain-backed values are saint's to write. A 1Password
			// reference or a command belongs to whatever produces it.
			// Only keychain-backed values are saint's to write. A 1Password
			// reference, a command, or the host's own Claude Code login all
			// belong to whatever produces them.
			Editable: kind.Spec == "" && (spec == "" || strings.HasPrefix(spec, "keychain")),
		})
	}
	return states, nil
}

func (c *tuiCredentials) Set(ctx context.Context, config, value string) error {
	kind, ok := kindByConfig(config)
	if !ok {
		return unknownCredential(config)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("nothing entered")
	}
	return c.keychain.Set(ctx, kind.Account, value)
}

func (c *tuiCredentials) Clear(ctx context.Context, config string) error {
	kind, ok := kindByConfig(config)
	if !ok {
		return unknownCredential(config)
	}
	return c.keychain.Remove(ctx, kind.Account)
}

func kindByConfig(config string) (credentialKind, bool) {
	for _, kind := range credentialKinds {
		if kind.Config == config {
			return kind, true
		}
	}
	return credentialKind{}, false
}
