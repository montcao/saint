package store

import (
	"strings"
	"testing"
)

// A saved host rule is what the proxy rules on for every session, so a rule
// that normalises to something other than the host a person typed would allow
// the wrong thing.
func FuzzNormalizeHostRule(f *testing.F) {
	for _, seed := range []string{
		"example.com", "*.example.com", ".example.com", "EXAMPLE.com.", "*.com", "https://x.com",
		"x.com:443", "a..b", "*", "", " ", "xn--bcher-kva.example", "a/b", "*.*.example.com",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		rule, err := NormalizeHostRule(raw)
		if err != nil {
			return
		}
		again, err := NormalizeHostRule(rule)
		if err != nil || again != rule {
			t.Fatalf("NormalizeHostRule(%q) = %q, which normalises again to %q, %v", raw, rule, again, err)
		}
		host := strings.TrimPrefix(rule, "*.")
		if strings.Contains(host, "*") {
			t.Fatalf("NormalizeHostRule(%q) = %q has a wildcard past its start", raw, rule)
		}
		for _, r := range host {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				t.Fatalf("NormalizeHostRule(%q) = %q contains %q", raw, rule, r)
			}
		}
		if strings.HasPrefix(rule, "*.") && !strings.Contains(host, ".") {
			t.Fatalf("NormalizeHostRule(%q) = %q covers a whole top-level domain", raw, rule)
		}
	})
}
