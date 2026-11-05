package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCodeSpec selects the host's existing Claude Code login as the source.
//
// saint reads that credential; it never copies it. A session takes the value
// live at creation and the host keeps the only stored copy, so there is no
// second secret on disk for saint to leak or leave stale.
const ClaudeCodeSpec = "claude-code"

// claudeCodeService is the keychain item Claude Code keeps its account
// credential in.
//
// The exact name matters. Claude Code also stores items suffixed with a hash -
// `Claude Code-credentials-<hex>` - and those hold OAuth tokens for the user's
// MCP servers, which are nothing to do with saint. Matching the bare name and
// only the bare name keeps saint out of them.
const claudeCodeService = "Claude Code-credentials"

// Status describes a credential without returning it.
//
// The form needs to show how much life a token has left before the user commits
// to it, and the auth screen needs to show whether one exists. Neither needs
// the secret, so neither gets it.
type Status struct {
	Present bool

	// ExpiresAt is when the value stops working. Zero means the source has no
	// expiry saint can see, which is not the same as "does not expire".
	ExpiresAt time.Time

	// Detail is a short non-secret note, such as the plan a login belongs to.
	Detail string
}

// Expired reports whether the credential has already lapsed.
func (s Status) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// Remaining reports how much life is left, or zero if that is unknown or past.
func (s Status) Remaining(now time.Time) time.Duration {
	if s.ExpiresAt.IsZero() {
		return 0
	}
	if left := s.ExpiresAt.Sub(now); left > 0 {
		return left
	}
	return 0
}

// Inspector is implemented by sources that can report on themselves without
// handing over the secret.
type Inspector interface {
	Status(ctx context.Context) (Status, error)
}

// claudeCodeSource reads the host's Claude Code login.
type claudeCodeSource struct {
	// read returns the raw keychain item. It is a field so tests do not need a
	// keychain, and so there is exactly one place that names the service.
	read func(ctx context.Context) ([]byte, error)
	now  func() time.Time
}

func newClaudeCodeSource(securityPath string) *claudeCodeSource {
	return &claudeCodeSource{
		read: func(ctx context.Context) ([]byte, error) {
			return readKeychainItem(ctx, securityPath, claudeCodeService)
		},
		now: time.Now,
	}
}

// claudeCodeCredentials is the subset of the item saint reads.
//
// Everything else in there - the refresh token above all - is deliberately not
// parsed. saint has no business refreshing a credential it does not own, and
// doing so would rotate the token out from under Claude Code itself.
type claudeCodeCredentials struct {
	OAuth struct {
		AccessToken string `json:"accessToken"`
		// ExpiresAt is unix milliseconds.
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func (s *claudeCodeSource) parse(ctx context.Context) (claudeCodeCredentials, error) {
	data, err := s.read(ctx)
	if err != nil {
		return claudeCodeCredentials{}, err
	}
	var creds claudeCodeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		// The format is undocumented, so a change here is expected eventually.
		// Say so plainly rather than reporting "no credential".
		return claudeCodeCredentials{}, fmt.Errorf("the Claude Code credential is not in a shape saint recognises: %w", err)
	}
	return creds, nil
}

func (s *claudeCodeSource) Get(ctx context.Context) (string, error) {
	creds, err := s.parse(ctx)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(creds.OAuth.AccessToken)
	if token == "" {
		return "", errors.New("no Claude Code login found; run `claude` and sign in")
	}

	// Refuse an expired token rather than handing a guest something that will
	// fail on its first request with an error it cannot explain.
	status := statusFrom(creds)
	if status.Expired(s.now()) {
		return "", fmt.Errorf("the Claude Code login expired at %s; open Claude Code to refresh it", status.ExpiresAt.Format(time.Kitchen))
	}
	return token, nil
}

func (s *claudeCodeSource) Status(ctx context.Context) (Status, error) {
	creds, err := s.parse(ctx)
	if err != nil {
		return Status{}, err
	}
	return statusFrom(creds), nil
}

func statusFrom(creds claudeCodeCredentials) Status {
	status := Status{Present: strings.TrimSpace(creds.OAuth.AccessToken) != ""}
	if creds.OAuth.ExpiresAt > 0 {
		status.ExpiresAt = time.UnixMilli(creds.OAuth.ExpiresAt)
	}
	if plan := strings.TrimSpace(creds.OAuth.SubscriptionType); plan != "" {
		status.Detail = plan
	}
	return status
}

func (s *claudeCodeSource) Describe() string { return "Claude Code login" }

// readKeychainItem fetches one generic password by service name.
func readKeychainItem(ctx context.Context, securityPath, service string) ([]byte, error) {
	if securityPath == "" {
		return nil, errors.New("the security binary was not found")
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, securityPath, "find-generic-password", "-s", service, "-w")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if detail := cleanStderr(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	if stdout.Len() > maxSecretBytes {
		return nil, fmt.Errorf("keychain item is %d bytes, which is too large to be a credential", stdout.Len())
	}
	return []byte(stdout.String()), nil
}
