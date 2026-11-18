package main

import (
	"fmt"
	"io"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// billingLine says, before the agent starts, which account a session's usage
// lands on and what credential it carries. The two modes bill different
// accounts, and a fallback from one to the other is otherwise invisible until
// a bill arrives (docs/design/principles.md).
func billingLine(s store.Session, now time.Time) string {
	if s.ProxyAuthOrDefault() == store.ProxyAuthSubscription {
		source := "your Claude plan token"
		if s.Credential == credential.ClaudeCodeSpec {
			source = "your Claude Code login"
		} else if s.Credential != "" {
			source += " (" + s.Credential + ")"
		}
		return fmt.Sprintf("saint: this session draws on your Claude plan, using %s, %s; the agent holds that token", source, credentialLife(s.CredentialExpiresAt, now))
	}
	provider := "Anthropic"
	if s.Runtime == sandbox.RuntimeCodex {
		provider = "OpenAI"
	}
	return fmt.Sprintf("saint: this session bills per token to the %s API key the proxy holds; the agent never sees it", provider)
}

// credentialLife is how long a credential has, in the unit a person would
// use. An unknown expiry is said to be unknown, never rounded to "lasts".
func credentialLife(expires, now time.Time) string {
	if expires.IsZero() {
		return "expiry unknown"
	}
	left := expires.Sub(now)
	switch {
	case left <= 0:
		return "already expired"
	case left < time.Hour:
		return fmt.Sprintf("expires in %d minutes", int(left.Minutes()))
	case left < 48*time.Hour:
		return fmt.Sprintf("expires in %d hours", int(left.Hours()))
	case left < 60*24*time.Hour:
		return fmt.Sprintf("expires in %d days", int(left.Hours()/24))
	default:
		return fmt.Sprintf("expires in %d months", int(left.Hours()/24/30))
	}
}

func printBilling(out io.Writer, s store.Session) {
	if out != nil {
		fmt.Fprintln(out, billingLine(s, time.Now()))
	}
}
