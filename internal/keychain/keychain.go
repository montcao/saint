package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai"

	// ProviderClaudeSubscription holds the long-lived OAuth token from
	// `claude setup-token`. It is not an API key: it authenticates against a
	// Claude subscription, so traffic carrying it draws on that plan's limits
	// rather than billing a Console account per token.
	ProviderClaudeSubscription Provider = "claude-subscription"
)

// service is the keychain service name every saint item is filed under.
const service = "saint"

// exitItemNotFound is the exit status security(1) uses for "the specified item
// could not be found in the keychain". Every other non-zero status (locked
// keychain, denied access prompt, missing binary) is a real error and must not
// be reported as an absent key.
const exitItemNotFound = 44

// ErrNotFound reports that the keychain answered correctly and holds no item.
//
// "Absent" is an ordinary outcome - a user who has not set a key yet - and it
// has to be distinguishable from a locked keychain or a denied prompt, which
// are failures. Callers should test with errors.Is rather than matching on an
// exit status.
var ErrNotFound = errors.New("keychain item not found")

// execFunc runs bin with args, writing stdin to the child process's standard
// input. stdin exists so secrets never have to travel through argv, which is
// readable by every same-UID process via ps(1) for the subprocess lifetime.
type execFunc func(ctx context.Context, bin string, stdin []byte, args ...string) (stdout []byte, stderr []byte, err error)

type Keychain struct {
	securityPath string
	exec         execFunc
	logf         func(string, ...any)
}

func New(securityPath string, logf func(string, ...any)) *Keychain {
	return &Keychain{
		securityPath: securityPath,
		exec:         runCommand,
		logf:         logf,
	}
}

func (k *Keychain) Get(ctx context.Context, provider Provider) (string, error) {
	stdout, _, err := k.exec(ctx, k.securityPath, nil, "find-generic-password", "-s", service, "-a", string(provider), "-w")
	if err != nil {
		if isItemNotFound(err) {
			return "", fmt.Errorf("%s: %w", provider, ErrNotFound)
		}
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

// Has reports whether a key is stored for provider.
//
// Only exit status 44 ("item not found") means "no key". Anything else - a
// locked keychain, a denied access prompt, a missing security binary - is
// surfaced as an error so doctor does not tell the user to re-enter a key they
// already have.
func (k *Keychain) Has(ctx context.Context, provider Provider) (bool, error) {
	_, err := k.Get(ctx, provider)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("keychain %s: %w", provider, err)
}

// isItemNotFound reports whether security(1) exited with "item not found".
//
// Only that exact status means absent. Every other non-zero status - a locked
// keychain, a denied access prompt, a missing binary - is a real error and must
// not be reported as "you have not set this yet".
func isItemNotFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == exitItemNotFound
}

// Set stores value for provider.
//
// The secret is delivered on stdin, never in argv. security(1) says so itself:
// "Use of the -p or -w options is insecure. Specify -w as the last option to be
// prompted." Behavior observed on this machine (macOS 26.3, /usr/bin/security,
// verified 2026-08-25 against a throwaway service name):
//
//   - with a trailing bare -w, security writes "password data for new item: "
//     then "retype password for new item: " to stderr and reads both values
//     from stdin; a pipe works fine, no tty is required
//   - exactly TWO newline-terminated writes of the same value are required
//   - supplying only ONE line is actively dangerous: the retype read hits EOF,
//     security prints "passwords don't match", re-prompts, reads EOF for both
//     prompts, and then stores an EMPTY password while still exiting 0
//   - a duplicate item without -U exits 45; stderr never echoes the secret
func (k *Keychain) Set(ctx context.Context, provider Provider, value string) error {
	if value == "" {
		return errors.New("keychain set: value is empty")
	}
	if strings.ContainsAny(value, "\n\r\x00") {
		return errors.New("keychain set: value contains a newline or NUL")
	}

	// Two newline-terminated writes: the password prompt and the retype prompt.
	stdin := []byte(value + "\n" + value + "\n")
	_, _, err := k.exec(ctx, k.securityPath, stdin, "add-generic-password", "-U", "-s", service, "-a", string(provider), "-w")
	if err != nil {
		return redact(err, value)
	}
	return nil
}

func (k *Keychain) Remove(ctx context.Context, provider Provider) error {
	_, _, err := k.exec(ctx, k.securityPath, nil, "delete-generic-password", "-s", service, "-a", string(provider))
	return err
}

// runCommand executes bin with args and feeds stdin to the child process.
//
// Errors deliberately carry only the security(1) subcommand, the exit status,
// and trimmed stderr. They must never carry argv: for add-generic-password that
// would put the user's API key into an error string that main prints to the
// terminal (and its scrollback) on any failure.
func runCommand(ctx context.Context, bin string, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := cleanStderr(stderr.String())
		if detail != "" {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("security %s: %w: %s", subcommandOf(args), err, detail)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("security %s: %w", subcommandOf(args), err)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func subcommandOf(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "command"
	}
	return args[0]
}

// stderrPrompts are the interactive prompts security(1) writes to stderr when
// it reads a password from stdin. They are noise, not diagnostics.
var stderrPrompts = []string{
	"password data for new item:",
	"retype password for new item:",
	"passwords don't match",
}

func cleanStderr(value string) string {
	for _, prompt := range stderrPrompts {
		value = strings.ReplaceAll(value, prompt, " ")
	}
	return strings.Join(strings.Fields(value), " ")
}

// redact is defense in depth. runCommand already keeps argv out of errors, but
// if a future security(1) ever echoed the secret back on stderr it must not
// reach a terminal through the returned error.
func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, secret, "<redacted>"))
}
