package main

import (
	"fmt"
	"io"
	"time"

	"github.com/montcao/saint/internal/credential"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// custodyLine says, before the agent starts, who holds the session's
// credential: the guest itself (subscription mode), or only the proxy, with a
// per-session token in the guest (key mode). That decides what a compromised agent
// could take with it, so it is shown where the session starts
// (docs/design/principles.md). Working state and spend are herdr's to show.
func custodyLine(s store.Session, now time.Time) string {
	if s.ProxyAuthOrDefault() == store.ProxyAuthSubscription {
		source := "your Claude plan token"
		if s.Credential == credential.ClaudeCodeSpec {
			source = "your Claude Code login"
		} else if s.Credential != "" {
			source += " (" + s.Credential + ")"
		}
		return fmt.Sprintf("saint: the agent holds %s, %s", source, credentialLife(s.CredentialExpiresAt, now))
	}
	provider := "Anthropic"
	if s.Runtime == sandbox.RuntimeCodex {
		provider = "OpenAI"
	}
	return fmt.Sprintf("saint: the proxy holds the %s API key; the agent sees only a per-session token", provider)
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

func printCustody(out io.Writer, s store.Session) {
	if out != nil {
		fmt.Fprintln(out, custodyLine(s, time.Now()))
	}
}
