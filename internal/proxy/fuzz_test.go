package proxy

import (
	"encoding/base64"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// Fuzz tests for the parsing a guest controls: the request target, the
// headers that carry a session token, the host rules, and the address the
// loopback guard checks. `go test` runs the seeds; `make fuzz` explores.

func FuzzParseTarget(f *testing.F) {
	for _, seed := range []string{
		"example.com:443", "example.com", "[::1]:443", "[::1]", "EXAMPLE.COM.:80",
		"", ":", "[", "]:80", "a:b:c", " host :1", "[fe80::1%en0]:443", "\x00:1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := parseTarget(raw, "80")
		if err != nil {
			return
		}
		if got.host == "" || got.port == "" {
			t.Fatalf("parseTarget(%q) = %+v with an empty part and no error", raw, got)
		}
		if got.host != strings.ToLower(got.host) {
			t.Fatalf("parseTarget(%q) host %q is not lower case; policy matching would miss it", raw, got.host)
		}
		// The host the policy rules on must be the host that is dialled: the
		// address built from a target parses back to the same target.
		again, err := parseTarget(got.addr(), "80")
		if err != nil || again != got {
			t.Fatalf("parseTarget(%q) = %+v, but its address %q parses as %+v, %v", raw, got, got.addr(), again, err)
		}
	})
}

const fuzzToken = "fuzz-session-token"

// tokenOnlyLookup accepts exactly one token and records that it was never
// asked about an empty one.
func tokenOnlyLookup(t *testing.T) TokenLookup {
	return func(token string) (string, bool) {
		if token == "" {
			t.Fatal("TokenLookup was called with an empty token")
		}
		return "s1", token == fuzzToken
	}
}

func FuzzReverseAuthentication(f *testing.F) {
	f.Add("", "", "")
	f.Add(fuzzToken, "", "")
	f.Add("", fuzzToken, "Bearer "+fuzzToken)
	f.Add(" ", "Bearer", "Bearer  ")
	f.Add("", "", "Basic "+fuzzToken)
	f.Fuzz(func(t *testing.T, saintHeader, apiKey, authorization string) {
		h := http.Header{}
		h.Set(SaintSessionHeader, saintHeader)
		h.Set("x-api-key", apiKey)
		h.Set("Authorization", authorization)
		p := &Proxy{cfg: Config{TokenLookup: tokenOnlyLookup(t)}}
		_, ok := p.authenticate(h)
		if ok && !strings.Contains(saintHeader+"\n"+apiKey+"\n"+authorization, fuzzToken) {
			t.Fatalf("authenticated without the token: %q %q %q", saintHeader, apiKey, authorization)
		}
	})
}

func FuzzForwardTokens(f *testing.F) {
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	f.Add("", basic("saint:"+fuzzToken))
	f.Add("", basic(fuzzToken))
	f.Add("", basic(":"))
	f.Add("", "Basic !!!")
	f.Add("", "Bearer ")
	f.Add(fuzzToken, "Digest x")
	f.Fuzz(func(t *testing.T, saintHeader, proxyAuth string) {
		h := http.Header{}
		h.Set(SaintSessionHeader, saintHeader)
		h.Set("Proxy-Authorization", proxyAuth)
		for _, token := range forwardTokens(h) {
			if token == "" {
				t.Fatalf("forwardTokens offered an empty token for %q %q", saintHeader, proxyAuth)
			}
		}
	})
}

func FuzzMatchHostRule(f *testing.F) {
	for _, seed := range [][2]string{
		{"github.com", "github.com"}, {"*.github.com", "api.github.com"}, {".github.com", "github.com"},
		{"*.datadoghq.com", "evildatadoghq.com"}, {"*", "anything"}, {".", "x"}, {"*.", "x"}, {"", ""},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, rule, host string) {
		if !matchHostRule(rule, host) {
			return
		}
		r := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rule)), "*")
		if domain, wildcard := strings.CutPrefix(r, "."); wildcard {
			// A domain rule covers the domain and names under it, never a
			// name that merely ends in the same letters.
			if host != domain && !strings.HasSuffix(host, "."+domain) {
				t.Fatalf("rule %q matched %q, which is not %s or under it", rule, host, domain)
			}
			return
		}
		if host != r {
			t.Fatalf("rule %q matched %q, which is a different host", rule, host)
		}
	})
}

func FuzzGuardTarget(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1:443", "[::1]:443", "[::ffff:127.0.0.1]:443", "0.0.0.0:80", "[::]:80",
		"169.254.169.254:80", "[fe80::1%en0]:443", "93.184.216.34:443", "127.1:80", "localhost:80", "",
	} {
		f.Add(seed)
	}
	guard := guardTarget(false)
	f.Fuzz(func(t *testing.T, address string) {
		err := guard("tcp", address, nil)
		addrPort, perr := netip.ParseAddrPort(address)
		if perr != nil {
			if err == nil {
				t.Fatalf("an address the guard cannot parse, %q, was let through", address)
			}
			return
		}
		a := addrPort.Addr().Unmap()
		forbidden := a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified()
		if forbidden && err == nil {
			t.Fatalf("loopback or link-local %q was let through", address)
		}
	})
}
