package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

// legacyProxyTokenFile is where per-session proxy tokens used to live: inside
// the session directory, which is mounted read-write into the guest at /saint.
//
// That was a mistake. The host treated a guest-writable file as the
// authoritative answer to "which session is this?", so a guest could rewrite the
// value the host would accept - including setting it to another session's token
// and taking over that session's identity in the logs and the metering.
//
// Tokens now live under Paths.TokensDir, which is never mounted. This constant
// remains only so existing installs can be migrated off it.
const legacyProxyTokenFile = "proxy-token"

// sessionIDPattern constrains what may be concatenated into a container name
// and handed to the container CLI. There is no shell involved, so this is not
// about command injection: it stops a crafted ID (a leading "-", say) from
// smuggling flags into `container exec/stop/start/rm`.
var sessionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidSessionID reports whether id is safe to use in paths and container names.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

type Session struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Repos     []sandbox.Repo  `json:"repos"`
	Runtime   sandbox.Runtime `json:"runtime"`
	MemoryGB  int             `json:"memory_gb"`
	CPUs      int             `json:"cpus"`
	CreatedAt time.Time       `json:"created_at"`
	LastRunAt time.Time       `json:"last_run_at"`

	// SkipPermissions records whether this session's agent runs without its
	// approval prompts.
	SkipPermissions bool `json:"skip_permissions,omitempty"`

	// Credential names the source this session's credential came from, using
	// the same specs as Config.Credentials. Empty means an API key.
	Credential string `json:"credential,omitempty"`

	// CredentialExpiresAt is when that credential stops working, when saint
	// knows. Zero means no known expiry, which is not a promise that it lasts
	// forever.
	CredentialExpiresAt time.Time `json:"credential_expires_at,omitempty"`

	// ProxyAuth records which credential this session's traffic carries, and so
	// which account pays for it. It is resolved once at create time and stored,
	// not recomputed: a running container's environment cannot change, and a
	// session must not appear to switch billing because the config did.
	ProxyAuth string `json:"proxy_auth"`

	// SkillHashes records, per skill name, the digest of what was actually
	// mounted at Create time - not what is on disk now. It exists so a later
	// question ("is this session still carrying the skill I reviewed?") is a
	// comparison, not a guess. See hashSkillTree.
	SkillHashes map[string]string `json:"skill_hashes,omitempty"`
}

// ProxyAuthOrDefault reports a session's auth mode, defaulting for sessions
// created before the field existed.
func (s Session) ProxyAuthOrDefault() string {
	if s.ProxyAuth == ProxyAuthSubscription {
		return ProxyAuthSubscription
	}
	return ProxyAuthKey
}

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
	// resolved mode is shown in the TUI header and by saint doctor, so auto is
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

	// DefaultProxyPort is the fallback when a config omits proxy_port. Without
	// it, a config written without the field would bind port 0 - an ephemeral
	// port nothing can find.
	DefaultProxyPort = 8787

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

// ProxyPortOrDefault returns the configured port, falling back when unset.
func (c Config) ProxyPortOrDefault() int {
	if c.ProxyPort <= 0 || c.ProxyPort > 65535 {
		return DefaultProxyPort
	}
	return c.ProxyPort
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
// One rule, in one place. It previously lived in three - the TUI, the auth
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

type Paths struct {
	Home         string
	Root         string
	SessionsDir  string
	SessionsPath string
	ConfigPath   string
	LogPath      string
	HooksDir     string

	// ProxyLogPath is the active request journal. Rotated files sit beside it
	// as ProxyLogPath.1 (newest) upwards.
	ProxyLogPath string

	// TokensDir holds the per-session proxy credentials. It is deliberately a
	// sibling of SessionsDir rather than a child: session directories are
	// mounted into guests, and nothing a guest can write may decide which
	// session the host thinks it is talking to.
	TokensDir string
}

type Store struct {
	paths Paths
}

func New(home string) *Store {
	return &Store{paths: DefaultPaths(home)}
}

func DefaultPaths(home string) Paths {
	root := filepath.Join(home, ".saint")
	return Paths{
		Home:         home,
		Root:         root,
		SessionsDir:  filepath.Join(root, "sessions"),
		SessionsPath: filepath.Join(root, "sessions.json"),
		ConfigPath:   filepath.Join(root, "config.json"),
		LogPath:      filepath.Join(root, "saint.log"),
		HooksDir:     filepath.Join(root, "hooks"),
		ProxyLogPath: filepath.Join(root, "proxy.jsonl"),
		TokensDir:    filepath.Join(root, "tokens"),
	}
}

func DefaultConfig() Config {
	return Config{
		DefaultRuntime:     sandbox.RuntimeClaude,
		DefaultMemoryGB:    2,
		DefaultCPUs:        2,
		NonPrimaryReadOnly: true,
		ProxyPort:          DefaultProxyPort,
		ProxyBind:          DefaultProxyBind,
		ProxyAuth:          DefaultProxyAuth,
		ProxyNetwork:       sandbox.DefaultNetwork,
		ProxyLogRequests:   true,
		ProxyLogMaxBytes:   DefaultProxyLogMaxBytes,
		ProxyLogKeepFiles:  DefaultProxyLogKeepFiles,
	}
}

func (s *Store) Paths() Paths {
	return s.paths
}

func (s *Store) Ensure() error {
	for _, dir := range []string{s.paths.Root, s.paths.SessionsDir, s.paths.HooksDir, s.paths.TokensDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}
	return nil
}

func (s *Store) EnsureConfig() (Config, error) {
	if err := s.Ensure(); err != nil {
		return Config{}, err
	}
	cfg, err := s.LoadConfig()
	if errors.Is(err, fs.ErrNotExist) {
		cfg = DefaultConfig()
		if err := s.SaveConfig(cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	return cfg, err
}

// LoadSessions returns the sessions whose IDs pass validation. Entries with an
// unusable ID are skipped; use LoadSessionsWithWarnings to surface that to the
// user.
func (s *Store) LoadSessions() ([]Session, error) {
	sessions, _, err := s.LoadSessionsWithWarnings()
	return sessions, err
}

// LoadSessionsWithWarnings reads sessions.json and drops any entry whose ID
// would be unsafe to concatenate into a container name or a filesystem path,
// returning one human-readable warning per rejected entry.
func (s *Store) LoadSessionsWithWarnings() ([]Session, []string, error) {
	if err := s.Ensure(); err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(s.paths.SessionsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Session{}, nil, nil
		}
		return nil, nil, fmt.Errorf("read sessions: %w", err)
	}
	var decoded []Session
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, nil, fmt.Errorf("decode sessions: %w", err)
	}

	sessions := make([]Session, 0, len(decoded))
	var warnings []string
	for _, session := range decoded {
		if !ValidSessionID(session.ID) {
			warnings = append(warnings, fmt.Sprintf("skipped session with invalid id %s", quoteTruncated(session.ID, 32)))
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, warnings, nil
}

// quoteTruncated renders untrusted text for display: %q escapes control
// characters and the length cap keeps a hostile value from flooding the UI.
func quoteTruncated(value string, maxLen int) string {
	if len(value) > maxLen {
		return fmt.Sprintf("%q...", value[:maxLen])
	}
	return fmt.Sprintf("%q", value)
}

func (s *Store) SaveSessions(sessions []Session) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	return writeJSONAtomic(s.paths.SessionsPath, sessions, 0o600)
}

func (s *Store) LoadConfig() (Config, error) {
	if err := s.Ensure(); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(s.paths.ConfigPath)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}

func (s *Store) SaveConfig(cfg Config) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	return writeJSONAtomic(s.paths.ConfigPath, cfg, 0o600)
}

func (s *Store) EnsureSessionDir(id string) (string, error) {
	if !ValidSessionID(id) {
		return "", fmt.Errorf("invalid session id %s", quoteTruncated(id, 32))
	}
	if err := s.Ensure(); err != nil {
		return "", err
	}
	path := filepath.Join(s.paths.SessionsDir, id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create session dir: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("chmod session dir: %w", err)
	}
	return path, nil
}

func (s *Store) TranscriptPath(id string) string {
	return filepath.Join(s.paths.SessionsDir, id, "transcript.log")
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

// BootstrapErrorPath is where a session's init script leaves a reason when it
// gives up before the agent ever ran.
//
// It is inside the mount the guest can write, so it is untrusted text. It is
// read to be shown, never to be acted on.
func (s *Store) BootstrapErrorPath(id string) string {
	return filepath.Join(s.paths.SessionsDir, id, sandbox.BootstrapErrorFile)
}

// ProxyTokenPath is the host-authoritative location of a session's token. It is
// outside every mount, so only the host can write it.
func (s *Store) ProxyTokenPath(id string) string {
	return filepath.Join(s.paths.TokensDir, id)
}

// legacyProxyTokenPath is the guest-writable location tokens used to live in.
func (s *Store) legacyProxyTokenPath(id string) string {
	return filepath.Join(s.paths.SessionsDir, id, legacyProxyTokenFile)
}

// EnsureProxyToken returns the session's proxy credential, creating it on first
// call and returning the same value on every call after that.
//
// The token is what the guest presents to the local proxy in place of a real
// provider key: the proxy authenticates it, strips it, and injects the user's
// actual key. It lives at 0600 inside the session directory, which is mounted
// into that one guest at /saint.
func (s *Store) EnsureProxyToken(id string) (string, error) {
	if _, err := s.EnsureSessionDir(id); err != nil {
		return "", err
	}
	path := s.ProxyTokenPath(id)

	if err := s.migrateProxyToken(id); err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if token := strings.TrimSpace(string(data)); token != "" {
			// Repair permissions on a token written before this ran, or loosened since.
			if err := os.Chmod(path, 0o600); err != nil {
				return "", fmt.Errorf("chmod proxy token: %w", err)
			}
			return token, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read proxy token: %w", err)
	}

	token, err := newProxyToken()
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write proxy token: %w", err)
	}
	return token, nil
}

// migrateProxyToken moves a token out of the guest-writable session directory.
//
// The value is carried over rather than replaced so a container that is already
// running keeps working: its environment holds the old token and cannot be
// changed without recreating it. A carried-over value may have been tampered
// with by that guest, which is why LoadProxyTokens refuses to serve duplicates -
// the one thing tampering could achieve is colliding with another session.
func (s *Store) migrateProxyToken(id string) error {
	legacy := s.legacyProxyTokenPath(id)
	data, err := os.ReadFile(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy proxy token: %w", err)
	}

	current := s.ProxyTokenPath(id)
	if _, err := os.Stat(current); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat proxy token: %w", err)
		}
		if token := strings.TrimSpace(string(data)); token != "" {
			if err := writeAtomic(current, []byte(token+"\n"), 0o600); err != nil {
				return fmt.Errorf("migrate proxy token: %w", err)
			}
		}
	}

	// Remove the guest-writable copy either way. Leaving it behind leaves a
	// second, weaker source of the same secret lying inside a mount.
	if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove legacy proxy token: %w", err)
	}
	return nil
}

// MigrateProxyTokens moves every existing session's token out of the
// guest-writable session directory.
//
// EnsureProxyToken migrates on the way past, but it only runs when a session is
// created. Without this, sessions that already existed keep their token in the
// old location, where LoadProxyTokens no longer looks - so the proxy rejects
// them and their agents fail every request.
func (s *Store) MigrateProxyTokens() error {
	if err := s.Ensure(); err != nil {
		return err
	}
	sessions, err := s.LoadSessions()
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if !ValidSessionID(session.ID) {
			continue
		}
		if err := s.migrateProxyToken(session.ID); err != nil {
			return err
		}
	}
	return nil
}

// RemoveProxyToken deletes a session's credential.
//
// Tokens no longer live in the session directory, so deleting that directory no
// longer takes the token with it.
func (s *Store) RemoveProxyToken(id string) error {
	if !ValidSessionID(id) {
		return fmt.Errorf("invalid session id %s", quoteTruncated(id, 32))
	}
	if err := os.Remove(s.ProxyTokenPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove proxy token: %w", err)
	}
	return nil
}

// LoadProxyTokens maps session ID to proxy token for every session that has
// one. Sessions without a token file simply cannot authenticate.
func (s *Store) LoadProxyTokens() (map[string]string, error) {
	sessions, err := s.LoadSessions()
	if err != nil {
		return nil, err
	}
	tokens := make(map[string]string, len(sessions))
	seen := make(map[string]string, len(sessions))
	for _, session := range sessions {
		data, err := os.ReadFile(s.ProxyTokenPath(session.ID))
		if err != nil {
			continue
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			continue
		}
		// Two sessions sharing a token cannot happen when the host mints them,
		// so it means a token migrated from the old guest-writable location was
		// tampered with. Serve neither: a request attributed to the wrong
		// session is worse than a request that fails.
		if other, clash := seen[token]; clash {
			delete(tokens, other)
			continue
		}
		seen[token] = session.ID
		tokens[session.ID] = token
	}
	return tokens, nil
}

func newProxyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	data = append(data, '\n')
	return writeAtomic(path, data, mode)
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	defer cleanup()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
