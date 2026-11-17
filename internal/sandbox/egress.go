package sandbox

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// This file is the guest half of the forward proxy. internal/proxy can accept
// CONNECT and absolute-form HTTP from a guest and record which hosts a session
// reached; nothing there tells a guest to use it. The convention that does is
// the proxy environment variables, and this is where saint writes them.
//
// Every new session sets it (CreateOptions.ProxyEgress), and every variable
// below is written only when it is on. There is no partial state: either the
// guest's environment is untouched and its traffic leaves the microVM the way
// it always did, or all of these are set and consistent with each other.

// guestProxyUser is the username half of the proxy URL's userinfo.
//
// The forward proxy reads the token from either half of a Basic credential, so
// this name is not load-bearing for authentication. It is there because a URL
// with a password and no user is malformed, and because a human reading a
// tool's error, a `container inspect`, or a proxy 407 should see what asked.
const guestProxyUser = "saint"

// guestEgressEnv builds the proxy environment for a guest, given the same
// proxyURL that already becomes ANTHROPIC_BASE_URL.
//
// Deriving from that argument is deliberate: the address a guest can reach the
// host at is discovered from the container network at startup (see
// GatewayAddr), and one discovery path that everything hangs off cannot drift.
// A second lookup here could disagree with the base URL the same container was
// given, which would be a session whose API traffic and whose git traffic
// believe the host is in two different places.
func guestEgressEnv(proxyURL, token string) ([]string, error) {
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("guest egress proxy: parse proxy url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("guest egress proxy: proxy url %q has no scheme and host to point the guest at", proxyURL)
	}

	proxy := url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	if token != "" {
		// The token travels as userinfo because that is the only channel a
		// forward proxy has: a CONNECT carries no application headers, so a
		// client's own Proxy-Authorization - which it derives from exactly this
		// URL - is how the session identifies itself. url.URL escapes the
		// userinfo on the way out, so a token containing a delimiter cannot
		// re-shape the URL.
		//
		// It goes in argv, via --env, the same as ANTHROPIC_API_KEY a few lines
		// up. That is a real exposure: ps(1) shows a process's argv to every
		// other process running as this user for the lifetime of the `container
		// run`. It is the established treatment for this value because the
		// value is saint's own per-session identity, not a provider
		// credential - anyone who reads it can impersonate one session to the
		// local proxy until it is deleted, and cannot learn the API key or the
		// subscription token behind it, which is the whole reason the proxy
		// holds those instead of the guest. The subscription token, which is a
		// real year-long credential, gets the env file instead. If the proxy
		// token ever becomes something an attacker could spend directly, both
		// places move together.
		proxy.User = url.UserPassword(guestProxyUser, token)
	}
	value := proxy.String()

	// Both cases, and this is not belt-and-braces.
	//
	// curl reads only the lowercase http_proxy, on purpose: an uppercase
	// HTTP_PROXY is indistinguishable from a CGI script's Proxy: request
	// header, so curl refuses to honour it (CVE-2016-5386, "httpoxy"). git
	// fetches over HTTP through libcurl, so lowercase alone decides whether a
	// clone is seen. Go, Node, Python and most everything else read the
	// uppercase name first and many read nothing else. Writing one case would
	// leave half of a session's tools quietly bypassing the proxy, which is the
	// failure this feature exists to remove - and the half that bypassed it
	// would still work, so nobody would find out.
	noProxy := guestNoProxy(parsed.Host)
	return []string{
		"HTTP_PROXY=" + value,
		"HTTPS_PROXY=" + value,
		"NO_PROXY=" + noProxy,
		"http_proxy=" + value,
		"https_proxy=" + value,
		"no_proxy=" + noProxy,
	}, nil
}

// redactURLUserinfo strips the credentials out of a URL while leaving the URL
// recognisable, so a log line still says where a guest was pointed.
//
// It works on the raw string rather than through net/url on purpose: this runs
// on the way to a log, and a value that fails to parse is exactly the value
// that must not be printed intact.
func redactURLUserinfo(value string) string {
	marker := strings.Index(value, "://")
	if marker < 0 {
		return value
	}
	start := marker + len("://")
	end := len(value)
	if cut := strings.IndexAny(value[start:], "/?#"); cut >= 0 {
		end = start + cut
	}
	authority := value[start:end]
	// Last, not first: an unescaped '@' inside a password is the case where
	// taking the first one would leave the tail of the secret in the log.
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return value
	}

	userinfo := authority[:at]
	user, _, hasPassword := strings.Cut(userinfo, ":")
	if !hasPassword {
		// A URL with only a username is a form the forward proxy accepts the
		// token in, so the username is a secret here too.
		user = ""
	}
	return value[:start] + user + ":<redacted>@" + value[start+at+1:]
}

// guestNoProxy lists the destinations a guest must reach directly.
//
// Both entries are correctness, not tidiness:
//
//   - The proxy's own host. The guest's ANTHROPIC_BASE_URL and OPENAI_BASE_URL
//     point at this same address, and a client that finds a proxy set sends
//     those requests to the proxy in absolute form. The forward path recognises
//     itself as the target and answers 508 Loop Detected, so without this entry
//     turning on egress proxying would break the agent's own API calls - the
//     one kind of traffic saint already handles.
//
//   - Loopback. The forward proxy refuses to tunnel to loopback by default, so
//     a guest's requests to a dev server it started itself would come back 403
//     for no reason a user could act on. Nothing is lost by exempting it: the
//     guest's 127.0.0.1 is inside the microVM and never reached the host.
//
// The host is written without its port. Every implementation that matches a
// NO_PROXY entry matches a bare host; support for host:port entries is not
// universal, and the port is not what needs distinguishing here.
func guestNoProxy(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	entries := []string{"localhost", "127.0.0.1", "::1"}
	if host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
		entries = append(entries, host)
	}
	return strings.Join(entries, ",")
}
