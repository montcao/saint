package sandbox

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Auth modes for CreateOptions. They mirror store.ProxyAuth* and proxy.Auth*;
// this package imports neither, so the strings are repeated and mapped at the
// call site.
const (
	AuthKey          = "key"
	AuthSubscription = "subscription"
)

// SessionHeaderName is the header a guest in subscription mode uses to present
// its saint session token.
//
// It has to be a header of saint's own because the credential headers are
// carrying the guest's subscription token in that mode. Claude Code copies
// whatever ANTHROPIC_CUSTOM_HEADERS names onto every request, which is how it
// reaches the proxy; the proxy strips it before forwarding.
const SessionHeaderName = "x-saint-session"

// proxySessionHeaderLine renders the ANTHROPIC_CUSTOM_HEADERS value.
func proxySessionHeaderLine(token string) string {
	return SessionHeaderName + ": " + token
}

// writeEnvFile writes key=value pairs to a private temporary file for
// `container run --env-file`.
//
// The file is 0600 in a 0700 directory and removed as soon as the run returns.
// It exists only because --env would place the value in argv, which ps(1)
// exposes to every process running as this user.
func writeEnvFile(values map[string]string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "saint-env-")
	if err != nil {
		return "", nil, fmt.Errorf("creating an env file directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	// Sorted so the file is deterministic and testable.
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		value := values[name]
		if strings.ContainsAny(value, "\n\r") {
			// A newline would silently define a second variable, or truncate
			// this one. Refuse rather than write a file that means something
			// other than it says.
			cleanup()
			return "", nil, fmt.Errorf("value for %s contains a newline", name)
		}
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}

	path = dir + string(os.PathSeparator) + "env"
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing the env file: %w", err)
	}
	return path, cleanup, nil
}
