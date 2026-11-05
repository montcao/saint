package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/store"
)

func findResult(t *testing.T, results []Result, name string) Result {
	t.Helper()

	for _, result := range results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("no %q result in %+v", name, results)
	return Result{}
}

func TestResolveMode(t *testing.T) {
	t.Parallel()

	withToken := []Result{{Name: "subscription token", OK: true}}
	withoutToken := []Result{{Name: "subscription token"}}

	cases := []struct {
		name     string
		cfg      store.Config
		results  []Result
		wantMode string
		wantWhy  string
	}{
		{
			name:     "auto with a token",
			cfg:      store.Config{ProxyAuth: store.ProxyAuthAuto},
			results:  withToken,
			wantMode: "subscription",
			wantWhy:  "Claude plan",
		},
		{
			name:     "auto without a token",
			cfg:      store.Config{ProxyAuth: store.ProxyAuthAuto},
			results:  withoutToken,
			wantMode: "api key",
			wantWhy:  "no subscription token",
		},
		{
			// Pinning key mode must not quietly become subscription just
			// because a token happens to exist.
			name:     "key ignores an available token",
			cfg:      store.Config{ProxyAuth: store.ProxyAuthKey},
			results:  withToken,
			wantMode: "api key",
			wantWhy:  "pinned to key",
		},
		{
			// Asking for a subscription and getting API billing has to say so.
			name:     "subscription without a token says why",
			cfg:      store.Config{ProxyAuth: store.ProxyAuthSubscription},
			results:  withoutToken,
			wantMode: "api key",
			wantWhy:  "no subscription token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mode, why := resolveMode(tc.cfg, tc.results)
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to mention %q", why, tc.wantWhy)
			}
		})
	}
}

// An unreadable config must not silently report a billing mode.
func TestAuthResultsWithAnUnreadableConfig(t *testing.T) {
	t.Parallel()

	d := &Doctor{}
	results := d.authResults(context.Background(), store.Config{}, context.Canceled)

	mode := findResult(t, results, "auth mode")
	if mode.OK {
		t.Error("auth mode reported OK with no readable config")
	}
}

// A typo in proxy_auth is reported rather than silently ignored.
func TestAuthResultsFlagAnUnknownMode(t *testing.T) {
	t.Parallel()

	d := &Doctor{}
	results := d.authResults(context.Background(), store.Config{ProxyAuth: "subscribe"}, nil)

	flagged := findResult(t, results, "proxy_auth")
	if flagged.OK {
		t.Error("an unknown proxy_auth value was reported as fine")
	}
	if !strings.Contains(flagged.Detail, "subscribe") {
		t.Errorf("detail = %q, want it to quote the bad value", flagged.Detail)
	}
}

// A misconfigured credential reference has to name itself; the fallback would
// otherwise look like a missing token.
func TestAuthResultsReportABadCredentialSpec(t *testing.T) {
	t.Parallel()

	d := &Doctor{}
	cfg := store.Config{Credentials: map[string]string{
		store.CredentialClaudeSubscription: "vault-of-secrets",
	}}
	results := d.authResults(context.Background(), cfg, nil)

	token := findResult(t, results, "subscription token")
	if token.OK {
		t.Error("an unparseable credential spec was reported as a working token")
	}
	if !strings.Contains(token.Detail, "vault-of-secrets") {
		t.Errorf("detail = %q, want it to quote the bad spec", token.Detail)
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()

	if got, want := firstLine("  one\ntwo  "), "one"; got != want {
		t.Errorf("firstLine() = %q, want %q", got, want)
	}
	if got := firstLine(strings.Repeat("x", 400)); len(got) > 165 {
		t.Errorf("firstLine() length = %d, want it truncated", len(got))
	}
}
