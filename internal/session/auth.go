package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

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
