package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
)

// containerNamePattern mirrors the session-ID rule in internal/store: the
// literal prefix "saint-" plus a validated session ID.
//
// Names reach the container CLI from sessions.json, which a guest with a
// mounted repo can influence. There is no shell involved so this is not command
// injection, but an unvalidated name is still a way to smuggle flags into
// `container exec/stop/start/rm`. Validation lives in the store at load time
// and is repeated here, immediately before exec, so no future caller can skip it.
var containerNamePattern = regexp.MustCompile(`^saint-[a-z0-9][a-z0-9-]{0,31}$`)

// ValidateContainerName rejects any name that saint did not construct itself.
func ValidateContainerName(name string) error {
	if !containerNamePattern.MatchString(name) {
		display := name
		if len(display) > 40 {
			display = display[:40] + "..."
		}
		return fmt.Errorf("invalid container name %q", display)
	}
	return nil
}

type Runtime string

const (
	RuntimeUnknown Runtime = ""
	RuntimeClaude  Runtime = "claude"
	RuntimeCodex   Runtime = "codex"
)

type SandboxState int

const (
	StateAbsent SandboxState = iota
	StateStopped
	StateRunning
)

type Repo struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

type Info struct {
	Name    string
	State   SandboxState
	Runtime Runtime
	Paths   []string
}

type CreateOptions struct {
	Resume   bool
	Prompt   string
	CPUs     int
	MemoryGB int
	Name     string

	// ProxyToken is the per-session credential the guest presents to the local
	// proxy in place of a real provider key. It is never the upstream key.
	ProxyToken string

	// ProxyEgress points the guest's HTTP_PROXY and HTTPS_PROXY at saint, so
	// the session's git, npm and curl traffic crosses the host's forward
	// proxy. A session on its own internal network has no other way out, so
	// every new session sets it.
	//
	// False must set none of those variables. A guest pointed at a proxy that
	// is not accepting forward traffic does not degrade - every clone and every
	// install fails at once - so the off state has to be the untouched
	// environment, never a proxy URL that happens to be refused.
	ProxyEgress bool

	// Network is the container network to run on. Empty leaves the runtime's
	// default network, with its ordinary internet access.
	Network string

	// SkipPermissions runs the agent without its approval prompts.
	//
	// The microVM is the boundary, so this is the usual choice: an agent that
	// stops to ask before every edit inside its own sandbox is mostly friction.
	// It is a per-session choice because the sandbox is not the only thing at
	// stake - the session's directory is mounted read-write, and its contents
	// are the user's.
	SkipPermissions bool

	// AuthMode is "key" or "subscription". Empty means key.
	AuthMode string

	// SubscriptionToken is the Claude subscription OAuth token, set only in
	// subscription mode. Unlike ProxyToken this is a real credential, so it
	// reaches the guest through an env file rather than argv.
	SubscriptionToken string

	// SkillMounts are host skill directories bound into the guest under
	// /saint/skills/<name>. Always read-only, unlike Repo.ReadOnly: a skill
	// is data the agent reads, never a place it writes back to.
	SkillMounts []SkillMount
}

// AttachOptions tells attach how to rebuild a session that is no longer there.
type AttachOptions struct {
	// Workdir is where a recreated session starts.
	Workdir string
	// Command is the agent to run when the session had to be recreated. Empty
	// leaves a plain shell.
	Command string
}

var ErrRuntimeBlocked = errors.New("runtime workflow blocked by spike findings")

type Sandbox interface {
	Create(ctx context.Context, name string, rt Runtime, repos []Repo, sessionDir, proxyURL string, opts CreateOptions) error
	// CreateNetwork creates (or reuses) a session's internal network and
	// returns the host's address on it.
	CreateNetwork(ctx context.Context, name string) (gateway string, err error)
	// RemoveNetwork deletes a session's network; an absent one is not an error.
	RemoveNetwork(ctx context.Context, name string) error
	List(ctx context.Context) ([]Info, error)
	Stop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	AttachCmd(name string, opts AttachOptions) (*exec.Cmd, error)
}
