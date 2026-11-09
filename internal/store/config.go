package store

import (
	"path/filepath"
	"strings"

	"github.com/montcao/saint/internal/sandbox"
)

type Config struct {
	DefaultRuntime     sandbox.Runtime `json:"default_runtime"`
	DefaultMemoryGB    int             `json:"default_memory_gb"`
	DefaultCPUs        int             `json:"default_cpus"`
	NonPrimaryReadOnly bool            `json:"non_primary_read_only"`
	ProxyPort          int             `json:"proxy_port"`

	// ProxyBind is the host address the proxy listens on. It is configurable
	// because the address a guest can reach the host at is a property of the
	// user's container networking, not a constant: guests see the host on the
	// bridge gateway, not on loopback. Empty means the safe default.
	ProxyBind string `json:"proxy_bind"`

	// ProxyNetwork is the container network whose gateway the proxy binds to
	// when ProxyBind is "auto". Empty means the runtime's default network.
	ProxyNetwork string `json:"proxy_network"`

	// ProxyAuth decides which credential a session's traffic carries, and
	// therefore which account pays for it. See ProxyAuthAuto.
	ProxyAuth string `json:"proxy_auth"`

	// MountRoots limits where a session's repos may come from. A repo has to sit
	// under one of these directories or saint refuses to mount it.
	//
	// Empty means the user's home directory, which is the useful default: it
	// keeps a guest confined to places the user actually keeps work, and out of
	// system directories, scratch space and other users' files.
	MountRoots []string `json:"mount_roots"`

	// RailOpen remembers whether the control rail was open, so it is where the
	// user left it.
	RailOpen bool `json:"rail_open,omitempty"`

	// RailCollapsed are the control rail sections folded away, by title.
	RailCollapsed []string `json:"rail_collapsed,omitempty"`

	// DefaultCredential is the credential the spawn form starts on. It is
	// remembered so a per-session choice does not become a per-session
	// question.
	DefaultCredential string `json:"default_credential,omitempty"`

	// Credentials maps a secret name to where it is stored, so a user can keep
	// secrets in 1Password or behind a script instead of the macOS keychain.
	// Names are CredentialAnthropic, CredentialOpenAI and
	// CredentialClaudeSubscription; an absent entry means the keychain.
	Credentials map[string]string `json:"credentials"`

	// Skills maps a skill name to where its content lives on disk, so it can be
	// swapped without touching every session that references it by name.
	//
	// Empty means "not installed." There is deliberately no fallback mode like
	// CredentialAPIKey - a skill has no lesser mode to fall back to, an
	// unresolvable name just isn't mounted, and Create refuses the session
	// rather than starting it silently short a skill it was asked for.
	Skills map[string]string `json:"skills,omitempty"`

	// DefaultSkills are mounted into every new session unless a request names
	// its own set. Empty means none. Unlike DefaultCredential there is no "auto"
	// concept worth guessing at here: handing every session a skill it did not
	// ask for is the silent scope creep the rest of this file argues against.
	DefaultSkills []string `json:"default_skills,omitempty"`

	// SkillsRoot is where every path in Skills must resolve to live under, and
	// the only directory a future `saint skills install` may write into. Empty
	// means ~/.saint/skills.
	//
	// This is the positive gate, playing the same role MountRoots plays for
	// repos: not a denylist of bad places, the one place a skill is ever
	// permitted to come from.
	SkillsRoot string `json:"skills_root,omitempty"`

	// ProxyLogRequests adds one journal record per request - method, path,
	// status and duration - on top of the usage records the journal always
	// carries. It is a separate switch because it is the only part of the
	// journal that costs anything on the request path: when it is off, nothing
	// wraps the proxy handler at all.
	ProxyLogRequests bool `json:"proxy_log_requests,omitempty"`

	// ProxyLogMaxBytes is the size at which the journal rotates. Zero means
	// DefaultProxyLogMaxBytes.
	ProxyLogMaxBytes int64 `json:"proxy_log_max_bytes,omitempty"`

	// ProxyLogKeepFiles is how many journal files are kept in total, the active
	// one included, so the journal occupies at most MaxBytes*KeepFiles on disk.
	// Zero means DefaultProxyLogKeepFiles.
	ProxyLogKeepFiles int `json:"proxy_log_keep_files,omitempty"`

	// ProxyGuestEgress points a guest's HTTP_PROXY and HTTPS_PROXY at saint,
	// so the traffic that is not model API traffic - git fetch, npm install, a
	// web-search tool, an update check - goes through the host's forward proxy
	// instead of straight out of the microVM.
	//
	// State the blast radius plainly: with this on, saint sees and records
	// every host a session's proxy-honouring HTTP traffic reaches. That is the
	// git remotes it pulls from, the registries it installs from, and the URLs
	// an agent fetches, by hostname and port, with byte counts. TLS is not
	// intercepted, so paths, headers and payloads stay unreadable - but the
	// list of who a session talked to is the user's own browsing-shaped record
	// of their work, and it lands in saint's journal on disk.
	//
	// It is also not an egress control, and must never be described as one.
	// The container keeps ordinary internet access either way: a statically
	// linked binary, anything with its own DNS or a non-HTTP protocol, and a
	// guest that simply unsets the variables all leave without passing through
	// here. What this buys is visibility into the traffic that honours the
	// convention, not containment.
	//
	// Off by default, for two independent reasons. Rerouting a user's traffic
	// through a recording proxy is not something to do because they installed
	// saint. And the failure mode is total: a guest pointed at a proxy that is
	// not accepting forward traffic has every clone and every install fail at
	// once, so this switch may only be on when the host side is serving it.
	ProxyGuestEgress bool `json:"proxy_guest_egress,omitempty"`

	// ProxyEgressEnforce turns the host policy from a record into a control.
	//
	// Off, saint records every host a session reaches and refuses none, which
	// is visibility and should not be described as anything more. On, a host
	// that is neither ruled on nor in the shipped allowlist is put to you, and
	// refused if you are not there to answer.
	//
	// It requires proxy_guest_egress, because a policy only binds the traffic
	// that crosses the proxy. A guest that was never pointed at saint is not
	// governed by this setting and saint says so rather than implying
	// otherwise.
	ProxyEgressEnforce bool `json:"proxy_egress_enforce,omitempty"`

	// ProxyAllowHosts and ProxyDenyHosts are the answers you have already
	// given. A denial is permanent and is never re-asked.
	ProxyAllowHosts []string `json:"proxy_allow_hosts,omitempty"`
	ProxyDenyHosts  []string `json:"proxy_deny_hosts,omitempty"`

	// ListStyle picks how the session list is drawn: "table" or "blocks".
	//
	// Empty means the table, so an existing config keeps the list it already
	// had. This is a flag rather than a replacement because which one reads
	// better is a question about a real fleet, and nobody can answer it from a
	// mockup.
	ListStyle string `json:"list_style,omitempty"`
}

const (
	// ProxyBindAuto binds the proxy to the container bridge gateway discovered
	// at runtime. This is the default because it is the only address a guest can
	// actually reach: bridging host loopback into a container requires admin
	// privileges (docs/research/spikes.md).
	//
	// It does widen the proxy's reach to every container on that bridge, not just
	// saint's. The per-session token is what stands between those containers and
	// the real API key, which is why ServeHTTP authenticates before it ever calls
	// keyFn. Do not relax that ordering.
	ProxyBindAuto = "auto"

	// ProxyBindLoopback keeps the proxy host-only. Guests cannot reach it.
	ProxyBindLoopback = "127.0.0.1"

	// DefaultProxyBind is what a fresh config gets.
	DefaultProxyBind = ProxyBindAuto

	// ProxyAuthKey keeps the real provider key on the host: the guest is given
	// only a per-session token, and the proxy swaps it for the key on the way
	// out. Best credential custody, and billed per token to the Console account
	// that owns the key.
	ProxyAuthKey = "key"

	// ProxyAuthSubscription bills a Claude subscription instead.
	//
	// This inverts the custody trade above and there is no way around it. A
	// subscription stays the active credential only when Claude Code holds it
	// itself and the gateway forwards it untouched; a gateway that injects its
	// own credential replaces the subscription and bills per token. So in this
	// mode the OAuth token lives in the guest and the proxy is an observer, not
	// a credential boundary.
	ProxyAuthSubscription = "subscription"

	// ProxyAuthAuto uses a subscription when one is configured and falls back to
	// ProxyAuthKey.
	//
	// Auto is the default because the two ways of guessing wrong are not
	// symmetric: defaulting to a subscription that is not wanted costs plan
	// limits, while defaulting to an API key that is not wanted costs money. The
	// resolved mode is shown by saint auth and saint doctor, so auto is
	// never silent about which account is paying.
	ProxyAuthAuto = "auto"

	// DefaultProxyAuth is what a fresh config gets.
	DefaultProxyAuth = ProxyAuthAuto

	// Credential names used as keys in Config.Credentials.
	// CredentialAPIKey selects key mode explicitly, as a choice rather than a
	// fallback, so it is never warned about.
	CredentialAPIKey = "api-key"

	// CredentialClaudeCode reads the host's existing Claude Code login live.
	// Mirrors credential.ClaudeCodeSpec.
	CredentialClaudeCode = "claude-code"

	CredentialAnthropic          = "anthropic"
	CredentialOpenAI             = "openai"
	CredentialClaudeSubscription = "claude_subscription"

	// DefaultProxyLogMaxBytes and DefaultProxyLogKeepFiles bound the request
	// journal at roughly 20 MiB. The predecessor of that journal was an
	// unrotated ~/.saint/saint.log that grew until the user noticed, so the
	// ceiling is the point: a diagnostic that can fill a disk is a fault, not a
	// feature.
	DefaultProxyLogMaxBytes  = 4 << 20
	DefaultProxyLogKeepFiles = 5

	// minProxyLogMaxBytes stops a hand-edited config from setting a size no
	// single record fits inside, which would rotate on every write and retain
	// nothing.
	minProxyLogMaxBytes = 4 << 10
)

// ProxyBindHost returns the configured bind address, falling back to the
// discovered-gateway sentinel.
func (c Config) ProxyBindHost() string {
	if strings.TrimSpace(c.ProxyBind) == "" {
		return DefaultProxyBind
	}
	return strings.TrimSpace(c.ProxyBind)
}

// ProxyBindIsAuto reports whether the bind address should be discovered.
func (c Config) ProxyBindIsAuto() bool {
	return strings.EqualFold(c.ProxyBindHost(), ProxyBindAuto)
}

// ProxyBindIsLoopback reports whether the proxy is pinned somewhere no guest can
// reach. Doctor surfaces this rather than silently rewriting the user's config.
func (c Config) ProxyBindIsLoopback() bool {
	host := strings.ToLower(c.ProxyBindHost())
	return host == ProxyBindLoopback || host == "localhost" || host == "::1"
}

// ProxyLogRequestsOrDefault reports whether per-request journal records are on.
//
// The field is plain JSON with omitempty, so false and absent are the same
// bytes on disk and cannot be told apart here. Absent therefore has to mean off:
// if it meant on, a user who turned it off would have the setting dropped from
// their config on the next save and find it back on. DefaultConfig writes it
// true instead, so a fresh install gets the full journal and an existing one
// keeps the usage records until it opts in.
func (c Config) ProxyLogRequestsOrDefault() bool {
	return c.ProxyLogRequests
}

// ProxyGuestEgressOrDefault reports whether guests are pointed at saint for
// their non-API HTTP traffic.
//
// Absent means off, and here that is not just the omitempty argument that
// applies to ProxyLogRequests - off is also the only safe reading. Every config
// file written before this field existed belongs to an install whose proxy is
// not serving forward traffic, and a guest pointed at a proxy that refuses to
// forward cannot clone or install anything at all.
func (c Config) ProxyGuestEgressOrDefault() bool {
	return c.ProxyGuestEgress
}

// ProxyLogMaxBytesOrDefault returns the size the journal rotates at.
func (c Config) ProxyLogMaxBytesOrDefault() int64 {
	if c.ProxyLogMaxBytes <= 0 {
		return DefaultProxyLogMaxBytes
	}
	if c.ProxyLogMaxBytes < minProxyLogMaxBytes {
		return minProxyLogMaxBytes
	}
	return c.ProxyLogMaxBytes
}

// ProxyLogKeepFilesOrDefault returns how many journal files are retained,
// counting the active one. One means no history at all, which is a legitimate
// choice and the floor: zero files would mean no journal, and the switch for
// that is ProxyLogRequests.
func (c Config) ProxyLogKeepFilesOrDefault() int {
	if c.ProxyLogKeepFiles <= 0 {
		return DefaultProxyLogKeepFiles
	}
	return c.ProxyLogKeepFiles
}

// ProxyNetworkOrDefault returns the network whose gateway the proxy binds to.
// ProxyAuthOrDefault reports the configured auth mode, defaulting when unset.
//
// An unrecognised value is treated as the default rather than as an error: a
// typo in config.json must not stop saint from starting, and doctor reports it.
func (c Config) ProxyAuthOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(c.ProxyAuth)) {
	case ProxyAuthKey:
		return ProxyAuthKey
	case ProxyAuthSubscription:
		return ProxyAuthSubscription
	default:
		return DefaultProxyAuth
	}
}

// ResolveProxyAuth reports which mode new Claude sessions will use, and why.
//
// One rule, in one place. It previously lived in three - the old TUI, the auth
// command and doctor - and a change to which credentials count as a
// subscription was applied to two of them, so the same machine reported
// "subscription" and "api key" depending on which screen you asked.
func ResolveProxyAuth(configured string, haveSubscription bool) (mode, why string) {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case ProxyAuthKey:
		return "api key", "pinned to key"
	case ProxyAuthSubscription:
		if haveSubscription {
			return "subscription", "billed to your Claude plan"
		}
		return "api key", "no subscription token is stored"
	default:
		if haveSubscription {
			return "subscription", "billed to your Claude plan"
		}
		return "api key", "no subscription token configured"
	}
}

// IsSubscriptionCredential reports whether a credential name can carry
// subscription billing.
func IsSubscriptionCredential(name string) bool {
	return name == CredentialClaudeSubscription || name == CredentialClaudeCode
}

// ProxyAuthIsValid reports whether proxy_auth holds a value saint understands.
func (c Config) ProxyAuthIsValid() bool {
	switch strings.ToLower(strings.TrimSpace(c.ProxyAuth)) {
	case "", ProxyAuthKey, ProxyAuthSubscription, ProxyAuthAuto:
		return true
	default:
		return false
	}
}

// MountRootsOrDefault reports the directories repos may be mounted from,
// defaulting to the user's home directory.
func (c Config) MountRootsOrDefault(home string) []string {
	roots := make([]string, 0, len(c.MountRoots))
	for _, root := range c.MountRoots {
		if trimmed := strings.TrimSpace(root); trimmed != "" {
			roots = append(roots, trimmed)
		}
	}
	if len(roots) == 0 && strings.TrimSpace(home) != "" {
		roots = append(roots, home)
	}
	return roots
}

// CredentialSpec reports where a named secret is stored. An empty result means
// the keychain, which is what Parse treats as the default.
func (c Config) CredentialSpec(name string) string {
	return strings.TrimSpace(c.Credentials[name])
}

// SkillPath reports where a named skill's content lives on disk. An empty
// result means the name is not configured at all.
func (c Config) SkillPath(name string) string {
	return strings.TrimSpace(c.Skills[name])
}

// DefaultSkillsOrEmpty reports the skills mounted into a session that does not
// name its own set. A copy is returned so a caller cannot mutate the config's
// slice through it.
func (c Config) DefaultSkillsOrEmpty() []string {
	return append([]string(nil), c.DefaultSkills...)
}

// SkillsRootOrDefault reports the directory every configured skill path must
// resolve to live under, defaulting to ~/.saint/skills.
func (c Config) SkillsRootOrDefault(home string) string {
	if root := strings.TrimSpace(c.SkillsRoot); root != "" {
		return root
	}
	return filepath.Join(home, ".saint", "skills")
}

func (c Config) ProxyNetworkOrDefault() string {
	if network := strings.TrimSpace(c.ProxyNetwork); network != "" {
		return network
	}
	return sandbox.DefaultNetwork
}

func DefaultConfig() Config {
	return Config{
		DefaultRuntime:     sandbox.RuntimeClaude,
		DefaultMemoryGB:    2,
		DefaultCPUs:        2,
		NonPrimaryReadOnly: true,
		ProxyBind:          DefaultProxyBind,
		ProxyAuth:          DefaultProxyAuth,
		ProxyNetwork:       sandbox.DefaultNetwork,
		ProxyLogRequests:   true,
		ProxyLogMaxBytes:   DefaultProxyLogMaxBytes,
		ProxyLogKeepFiles:  DefaultProxyLogKeepFiles,
	}
}

// List styles. The table is the shipped default; blocks is the alternative
// being evaluated, and exists so the two can be judged against real sessions
// rather than against a mockup.
const (
	ListStyleTable  = "table"
	ListStyleBlocks = "blocks"
)

// ProxyEgressEnforceOrDefault reports whether the host policy is enforced.
//
// Enforcement without egress proxying would be a claim with nothing behind it:
// the policy governs traffic that crosses the proxy, and a guest that was never
// pointed at saint does not. So it reads off unless both are on.
func (c Config) ProxyEgressEnforceOrDefault() bool {
	return c.ProxyEgressEnforce && c.ProxyGuestEgressOrDefault()
}

// ListStyleOrDefault reports how the session list is drawn. Anything
// unrecognised reads as the table, so a hand-edited config cannot produce a
// blank list.
func (c Config) ListStyleOrDefault() string {
	if c.ListStyle == ListStyleBlocks {
		return ListStyleBlocks
	}
	return ListStyleTable
}
