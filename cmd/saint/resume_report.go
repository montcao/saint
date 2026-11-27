package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/montcao/saint/internal/store"
)

func resumeReportArgs(paneID, runtime, executable, stateDir, sessionID string) []string {
	return []string{"pane", "report-agent", paneID, "--source", "saint-resume", "--agent", runtime, "--state", "unknown",
		"--", "env", store.StateDirEnv + "=" + stateDir, executable, "attach", sessionID}
}

func reportResume(ctx context.Context, deps sessionDeps, session store.Session, out io.Writer) {
	if deps.pane == nil {
		return
	}
	pane := deps.pane()
	if pane == nil {
		return
	}
	executable, err := os.Executable()
	if err == nil {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		err = runHerdrAt(bounded, *pane, resumeReportArgs(pane.PaneID, string(session.Runtime), executable, deps.svc.Store.Paths().Root, session.ID)...)
	}
	if err != nil {
		fmt.Fprintf(out, "saint: could not register restart command: %v; reattach with saint attach %s\n", err, session.ID)
	}
}
