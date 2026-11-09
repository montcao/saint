package session

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
