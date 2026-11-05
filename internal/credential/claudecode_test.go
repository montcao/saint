package credential

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func testSource(item string, err error, now time.Time) *claudeCodeSource {
	return &claudeCodeSource{
		read: func(context.Context) ([]byte, error) {
			if err != nil {
				return nil, err
			}
			return []byte(item), nil
		},
		now: fixedClock(now),
	}
}

func credentialJSON(token string, expiresAt time.Time, plan string) string {
	return `{"claudeAiOauth":{"accessToken":"` + token +
		`","refreshToken":"refresh-value","expiresAt":` +
		itoa(expiresAt.UnixMilli()) + `,"subscriptionType":"` + plan + `"}}`
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestClaudeCodeStatusReportsRemainingLife(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	expires := now.Add(6*time.Hour + 25*time.Minute)
	src := testSource(credentialJSON("token-value", expires, "max"), nil, now)

	status, err := src.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Present {
		t.Error("Present = false, want true")
	}
	if got := status.Remaining(now); got != 6*time.Hour+25*time.Minute {
		t.Errorf("Remaining() = %v, want 6h25m", got)
	}
	if status.Expired(now) {
		t.Error("Expired() = true for a live token")
	}
	// The plan is non-secret and worth showing; the token never is.
	if status.Detail != "max" {
		t.Errorf("Detail = %q, want the plan name", status.Detail)
	}
}

// A guest handed an expired token fails on its first request with an error it
// cannot explain. Refuse at the source instead.
func TestClaudeCodeRefusesAnExpiredToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	src := testSource(credentialJSON("token-value", now.Add(-time.Minute), "max"), nil, now)

	_, err := src.Get(context.Background())
	if err == nil {
		t.Fatal("Get() error = nil, want a refusal for an expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %v, want it to say the login expired", err)
	}
}

func TestClaudeCodeReturnsALiveToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	src := testSource(credentialJSON("token-value", now.Add(time.Hour), "max"), nil, now)

	got, err := src.Get(context.Background())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != "token-value" {
		t.Errorf("Get() = %q, want the access token", got)
	}
}

// The format is undocumented, so it will change eventually. Say that, rather
// than reporting "you are not signed in" and sending the user in circles.
func TestClaudeCodeUnrecognisedShapeSaysSo(t *testing.T) {
	t.Parallel()

	now := time.Now()
	src := testSource(`{"somethingElse":true}`, nil, now)

	if _, err := src.Get(context.Background()); err == nil {
		t.Fatal("Get() error = nil, want an error when no token is present")
	}

	broken := testSource(`not json at all`, nil, now)
	_, err := broken.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want an error for an unparseable item")
	}
	if !strings.Contains(err.Error(), "shape saint recognises") {
		t.Errorf("error = %v, want it to name the format as the problem", err)
	}
}

func TestClaudeCodeSurfacesKeychainFailure(t *testing.T) {
	t.Parallel()

	src := testSource("", errors.New("keychain is locked"), time.Now())
	if _, err := src.Status(context.Background()); err == nil {
		t.Fatal("Status() error = nil, want the keychain error")
	}
}

// saint must never read the sibling items - they hold OAuth tokens for the
// user's MCP servers and are nothing to do with saint.
func TestClaudeCodeNamesOnlyTheAccountItem(t *testing.T) {
	t.Parallel()

	if claudeCodeService != "Claude Code-credentials" {
		t.Fatalf("service = %q, want the bare account item", claudeCodeService)
	}
	if strings.Contains(claudeCodeService, "-credentials-") {
		t.Error("the service name would match the per-MCP-server items")
	}
}

func TestClaudeCodeSpecParses(t *testing.T) {
	t.Parallel()

	source, err := ParseWithSecurity(ClaudeCodeSpec, "anthropic", nil, "", "/usr/bin/security")
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", ClaudeCodeSpec, err)
	}
	if got := source.Describe(); got != "Claude Code login" {
		t.Errorf("Describe() = %q", got)
	}
	if _, ok := source.(Inspector); !ok {
		t.Error("the Claude Code source does not implement Inspector, so nothing can show its remaining life")
	}
}

func TestStatusWithoutExpiry(t *testing.T) {
	t.Parallel()

	// A keychain-stored setup-token has no expiry saint can see. That is not
	// the same as "does not expire", and Remaining must not claim otherwise.
	status := Status{Present: true}
	now := time.Now()
	if status.Expired(now) {
		t.Error("Expired() = true for a source with no known expiry")
	}
	if got := status.Remaining(now); got != 0 {
		t.Errorf("Remaining() = %v, want zero when the expiry is unknown", got)
	}
}
