package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/montcao/saint/internal/store"
)

const claudeConfigUsage = "usage: saint claude-config [on|off]"

// runClaudeConfig shows or changes whether new Claude sessions get a read-only
// copy of the user's Claude Code configuration and git identity. It is off
// until the user turns it on: saint shares nothing of the user's with a guest
// that they have not approved.
//
//	saint claude-config       say which it is, and what is shared
//	saint claude-config on    share with new sessions
//	saint claude-config off   stop sharing, including when a session renews
func runClaudeConfig(st *store.Store, args []string, out io.Writer) error {
	if len(args) > 1 {
		return errors.New(claudeConfigUsage)
	}
	if len(args) == 1 {
		var on bool
		switch args[0] {
		case "on":
			on = true
		case "off":
			on = false
		default:
			return errors.New(claudeConfigUsage)
		}
		if _, err := st.UpdateConfig(func(cfg *store.Config) error {
			cfg.ShareClaudeConfig = on
			return nil
		}); err != nil {
			return err
		}
	}
	cfg, err := st.EnsureConfig()
	if err != nil {
		return err
	}
	fmt.Fprint(out, claudeConfigState(cfg.ShareClaudeConfig))
	return nil
}

// claudeConfigState says what is shared and what that exposes, at the place
// the choice is made rather than only in the docs.
func claudeConfigState(on bool) string {
	if !on {
		return "Sharing your Claude Code setup with sessions: off\n" +
			"  Sessions start with no CLAUDE.md, settings, skills or git identity of yours.\n" +
			"  Turn it on with saint claude-config on.\n"
	}
	return "Sharing your Claude Code setup with sessions: on\n" +
		"  New Claude sessions get a read-only copy of: CLAUDE.md; settings.json reduced\n" +
		"  to an allowlist (permission deny rules only, no env, hooks, plugins or MCP\n" +
		"  servers); skills, agents, commands and output styles (not linked folders, not\n" +
		"  credential-looking files); and your git name and email.\n" +
		"  The agent can read all of it, and can send what it reads out through any host\n" +
		"  a session may reach. Never shared: credentials, history, transcripts, MCP servers.\n" +
		"  Turn it off with saint claude-config off.\n"
}
