package sandbox

import (
	"strings"
	"testing"
)

func TestVersionGuardBeforeAgentLaunch(t *testing.T) {
	s, e := runtimeBootstrapScript(RuntimeClaude, "/synthetic", CreateOptions{AgentVersion: "2.1.3"})
	if e != nil {
		t.Fatal(e)
	}
	check := strings.Index(s, `$(/usr/local/bin/claude --version 2>/dev/null)`)
	launch := strings.Index(s, "tmux new-session")
	if check < 0 || launch < check || !strings.Contains(s, "exit 70") {
		t.Fatal("version guard missing or after launch")
	}
	if _, e = runtimeBootstrapScript(RuntimeClaude, "/synthetic", CreateOptions{AgentVersion: "2.1.3;bad"}); e == nil {
		t.Fatal("unvalidated version accepted")
	}
	s, e = AgentCommandVersion(RuntimeClaude, "/synthetic", "2.1.3")
	if e != nil || !strings.Contains(s, "2.1.3 (Claude Code)") {
		t.Fatal(s, e)
	}
}
