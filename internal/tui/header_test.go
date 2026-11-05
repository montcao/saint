package tui

import (
	"strings"
	"testing"
)

func TestHeaderShowsTopCommandMenuInListMode(t *testing.T) {
	t.Parallel()

	m := newRenderModel(t, 160, 40)
	header := m.renderHeader()

	// Two rows. The third used to carry a subtitle and a second copy of the
	// counts; both were folded into the title row so the list gets that row.
	if got := strings.Count(header, "\n"); got != 1 {
		t.Fatalf("header has %d newlines, want 1 for a two-row header:\n%s", got, header)
	}
	if !strings.Contains(header, "running") {
		t.Fatalf("header does not carry the counts:\n%s", header)
	}

	for _, want := range []string{"SESSIONS", "COMMANDS", "enter attach", "R refresh", ": cmd"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header is missing %q:\n%s", want, header)
		}
	}
}

func TestHeaderSwitchesToAttachedCommands(t *testing.T) {
	t.Parallel()

	m := newRenderModel(t, 160, 40)
	m.mode = modeAttached

	header := m.renderHeader()
	for _, want := range []string{"ATTACHED", "COMMANDS", "ctrl+\\ detach", "ctrl+q quit"} {
		if !strings.Contains(header, want) {
			t.Fatalf("attached header is missing %q:\n%s", want, header)
		}
	}
	for _, absent := range []string{"n spawn", "enter attach", ": cmd"} {
		if strings.Contains(header, absent) {
			t.Fatalf("attached header still shows list-mode action %q:\n%s", absent, header)
		}
	}
}
