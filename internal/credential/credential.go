// Package credential resolves a configured secret reference to its value.
//
// saint holds three kinds of secret - an Anthropic API key, an OpenAI API key,
// and a Claude subscription token - and users disagree about where secrets
// should live. Rather than picking one store, a credential is named by a spec
// string in ~/.saint/config.json and resolved here.
//
// Nothing in this package may log a secret. Errors from the underlying tools
// can quote vault names, file paths and item titles; those are diagnostics and
// are kept, but any value that could be the secret itself is redacted.
package credential

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Source resolves one secret.
type Source interface {
	Get(ctx context.Context) (string, error)

	// Describe names the source without revealing the secret. It is safe to
	// print in doctor output and error messages.
	Describe() string
}

// Timeout bounds a single resolution.
//
// It is generous because 1Password may prompt for biometrics and a user script
// may do network work. Claude Code warns at ten seconds for the equivalent
// apiKeyHelper; saint keeps the same order of magnitude and then gives up
// rather than hanging a session start forever.
const Timeout = 30 * time.Second

// maxSecretBytes caps what a source may return. A runaway script that prints a
// log file must fail loudly rather than have its first line silently used as a
// credential.
const maxSecretBytes = 64 << 10

// KeychainGetter is the subset of internal/keychain this package needs. Taking
// an interface keeps the keychain's exec plumbing testable from here without
// duplicating it.
type KeychainGetter interface {
	Get(ctx context.Context, provider string) (string, error)
}

// Parse turns a config spec into a Source.
//
//	""                       -> the keychain, under fallbackAccount
//	"keychain"               -> the same
//	"keychain:<account>"     -> the keychain, under an explicit account
//	"claude-code"            -> the host's existing Claude Code login, read live
//	"op://<vault>/<item>/<field>" -> 1Password, via the op CLI
//	"cmd:<command>"          -> the output of a shell command
//
// The cmd form runs arbitrary code from the user's own config file. That is the
// same trust boundary as Claude Code's apiKeyHelper - a file only the user
// writes - but it is worth naming: anything that can edit ~/.saint/config.json
// can already run code as the user next time saint starts.
func Parse(spec, fallbackAccount string, kc KeychainGetter, opPath string) (Source, error) {
	return ParseWithSecurity(spec, fallbackAccount, kc, opPath, defaultSecurityPath)
}

// defaultSecurityPath is pinned rather than resolved through PATH: a shim
// earlier in PATH would otherwise get to impersonate the keychain and harvest
// whatever it was asked for.
const defaultSecurityPath = "/usr/bin/security"

// ParseWithSecurity is Parse with the security(1) location injected, so tests
// can exercise the Claude Code source without a keychain.
func ParseWithSecurity(spec, fallbackAccount string, kc KeychainGetter, opPath, securityPath string) (Source, error) {
	spec = strings.TrimSpace(spec)

	switch {
	case spec == ClaudeCodeSpec:
		return newClaudeCodeSource(securityPath), nil

	case spec == "" || spec == "keychain":
		if fallbackAccount == "" {
			return nil, errors.New("keychain source needs an account")
		}
		return keychainSource{kc: kc, account: fallbackAccount}, nil

	case strings.HasPrefix(spec, "keychain:"):
		account := strings.TrimSpace(strings.TrimPrefix(spec, "keychain:"))
		if account == "" {
			return nil, errors.New(`keychain source needs an account after "keychain:"`)
		}
		return keychainSource{kc: kc, account: account}, nil

	case strings.HasPrefix(spec, "op://"):
		if opPath == "" {
			return nil, errors.New("1Password source needs the op CLI, which was not found on PATH")
		}
		return onePasswordSource{opPath: opPath, ref: spec}, nil

	case strings.HasPrefix(spec, "cmd:"):
		command := strings.TrimSpace(strings.TrimPrefix(spec, "cmd:"))
		if command == "" {
			return nil, errors.New(`command source needs a command after "cmd:"`)
		}
		return commandSource{command: command}, nil

	default:
		return nil, fmt.Errorf("unknown credential source %q: expected keychain, op://… or cmd:…", spec)
	}
}

type keychainSource struct {
	kc      KeychainGetter
	account string
}

func (s keychainSource) Get(ctx context.Context) (string, error) {
	if s.kc == nil {
		return "", errors.New("keychain is not configured")
	}
	return s.kc.Get(ctx, s.account)
}

func (s keychainSource) Describe() string { return "keychain:" + s.account }

type onePasswordSource struct {
	opPath string
	ref    string
}

func (s onePasswordSource) Get(ctx context.Context) (string, error) {
	// --no-newline keeps op from appending one; the trim below is belt and
	// braces for older versions.
	out, err := runSecret(ctx, s.opPath, "read", "--no-newline", s.ref)
	if err != nil {
		return "", fmt.Errorf("1Password read %s: %w", s.ref, err)
	}
	return out, nil
}

func (s onePasswordSource) Describe() string { return s.ref }

type commandSource struct {
	command string
}

func (s commandSource) Get(ctx context.Context) (string, error) {
	out, err := runSecret(ctx, "/bin/sh", "-c", s.command)
	if err != nil {
		return "", fmt.Errorf("credential command failed: %w", err)
	}
	return out, nil
}

// Describe deliberately does not print the command: a user may well have
// embedded a path or an argument they would not want on screen.
func (s commandSource) Describe() string { return "cmd" }

// runSecret executes a helper and returns its trimmed stdout.
//
// stderr is captured and folded into the error because a bare exit status makes
// a locked vault indistinguishable from a typo in a reference. Stdout never
// reaches an error, because stdout is the secret.
func runSecret(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return "", fmt.Errorf("timed out after %s", Timeout)
	}
	if err != nil {
		if detail := cleanStderr(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}

	if stdout.Len() > maxSecretBytes {
		return "", fmt.Errorf("returned %d bytes, which is too large to be a credential", stdout.Len())
	}
	secret := strings.TrimSpace(stdout.String())
	if secret == "" {
		return "", errors.New("returned nothing")
	}
	if strings.ContainsAny(secret, "\n\r") {
		// A multi-line result is a script printing diagnostics alongside the
		// secret. Using the whole blob as a credential produces a 401 that is
		// very hard to trace back to here.
		return "", errors.New("returned more than one line; a credential source must print only the secret")
	}
	return secret, nil
}

// cleanStderr keeps helper diagnostics short and single-line.
func cleanStderr(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\n", "; ")
	const limit = 200
	if len(value) > limit {
		value = value[:limit] + "…"
	}
	return value
}
