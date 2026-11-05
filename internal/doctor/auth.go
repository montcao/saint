package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/keychain"
	"github.com/montcao/saint/internal/store"
)

// authResults report which account a new session will bill.
//
// This is in doctor rather than left to config inspection because the two modes
// charge different accounts, and a config that reads as "subscription" while
// resolving to an API key is exactly the kind of quiet mistake that shows up on
// a bill instead of on screen.
func (d *Doctor) authResults(ctx context.Context, cfg store.Config, cfgErr error) []Result {
	if cfgErr != nil {
		return []Result{{
			Name:   "auth mode",
			Detail: "config could not be read",
			Fix:    "fix ~/.saint/config.json, then re-run saint doctor",
		}}
	}

	results := make([]Result, 0, 3)

	if !cfg.ProxyAuthIsValid() {
		results = append(results, Result{
			Name:   "proxy_auth",
			Detail: fmt.Sprintf("%q is not a mode saint understands; using %q", cfg.ProxyAuth, cfg.ProxyAuthOrDefault()),
			Fix:    `set "proxy_auth" to "auto", "key" or "subscription" in ~/.saint/config.json`,
		})
	}

	// The live login is a credential in its own right, and doctor used to miss
	// it entirely - reporting "api key" on a machine where `saint auth` said
	// "subscription".
	results = append(results, d.claudeCodeResult(ctx))

	spec := cfg.CredentialSpec(store.CredentialClaudeSubscription)
	source, parseErr := credential.Parse(spec, string(keychain.ProviderClaudeSubscription), d.credentialKeychain(), opPath())

	// 1Password is only worth reporting on when the user actually chose it.
	if strings.HasPrefix(spec, "op://") {
		results = append(results, d.onePasswordResult(ctx))
	}

	switch {
	case parseErr != nil:
		results = append(results, Result{
			Name:   "subscription token",
			Detail: parseErr.Error(),
			Fix:    `set "credentials"."claude_subscription" in ~/.saint/config.json`,
		})
	default:
		value, err := source.Get(ctx)
		ok := err == nil && value != ""
		detail := source.Describe()
		switch {
		case errors.Is(err, keychain.ErrNotFound):
			// Not stored yet is an ordinary state, not a fault to diagnose.
			detail = source.Describe() + ": not set"
		case err != nil:
			detail = fmt.Sprintf("%s: %v", source.Describe(), firstLine(err.Error()))
		case !ok:
			detail = source.Describe() + ": empty"
		}
		results = append(results, Result{
			Name:   "subscription token",
			OK:     ok,
			Detail: detail,
			Fix:    "claude setup-token, then saint keychain set claude-subscription",
		})
	}

	// The resolved mode, stated plainly. "auto" is only acceptable as a default
	// because this line always says which way it went.
	mode, why := resolveMode(cfg, results)
	results = append(results, Result{
		Name:   "auth mode",
		OK:     true,
		Detail: mode + " · " + why,
		Fix:    `set "proxy_auth" in ~/.saint/config.json to pin it`,
	})
	return results
}

// resolveMode reports what a new Claude session would use right now.
func resolveMode(cfg store.Config, results []Result) (mode, why string) {
	haveToken := false
	for _, result := range results {
		// Either subscription credential counts. Checking only the stored token
		// made doctor report "api key" while `saint auth` reported
		// "subscription" for the same machine.
		switch result.Name {
		case "subscription token", "claude code login":
			if result.OK {
				haveToken = true
			}
		}
	}

	return store.ResolveProxyAuth(cfg.ProxyAuthOrDefault(), haveToken)
}

// claudeCodeResult reports the host's existing Claude Code login.
func (d *Doctor) claudeCodeResult(ctx context.Context) Result {
	source, err := credential.Parse(credential.ClaudeCodeSpec, "", nil, "")
	if err != nil {
		return Result{Name: "claude code login", Detail: firstLine(err.Error())}
	}
	inspector, ok := source.(credential.Inspector)
	if !ok {
		return Result{Name: "claude code login", Detail: "not inspectable"}
	}

	status, err := inspector.Status(ctx)
	switch {
	case err != nil:
		return Result{
			Name:   "claude code login",
			Detail: firstLine(err.Error()),
			Fix:    "run `claude` and sign in",
		}
	case !status.Present:
		return Result{
			Name:   "claude code login",
			Detail: "not signed in",
			Fix:    "run `claude` and sign in",
		}
	case status.Expired(time.Now()):
		return Result{
			Name:   "claude code login",
			Detail: "expired; open Claude Code to refresh it",
			Fix:    "run `claude` and sign in",
		}
	}

	// Sessions take whatever is left, so the remaining life is the number that
	// matters, not the fact that a login exists.
	detail := "signed in"
	if plan := status.Detail; plan != "" {
		detail = plan
	}
	if left := status.Remaining(time.Now()); left > 0 {
		detail += fmt.Sprintf(" · %s left", left.Round(time.Minute))
	}
	return Result{Name: "claude code login", OK: true, Detail: detail}
}

// onePasswordResult checks that the op CLI can actually answer.
//
// `op` being installed says nothing about whether it is usable: the desktop app
// integration fails independently, and its error message names the fix.
func (d *Doctor) onePasswordResult(ctx context.Context) Result {
	path := opPath()
	if path == "" {
		return Result{
			Name:   "1password cli",
			Detail: "op is not on PATH, but a credential is configured as op://…",
			Fix:    "brew install 1password-cli",
		}
	}

	out, err := run(ctx, path, "whoami")
	if err != nil {
		return Result{
			Name:   "1password cli",
			Detail: firstLine(err.Error()),
			Fix:    "open the 1Password app, enable Settings → Developer → Integrate with 1Password CLI, then run: op signin",
		}
	}
	return Result{Name: "1password cli", OK: true, Detail: firstLine(out)}
}

func opPath() string {
	path, err := exec.LookPath("op")
	if err != nil {
		return ""
	}
	return path
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		value = value[:idx]
	}
	const limit = 160
	if len(value) > limit {
		value = value[:limit] + "…"
	}
	return value
}

// credentialKeychain adapts the doctor's keychain to the credential package.
func (d *Doctor) credentialKeychain() credential.KeychainGetter {
	return doctorKeychain{kc: d.Keychain}
}

type doctorKeychain struct{ kc *keychain.Keychain }

func (a doctorKeychain) Get(ctx context.Context, account string) (string, error) {
	if a.kc == nil {
		return "", fmt.Errorf("keychain is not configured")
	}
	return a.kc.Get(ctx, keychain.Provider(account))
}
