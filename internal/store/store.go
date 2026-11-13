package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/montcao/saint/internal/sandbox"
)

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

	// Network is the container network this session's microVM runs on, its
	// own internal network with no route out except the proxy. Empty means a
	// session created before sessions had their own network: it runs on the
	// shared network (Config.ProxyNetwork) with ordinary internet access.
	Network string `json:"network,omitempty"`

	// Herdr is the herdr pane showing this session right now, if any. The
	// proxy uses it to mark the pane blocked and to ask about a host there.
	// It is cleared when that pane detaches, and it may still be stale (a pane
	// killed outright), so a caller treats a failed herdr call as "no pane".
	Herdr *HerdrPane `json:"herdr,omitempty"`
}

// HerdrPane is how to reach one herdr pane from outside it: its ID, and the
// herdr binary and API socket of the server that owns it, because a named
// herdr session has a socket of its own.
type HerdrPane struct {
	PaneID     string `json:"pane_id"`
	SocketPath string `json:"socket_path,omitempty"`
	BinPath    string `json:"bin_path,omitempty"`
}

// ProxyAuthOrDefault reports a session's auth mode, defaulting for sessions
// created before the field existed.
func (s Session) ProxyAuthOrDefault() string {
	if s.ProxyAuth == ProxyAuthSubscription {
		return ProxyAuthSubscription
	}
	return ProxyAuthKey
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

	// ControlSocket is the running proxy's unix socket: panes ask it to listen
	// on a session's network and answer its questions about hosts through it.
	ControlSocket string

	// LockPath serialises read-modify-write updates to config.json and
	// sessions.json between processes (the proxy, panes and the CLI).
	LockPath string
}

type Store struct {
	paths Paths
}

func New(home string) *Store {
	return &Store{paths: DefaultPaths(home)}
}

func DefaultPaths(home string) Paths {
	return pathsAt(home, filepath.Join(home, ".saint"))
}

func pathsAt(home, root string) Paths {
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

		ControlSocket: filepath.Join(root, "proxy.sock"),
		LockPath:      filepath.Join(root, "state.lock"),
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
		return s.createConfig()
	}
	if err != nil {
		return Config{}, err
	}
	return s.ensureProxyPort(cfg)
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

// BootstrapErrorPath is where a session's init script leaves a reason when it
// gives up before the agent ever ran.
//
// It is inside the mount the guest can write, so it is untrusted text. It is
// read to be shown, never to be acted on.
func (s *Store) BootstrapErrorPath(id string) string {
	return filepath.Join(s.paths.SessionsDir, id, sandbox.BootstrapErrorFile)
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
