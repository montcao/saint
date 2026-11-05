package sandbox

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// proxyEnvNames is every variable this feature writes. Tests that assert
// absence check all of them, in both cases, because a half-applied proxy
// environment is worse than none: the tools that read the case saint wrote go
// through saint, the rest do not, and the difference is invisible.
var proxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// createArgvWithProxy is createArgv for tests that need to vary the proxy URL
// the guest is handed.
func createArgvWithProxy(t *testing.T, proxyURL string, opts CreateOptions) []string {
	t.Helper()

	binPath, argvPath := fakeContainer(t)
	sb := NewAppleContainer(binPath, nil)
	repos := []Repo{{Path: t.TempDir()}}
	if err := sb.Create(context.Background(), "saint-abc123", RuntimeClaude, repos, t.TempDir(), proxyURL, opts); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("reading recorded argv: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// With the setting on, the guest must be pointed at saint in every case a
// common tool reads.
func TestEgressOnPointsTheGuestAtTheProxy(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "http://192.0.2.1:8787", CreateOptions{
		ProxyToken:  "session-token",
		ProxyEgress: true,
	})
	joined := strings.Join(argv, "\n")

	const want = "http://saint:session-token@192.0.2.1:8787"
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if !strings.Contains(joined, name+"="+want) {
			t.Errorf("argv does not set %s=%s:\n%s", name, want, joined)
		}
	}
	// curl - and so git over HTTP - honours only the lowercase name, on
	// purpose (httpoxy). Uppercase-only would leave every clone unproxied.
	if !strings.Contains(joined, "http_proxy=") {
		t.Error("no lowercase http_proxy: curl and git would bypass the proxy entirely")
	}
}

// The important one. A guest pointed at a proxy that is not serving forward
// traffic fails every clone and every install, so "off" has to mean an
// untouched environment rather than a proxy URL that happens to be refused.
func TestEgressOffSetsNoProxyVariableAtAll(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "http://192.0.2.1:8787", CreateOptions{
		ProxyToken: "session-token",
	})
	joined := strings.Join(argv, "\n")

	for _, name := range proxyEnvNames {
		if strings.Contains(joined, name+"=") {
			t.Errorf("argv sets %s with guest egress proxying off:\n%s", name, joined)
		}
	}
}

// Subscription mode changes which credential the guest holds, not whether its
// traffic is proxied; the off state must hold there too.
func TestEgressOffInSubscriptionModeSetsNoProxyVariable(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "http://192.0.2.1:8787", CreateOptions{
		ProxyToken:        "session-token",
		AuthMode:          AuthSubscription,
		SubscriptionToken: "oauth-token",
	})
	joined := strings.Join(argv, "\n")

	for _, name := range proxyEnvNames {
		if strings.Contains(joined, name+"=") {
			t.Errorf("argv sets %s with guest egress proxying off:\n%s", name, joined)
		}
	}
}

// No proxy URL means there is nothing to point the guest at. That is not an
// error - it is the same state a session runs in when the proxy could not
// start - but it must not produce a half-written environment either.
func TestEgressWithoutAProxyURLSetsNothing(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "", CreateOptions{
		ProxyToken:  "session-token",
		ProxyEgress: true,
	})
	joined := strings.Join(argv, "\n")

	for _, name := range proxyEnvNames {
		if strings.Contains(joined, name+"=") {
			t.Errorf("argv sets %s with no proxy to point at:\n%s", name, joined)
		}
	}
}

// NO_PROXY is correctness, not tidiness: the guest's ANTHROPIC_BASE_URL points
// at the same address, and the forward path answers a request aimed at itself
// with 508. Without the proxy host here, turning egress proxying on would break
// the agent's own API calls.
func TestNoProxyCoversTheProxyHostAndLoopback(t *testing.T) {
	t.Parallel()

	argv := createArgvWithProxy(t, "http://192.0.2.1:8787", CreateOptions{
		ProxyToken:  "session-token",
		ProxyEgress: true,
	})

	var values []string
	for _, arg := range argv {
		for _, name := range []string{"NO_PROXY=", "no_proxy="} {
			if strings.HasPrefix(arg, name) {
				values = append(values, strings.TrimPrefix(arg, name))
			}
		}
	}
	if len(values) != 2 {
		t.Fatalf("found %d NO_PROXY assignments, want both cases: %v", len(values), values)
	}
	for _, value := range values {
		entries := strings.Split(value, ",")
		for _, want := range []string{"192.0.2.1", "localhost", "127.0.0.1"} {
			if !containsEntry(entries, want) {
				t.Errorf("NO_PROXY %q does not exempt %s", value, want)
			}
		}
		// The port would make the entry unmatchable for implementations that
		// only compare hosts.
		if strings.Contains(value, "8787") {
			t.Errorf("NO_PROXY %q carries a port; not every client matches host:port entries", value)
		}
	}
}

func containsEntry(entries []string, want string) bool {
	for _, entry := range entries {
		if strings.TrimSpace(entry) == want {
			return true
		}
	}
	return false
}

// A token is opaque bytes from the store's point of view. If one ever contains
// a URL delimiter it must be escaped, not allowed to re-shape the URL into one
// pointing somewhere else.
func TestEgressURLEscapesTheToken(t *testing.T) {
	t.Parallel()

	const token = "tok:en@evil.example/#?"
	env, err := guestEgressEnv("http://192.0.2.1:8787", token)
	if err != nil {
		t.Fatalf("guestEgressEnv() error = %v", err)
	}

	value := strings.TrimPrefix(env[0], "HTTP_PROXY=")
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("the proxy URL does not parse: %v", err)
	}
	if parsed.Host != "192.0.2.1:8787" {
		t.Fatalf("host = %q, want the proxy; the token escaped its field", parsed.Host)
	}
	password, ok := parsed.User.Password()
	if !ok || password != token {
		t.Errorf("password = %q (%v), want the token back unchanged", password, ok)
	}
}

// The user asked for this session's traffic to be visible. A container that
// silently talks to the internet directly is not a degraded version of that.
func TestEgressWithAnUnusableProxyURLFails(t *testing.T) {
	t.Parallel()

	for _, proxyURL := range []string{"192.0.2.1:8787", "http://", "::not a url"} {
		if _, err := guestEgressEnv(proxyURL, "session-token"); err == nil {
			t.Errorf("guestEgressEnv(%q) error = nil, want a refusal", proxyURL)
		}
	}
}

// The debug log outlives the container. None of the proxy variable names
// contain "key" or "token", so without this the session token would be written
// to ~/.saint/saint.log in full.
func TestRedactedArgvHidesTheProxyToken(t *testing.T) {
	t.Parallel()

	const token = "session-token"
	argv := []string{
		"--env", "HTTP_PROXY=http://saint:" + token + "@192.0.2.1:8787",
		"--env", "http_proxy=http://" + token + "@192.0.2.1:8787",
	}
	joined := strings.Join(redactArgv(argv), " ")

	if strings.Contains(joined, token) {
		t.Errorf("redacted argv still carries the session token: %s", joined)
	}
	// The address is the part of the line worth having.
	if !strings.Contains(joined, "192.0.2.1:8787") {
		t.Errorf("redaction removed the proxy address too: %s", joined)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"HTTP_PROXY=http://saint:tok@host:8787", "HTTP_PROXY=http://saint:<redacted>@host:8787"},
		{"HTTP_PROXY=http://tok@host:8787", "HTTP_PROXY=http://:<redacted>@host:8787"},
		{"HTTP_PROXY=http://a:b@c@host:8787", "HTTP_PROXY=http://a:<redacted>@host:8787"},
		{"ANTHROPIC_BASE_URL=http://host:8787", "ANTHROPIC_BASE_URL=http://host:8787"},
		{"--env", "--env"},
		{"user@example.com", "user@example.com"},
	}
	for _, tc := range cases {
		if got := redactURLUserinfo(tc.in); got != tc.want {
			t.Errorf("redactURLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
