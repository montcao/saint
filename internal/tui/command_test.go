package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRunCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  commandResult
	}{
		{input: "q", want: commandQuit},
		{input: ":q", want: commandQuit},
		{input: "  :q  ", want: commandQuit},
		{input: "Q", want: commandQuit},
		{input: "q!", want: commandQuit},
		{input: "qa", want: commandQuit},
		{input: "quit", want: commandQuit},
		{input: "wq", want: commandQuit},
		{input: "exit", want: commandQuit},
		{input: "detach", want: commandDetach},
		{input: "d", want: commandDetach},
		{input: "help", want: commandHelp},
		{input: "refresh", want: commandRefresh},
		{input: "new", want: commandNew},
		{input: "", want: commandNone},
		{input: ":", want: commandNone},
		{input: "nope", want: commandUnknown},
		{input: "q x", want: commandUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			if got := runCommand(tc.input); got != tc.want {
				t.Fatalf("runCommand(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// Typing ":q" and pressing enter must quit, the way it does in vim and k9s.
func TestColonQQuits(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, nil)

	opened, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	got := opened.(Model)
	if got.mode != modeCommand {
		t.Fatalf("mode = %v, want modeCommand after ':'", got.mode)
	}

	typed, _ := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	got = typed.(Model)
	if got.commandInput.Value() != "q" {
		t.Fatalf("command line = %q, want %q", got.commandInput.Value(), "q")
	}

	_, cmd := got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("cmd = nil, want tea.Quit")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("command produced no message, want a quit")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("msg = %T, want tea.QuitMsg", msg)
	}
}

func TestCommandLineEscapeCancels(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, nil)
	opened, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	typed, _ := opened.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	cancelled, cmd := typed.(Model).Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := cancelled.(Model)

	if got.mode != modeList {
		t.Fatalf("mode = %v, want modeList", got.mode)
	}
	if got.commandInput.Value() != "" {
		t.Fatalf("command line = %q, want it cleared", got.commandInput.Value())
	}
	if cmd != nil {
		t.Fatal("esc must not run the command")
	}
}

func TestUnknownCommandReportsWithoutQuitting(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, nil)
	m.mode = modeCommand
	m.commandInput.SetValue("bogus")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	if cmd != nil {
		t.Fatal("an unknown command must not act")
	}
	if got.mode != modeList {
		t.Fatalf("mode = %v, want modeList", got.mode)
	}
	if !strings.Contains(got.status, "unknown command") {
		t.Fatalf("status = %q", got.status)
	}
}

// The command line is only reachable from the list: while a session is attached
// ":" is the agent's keystroke, not saint's.
func TestColonIsNotACommandWhileAttached(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, []SessionRow{{ID: "run1234", Name: "one", State: "running"}})
	m.width, m.height = 100, 40

	attached, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := attached.(Model)
	t.Cleanup(got.closePane)

	next, _ := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}})
	if after := next.(Model); after.mode != modeAttached {
		t.Fatalf("mode = %v, want the session to keep the keystroke", after.mode)
	}
}

func TestCommandLineIsVisibleWhileOpen(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t, nil)
	m.width, m.height = 100, 40
	m.mode = modeCommand
	m.commandInput.SetValue("q")

	if !strings.Contains(m.View(), ":q") {
		t.Fatal("view does not show the open command line")
	}
}
