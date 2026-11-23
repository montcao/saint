package main

import (
	"github.com/montcao/saint/internal/store"
	"slices"
	"testing"
)

func TestResumeReportKeepsCommandArgumentsSeparate(t *testing.T) {
	got := resumeReportArgs("w:p", "claude", "/test path/saint", "/state path", "abc")
	want := []string{"pane", "report-agent", "w:p", "--source", "saint-resume", "--agent", "claude", "--", "env", store.StateDirEnv + "=/state path", "/test path/saint", "attach", "abc"}
	if !slices.Equal(got, want) {
		t.Fatalf("resume command=%q", got)
	}
}
