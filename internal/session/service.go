package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

type CreateRequest struct {
	Name     string
	Repos    []sandbox.Repo
	Runtime  sandbox.Runtime
	Prompt   string
	Resume   bool
	MemoryGB int
	CPUs     int

	// Credential names which credential this session should carry, using the
	// same specs as ~/.saint/config.json. Empty means the configured default.
	Credential string

	// SkipPermissions runs the agent without its approval prompts.
	SkipPermissions bool

	// Skills names which configured skills to mount for this session. Empty
	// means cfg.DefaultSkillsOrEmpty(), not "none" - a request has to name an
	// empty slice explicitly to opt a session out of the defaults, and today
	// there is no way to do that, matching how DefaultSkills itself works.
	Skills []string
}

type Service struct {
	Store    *store.Store
	Sandbox  sandbox.Sandbox
	Workdir  string
	ProxyURL string
	Now      func() time.Time

	// Credentials resolves a credential spec to a source. Nil means no
	// subscription is available and every session falls back to key mode
	// rather than failing.
	Credentials func(spec string) (credential.Source, error)

	// Logf reports non-fatal decisions, such as falling back from subscription
	// billing to API billing.
	Logf func(string, ...any)
}

// resolveAuth decides which credential a new session will carry.
//
// Only Claude sessions can use a subscription: there is no equivalent for the
// OpenAI runtime, so Codex always uses key mode regardless of configuration.
// A configured subscription that cannot be read falls back to key mode with a
// warning rather than refusing to start a session - but the fallback is never
// silent, because the two modes bill different accounts.
func (s *Service) resolveAuth(ctx context.Context, cfg store.Config, rt sandbox.Runtime, chosen string) authChoice {
	configured := cfg.ProxyAuthOrDefault()

	if rt != sandbox.RuntimeClaude {
		return authChoice{Mode: sandbox.AuthKey}
	}
	if configured == store.ProxyAuthKey {
		return authChoice{Mode: sandbox.AuthKey}
	}

	// The order is: what this session asked for, then the configured default,
	// then the subscription slot.
	//
	// The default used to be skipped entirely, which made CreateRequest's own
	// documentation false - "empty means the configured default" resolved to
	// the subscription slot instead. On an install whose default is the live
	// Claude Code login and whose subscription slot is empty, that silently
	// produced a key-mode session; and because "auto" suppresses the fallback
	// warning, nothing said so. The agent then failed on its first request with
	// an error pointing at the proxy rather than at the credential.
	spec := strings.TrimSpace(chosen)
	if spec == "" {
		spec = strings.TrimSpace(cfg.DefaultCredential)
	}
	if spec == "" {
		spec = cfg.CredentialSpec(store.CredentialClaudeSubscription)
	}

	// Explicitly choosing the API key is a decision, not a fallback, so it
	// never warns.
	if spec == store.CredentialAPIKey {
		return authChoice{Mode: sandbox.AuthKey}
	}

	// Only an explicit subscription setting is worth warning about when it
	// cannot be honoured. Under "auto", key mode is the expected outcome.
	warn := func(format string, args ...any) authChoice {
		choice := authChoice{Mode: sandbox.AuthKey}
		if configured == store.ProxyAuthSubscription {
			choice.Warning = fmt.Sprintf(format, args...)
		}
		return choice
	}

	if s.Credentials == nil {
		return warn("no credential source is configured; this session will bill API rates")
	}
	source, err := s.Credentials(spec)
	if err != nil {
		return warn("credential %q is unusable (%v); this session will bill API rates", spec, err)
	}

	// A credential with minutes left would start the session and then strand it
	// mid-run with an authentication error the user cannot trace. Check before
	// spending anything.
	var expires time.Time
	if inspector, ok := source.(credential.Inspector); ok {
		status, err := inspector.Status(ctx)
		switch {
		case err != nil:
			return warn("credential %q could not be read (%v); this session will bill API rates", spec, err)
		case !status.Present:
			return warn("no credential is stored for %q; this session will bill API rates", spec)
		}
		expires = status.ExpiresAt

		if left := status.Remaining(s.now()); !status.ExpiresAt.IsZero() && left < MinCredentialLife {
			return authChoice{
				Mode: sandbox.AuthKey,
				Warning: fmt.Sprintf(
					"the %s has %s left, under the %s minimum; this session will bill API rates instead of being stranded mid-run",
					source.Describe(), left.Round(time.Minute), MinCredentialLife,
				),
			}
		}
	}

	value, err := source.Get(ctx)
	if err != nil || strings.TrimSpace(value) == "" {
		return warn("credential %q is unavailable (%v); this session will bill API rates", spec, err)
	}
	return authChoice{
		Mode:      sandbox.AuthSubscription,
		Token:     value,
		Source:    spec,
		ExpiresAt: expires,
	}
}

// MinCredentialLife is how much life a credential must have left before saint
// will start a session with it.
//
// The host's Claude Code login is refreshed on a cycle of hours, and saint
// reads it at creation - so a session does not get a token's full lifetime, it
// gets whatever is left. Below this floor the session would very likely outlive
// its credential.
const MinCredentialLife = 15 * time.Minute

// authChoice is what a session ended up carrying, and why.
type authChoice struct {
	Mode      string
	Token     string
	Source    string
	ExpiresAt time.Time
	Warning   string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (store.Session, error) {
	if s.Store == nil || s.Sandbox == nil {
		return store.Session{}, errors.New("service is not configured")
	}

	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return store.Session{}, err
	}

	repos, err := s.normalizeRepos(req.Repos)
	if err != nil {
		return store.Session{}, err
	}

	skillNames := req.Skills
	if len(skillNames) == 0 {
		skillNames = cfg.DefaultSkillsOrEmpty()
	}
	skillMounts, err := s.resolveSkills(cfg, skillNames)
	if err != nil {
		return store.Session{}, err
	}

	rt := req.Runtime
	if rt == "" {
		rt = cfg.DefaultRuntime
	}
	if rt == "" {
		rt = sandbox.RuntimeClaude
	}

	cpus := req.CPUs
	if cpus <= 0 {
		cpus = cfg.DefaultCPUs
	}
	if cpus <= 0 {
		cpus = 2
	}

	memoryGB := req.MemoryGB
	if memoryGB <= 0 {
		memoryGB = cfg.DefaultMemoryGB
	}
	if memoryGB <= 0 {
		memoryGB = 2
	}

	id, err := newSessionID()
	if err != nil {
		return store.Session{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(repos[0].Path)
	}

	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}

	session := store.Session{
		ID:        id,
		Name:      name,
		Repos:     repos,
		Runtime:   rt,
		MemoryGB:  memoryGB,
		CPUs:      cpus,
		CreatedAt: now,
		LastRunAt: now,

		SkipPermissions: req.SkipPermissions,
	}

	sessionDir, err := s.Store.EnsureSessionDir(session.ID)
	if err != nil {
		return store.Session{}, err
	}

	// The guest needs a credential for the local proxy before it starts. The
	// token also lands in the session dir, which is mounted at /saint.
	proxyToken, err := s.Store.EnsureProxyToken(session.ID)
	if err != nil {
		_ = os.RemoveAll(sessionDir)
		return store.Session{}, err
	}

	auth := s.resolveAuth(ctx, cfg, session.Runtime, req.Credential)
	if auth.Warning != "" && s.Logf != nil {
		s.Logf("session %s: %s", session.ID, auth.Warning)
	}
	session.ProxyAuth = auth.Mode
	session.Credential = auth.Source
	session.CredentialExpiresAt = auth.ExpiresAt

	if len(skillMounts) > 0 {
		session.SkillHashes = make(map[string]string, len(skillMounts))
		for _, m := range skillMounts {
			session.SkillHashes[m.Name] = m.Hash
		}
	}

	createOpts := sandbox.CreateOptions{
		Resume:            req.Resume,
		Prompt:            req.Prompt,
		CPUs:              session.CPUs,
		MemoryGB:          session.MemoryGB,
		Name:              session.Name,
		ProxyToken:        proxyToken,
		ProxyEgress:       cfg.ProxyGuestEgressOrDefault(),
		SkipPermissions:   req.SkipPermissions,
		AuthMode:          auth.Mode,
		SubscriptionToken: auth.Token,
		SkillMounts:       skillMounts,
	}
	if err := s.Sandbox.Create(ctx, containerName(session.ID), session.Runtime, session.Repos, sessionDir, s.ProxyURL, createOpts); err != nil {
		_ = os.RemoveAll(sessionDir)
		return store.Session{}, err
	}

	sessions, err := s.Store.LoadSessions()
	if err != nil {
		_ = s.Sandbox.Remove(ctx, containerName(session.ID))
		_ = os.RemoveAll(sessionDir)
		return store.Session{}, err
	}
	sessions = append(sessions, session)
	if err := s.Store.SaveSessions(sessions); err != nil {
		_ = s.Sandbox.Remove(ctx, containerName(session.ID))
		_ = os.RemoveAll(sessionDir)
		return store.Session{}, err
	}

	return session, nil
}

func (s *Service) Start(ctx context.Context, id string) error {
	if err := s.Sandbox.Start(ctx, containerName(id)); err != nil {
		return err
	}
	return s.touch(id)
}

func (s *Service) Stop(ctx context.Context, id string) error {
	return s.Sandbox.Stop(ctx, containerName(id))
}

func (s *Service) Delete(ctx context.Context, id string) error {
	_ = s.Sandbox.Remove(ctx, containerName(id))

	sessions, err := s.Store.LoadSessions()
	if err != nil {
		return err
	}

	filtered := sessions[:0]
	for _, session := range sessions {
		if session.ID == id {
			continue
		}
		filtered = append(filtered, session)
	}
	if err := s.Store.SaveSessions(filtered); err != nil {
		return err
	}

	// The token lives outside the session directory now, so removing that
	// directory no longer takes the credential with it.
	if err := s.Store.RemoveProxyToken(id); err != nil {
		return err
	}

	sessionDir := filepath.Join(s.Store.Paths().SessionsDir, id)
	return os.RemoveAll(sessionDir)
}

func (s *Service) AttachCmd(id string) (*exec.Cmd, error) {
	return s.Sandbox.AttachCmd(containerName(id), s.attachOptions(id))
}

// attachOptions describe how to rebuild a session whose tmux server has died.
//
// Best effort: a session record saint cannot read still attaches, it just comes
// back as a plain shell in the container's default directory rather than a
// running agent.
func (s *Service) attachOptions(id string) sandbox.AttachOptions {
	if s.Store == nil {
		return sandbox.AttachOptions{}
	}
	sessions, err := s.Store.LoadSessions()
	if err != nil {
		return sandbox.AttachOptions{}
	}
	for _, session := range sessions {
		if session.ID != id || len(session.Repos) == 0 {
			continue
		}
		workdir := session.Repos[0].Path
		opts := sandbox.AttachOptions{Workdir: workdir}
		if command, err := sandbox.AgentCommand(session.Runtime, workdir); err == nil {
			opts.Command = command
		}
		return opts
	}
	return sandbox.AttachOptions{}
}

func (s *Service) ContainerName(id string) string {
	return containerName(id)
}

func (s *Service) touch(id string) error {
	sessions, err := s.Store.LoadSessions()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}

	for i := range sessions {
		if sessions[i].ID != id {
			continue
		}
		sessions[i].LastRunAt = now
		return s.Store.SaveSessions(sessions)
	}
	return fmt.Errorf("session %q not found", id)
}

func (s *Service) normalizeRepos(repos []sandbox.Repo) ([]sandbox.Repo, error) {
	if len(repos) == 0 {
		if strings.TrimSpace(s.Workdir) == "" {
			return nil, errors.New("at least one repo is required")
		}
		repos = []sandbox.Repo{{Path: s.Workdir}}
	}

	normalized := make([]sandbox.Repo, 0, len(repos))
	seen := make(map[string]struct{}, len(repos))
	for i, repo := range repos {
		resolved, err := s.normalizeRepoPath(repo.Path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		normalized = append(normalized, sandbox.Repo{
			Path:     resolved,
			ReadOnly: i > 0 && repo.ReadOnly,
		})
	}
	return normalized, nil
}

// blockedSystemRoots are directories no session may mount, whatever the mount
// roots say. They are defence in depth: the allowlist in checkMountRoot is the
// real bound, and these only catch a mount root configured too widely.
//
// Paths are resolved before this check, so the macOS /tmp -> /private/tmp
// symlink is covered by naming both. Bare /private and /var are deliberately
// absent: on macOS they contain the per-user temporary directories, so denying
// them outright would reject legitimate paths on systems that put a home there.
// Those are excluded by the allowlist instead.
var blockedSystemRoots = []string{
	"/etc", "/usr", "/bin", "/sbin", "/proc", "/sys", "/dev",
	"/System", "/Library", "/Applications", "/opt", "/nix", "/Network", "/cores",
	"/tmp", "/private/tmp", "/private/etc", "/private/var/root", "/var/root",
}

// blockedHomeDirs are home subdirectories that hold credentials or tool state
// rather than work.
var blockedHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".config", ".claude", ".cursor", ".docker",
	".kube", ".gcloud", ".azure", ".npm", ".gem", ".cargo", "Library",
}

// resolveOrSelf resolves symlinks, falling back to the original path when it
// cannot - a path that does not exist is handled by the caller's own checks.
func resolveOrSelf(path string) string {
	if actual, err := filepath.EvalSymlinks(path); err == nil {
		return actual
	}
	return path
}

// checkMountRoot enforces that a repo lives under a configured mount root.
func (s *Service) checkMountRoot(resolved string) error {
	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return err
	}
	home := resolveOrSelf(s.Store.Paths().Home)

	roots := cfg.MountRootsOrDefault(home)
	if len(roots) == 0 {
		return errors.New("no mount roots are configured and the home directory is unknown")
	}

	for _, root := range roots {
		expanded := root
		if expanded == "~" {
			expanded = home
		} else if strings.HasPrefix(expanded, "~/") {
			expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
		}
		// Resolve the root the same way the repo was, so a symlinked root still
		// matches rather than silently rejecting everything under it.
		expanded = resolveOrSelf(expanded)
		if underPath(resolved, expanded) {
			return nil
		}
	}

	return fmt.Errorf(
		"repo path %q is outside every mount root (%s); add its parent to \"mount_roots\" in ~/.saint/config.json to allow it",
		resolved, strings.Join(roots, ", "),
	)
}

func (s *Service) normalizeRepoPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("repo path contains NUL")
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = s.Workdir
	}
	if raw == "" {
		return "", errors.New("repo path is required")
	}

	home := s.Store.Paths().Home
	if raw == "~" {
		raw = home
	} else if strings.HasPrefix(raw, "~/") {
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}

	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %q: %w", raw, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}

	// Deliberately no version-control requirement. An agent should be able to
	// work in any directory - a scratch prototype, a tarball, a checkout of
	// something that predates git - and the microVM is what makes that safe.
	// What bounds a session is where the directory is, not what is inside it.

	// Compare against the resolved home. The repo path has been through
	// EvalSymlinks, so an unresolved home would silently fail to match on any
	// system where the home directory is reached through a symlink - and every
	// check below would quietly pass.
	home = resolveOrSelf(home)

	if resolved == string(filepath.Separator) {
		return "", errors.New("repo path cannot be /")
	}
	if samePath(resolved, home) {
		return "", errors.New("repo path cannot be your home directory")
	}
	if underPath(resolved, s.Store.Paths().Root) {
		return "", errors.New("repo path cannot be inside ~/.saint")
	}

	// Credential and tool-state directories are never work, and a guest that can
	// read them can act as the user everywhere else. Checked before the system
	// roots so the message says which of the two problems it is.
	for _, name := range blockedHomeDirs {
		sensitive := filepath.Join(home, name)
		if samePath(resolved, sensitive) || underPath(resolved, sensitive) {
			return "", fmt.Errorf("repo path %q holds credentials or tool state and cannot be mounted into a session", resolved)
		}
	}

	for _, blocked := range blockedSystemRoots {
		if samePath(resolved, blocked) || underPath(resolved, blocked) {
			return "", fmt.Errorf("repo path %q is a system directory and cannot be mounted into a session", resolved)
		}
	}

	// The positive gate. Everything above is a denylist, and denylists leak;
	// this is the rule that actually bounds what a guest can ever see.
	if err := s.checkMountRoot(resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

// resolveSkills turns requested skill names into validated, hashed mounts.
// It is the skills-side counterpart to normalizeRepos: a refusal here means
// the session does not start, the same as an unmountable repo.
func (s *Service) resolveSkills(cfg store.Config, names []string) ([]sandbox.SkillMount, error) {
	if len(names) == 0 {
		return nil, nil
	}
	home := resolveOrSelf(s.Store.Paths().Home)
	root := resolveOrSelf(cfg.SkillsRootOrDefault(home))

	mounts := make([]sandbox.SkillMount, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if err := sandbox.ValidateSkillName(name); err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		configured := cfg.SkillPath(name)
		if configured == "" {
			return nil, fmt.Errorf("skill %q is not configured", name)
		}

		resolved, err := normalizeSkillPath(configured)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}
		if !underPath(resolved, root) {
			return nil, fmt.Errorf(
				"skill %q resolves to %q, outside skills_root (%s)",
				name, resolved, root)
		}

		hash, err := hashSkillTree(resolved)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", name, err)
		}

		mounts = append(mounts, sandbox.SkillMount{
			Name: name,
			Path: resolved,
			Hash: hash,
		})
	}
	return mounts, nil
}

func normalizeSkillPath(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %q: %w", raw, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return resolved, nil
}

// hashSkillTree returns a deterministic digest of a skill directory's
// content, independent of file order or OS-reported timestamps - only path
// and byte content affect the result, so the same skill hashes the same on
// every machine that has it.
func hashSkillTree(dir string) (string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// A symlink inside a skill directory can point anywhere on the host,
		// including outside SkillsRoot - hashing its target would hash
		// content nobody validated as a skill at all. Reject rather than
		// follow, the same posture blockedSystemRoots takes toward paths
		// that reach outside their own boundary.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("skill contains a symlink at %q, which is not allowed", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		entries = append(entries, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SkillDrift reports, for a running session, which of its mounted skills no
// longer match what was hashed at Create time. It never re-mounts anything -
// see RenewCredential's own note that a running guest cannot be handed a new
// environment - it only tells the user a renewal (new container) would pick
// up a change.
func (s *Service) SkillDrift(session store.Session) (map[string]bool, error) {
	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return nil, err
	}
	drift := make(map[string]bool, len(session.SkillHashes))
	for name, was := range session.SkillHashes {
		path := cfg.SkillPath(name)
		if path == "" {
			drift[name] = true
			continue
		}
		resolved, err := normalizeSkillPath(path)
		if err != nil {
			drift[name] = true
			continue
		}
		now, err := hashSkillTree(resolved)
		if err != nil {
			drift[name] = true
			continue
		}
		drift[name] = now != was
	}
	return drift, nil
}

func containerName(id string) string {
	return "saint-" + id
}

func newSessionID() (string, error) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func underPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// RenewCredential gives a session a freshly read credential.
//
// It exists because a session's credential is a copy, not a reference. In
// subscription mode the token is written into the container's environment when
// the container is created, and a process environment cannot be changed
// afterwards - so the host's Claude Code login refreshing every few hours does
// nothing for a session already running on a copy taken hours ago. The host
// login can read "2h left" while a session's own token is genuinely expired,
// and both numbers are correct about different things.
//
// The only way to hand a running guest a new environment is to give it a new
// container, so that is what this does. It is deliberately not silent about the
// cost: the microVM is replaced, so whatever the agent held in memory - its
// conversation, its shell history, anything written outside the mounted
// repositories - is gone. Work on disk survives, because the repositories are
// host mounts and were never inside the microVM.
//
// The session record, its name, repositories, resources and permission mode are
// all preserved, so this is a renewal rather than a new session.
func (s *Service) RenewCredential(ctx context.Context, id string) (store.Session, error) {
	sessions, err := s.Store.LoadSessions()
	if err != nil {
		return store.Session{}, err
	}

	idx := -1
	for i, candidate := range sessions {
		if candidate.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return store.Session{}, fmt.Errorf("session %q not found", id)
	}
	session := sessions[idx]

	cfg, err := s.Store.EnsureConfig()
	if err != nil {
		return store.Session{}, err
	}

	// Resolve before removing anything. A credential that cannot be read is a
	// reason to leave the session exactly as it was, not to replace a working
	// container with a broken one.
	auth := s.resolveAuth(ctx, cfg, session.Runtime, session.Credential)

	// Renewing must not quietly change which account pays.
	//
	// resolveAuth falls back to key mode whenever a subscription credential
	// cannot be read, which is the right answer when a session is being created
	// - a session that bills the API is better than no session. It is the wrong
	// answer here: the user pressed a key to repair a credential, not to move
	// this session onto per-token billing, and the fallback is silent under
	// "auto". Refusing leaves the session exactly as it was, which is the
	// outcome that can still be undone.
	if session.ProxyAuthOrDefault() == store.ProxyAuthSubscription && auth.Mode != sandbox.AuthSubscription {
		reason := auth.Warning
		if reason == "" {
			reason = "no subscription credential could be read"
		}
		return store.Session{}, fmt.Errorf(
			"not renewing: %s, and renewing would move this session onto API billing", reason)
	}
	if auth.Mode == sandbox.AuthSubscription && strings.TrimSpace(auth.Token) == "" {
		return store.Session{}, errors.New("no usable credential to renew with")
	}
	if auth.Warning != "" && s.Logf != nil {
		s.Logf("session %s: %s", session.ID, auth.Warning)
	}

	// Skills are re-resolved the same way, and for the same reason: renewing
	// must not quietly hand the guest skill content nobody reviewed for this
	// session. A credential has a fallback (key mode) when it cannot be
	// renewed cleanly; a skill does not - drift means refusing, the same
	// outcome the auth check above reaches by a different path, and for the
	// same reason: the session is left exactly as it was, which can still be
	// undone.
	var skillMounts []sandbox.SkillMount
	if len(session.SkillHashes) > 0 {
		names := make([]string, 0, len(session.SkillHashes))
		for name := range session.SkillHashes {
			names = append(names, name)
		}
		mounts, err := s.resolveSkills(cfg, names)
		if err != nil {
			return store.Session{}, fmt.Errorf("not renewing: %w", err)
		}
		for _, m := range mounts {
			if was := session.SkillHashes[m.Name]; was != m.Hash {
				return store.Session{}, fmt.Errorf(
					"not renewing: skill %q has changed on disk since this session was created; review the change and create a new session to pick it up", m.Name)
			}
		}
		skillMounts = mounts
	}

	sessionDir, err := s.Store.EnsureSessionDir(session.ID)
	if err != nil {
		return store.Session{}, err
	}
	proxyToken, err := s.Store.EnsureProxyToken(session.ID)
	if err != nil {
		return store.Session{}, err
	}

	name := containerName(session.ID)
	if err := s.Sandbox.Remove(ctx, name); err != nil {
		return store.Session{}, fmt.Errorf("replace container: %w", err)
	}

	createOpts := sandbox.CreateOptions{
		CPUs:              session.CPUs,
		MemoryGB:          session.MemoryGB,
		Name:              session.Name,
		ProxyToken:        proxyToken,
		SkipPermissions:   session.SkipPermissions,
		AuthMode:          auth.Mode,
		SubscriptionToken: auth.Token,
		ProxyEgress:       cfg.ProxyGuestEgressOrDefault(),
		SkillMounts:       skillMounts,
	}
	if err := s.Sandbox.Create(ctx, name, session.Runtime, session.Repos, sessionDir, s.ProxyURL, createOpts); err != nil {
		return store.Session{}, fmt.Errorf("recreate session: %w", err)
	}

	session.ProxyAuth = auth.Mode
	session.Credential = auth.Source
	session.CredentialExpiresAt = auth.ExpiresAt
	session.LastRunAt = s.now()
	sessions[idx] = session
	if err := s.Store.SaveSessions(sessions); err != nil {
		return store.Session{}, err
	}
	return session, nil
}
