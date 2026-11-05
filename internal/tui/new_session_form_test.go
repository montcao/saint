package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/montcao/saint/internal/sandbox"
)

func typeInto(f *newSessionForm, text string) {
	for _, r := range text {
		f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// "h" and "l" used to toggle the runtime from every field, which made those two
// letters impossible to type anywhere in the form.
func TestFormAcceptsEveryLetter(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	f.repo.SetValue("")
	typeInto(&f, "hello")

	if got := f.repo.Value(); got != "hello" {
		t.Errorf("directory = %q, want %q", got, "hello")
	}
	if f.runtime != sandbox.RuntimeClaude {
		t.Errorf("runtime = %q, want it untouched by typing", f.runtime)
	}
}

// Arrow keys belong to the text cursor in a text field, the way they do in
// every other program.
func TestArrowsDoNotToggleRuntimeInTextFields(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	f.Update(tea.KeyMsg{Type: tea.KeyLeft})
	f.Update(tea.KeyMsg{Type: tea.KeyRight})

	if f.runtime != sandbox.RuntimeClaude {
		t.Errorf("runtime = %q, want it unchanged while a text field is focused", f.runtime)
	}
}

func TestRuntimeRowTogglesWithArrows(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	for range formRowRuntime {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if f.focus != formRowRuntime {
		t.Fatalf("focus = %d, want the runtime row", f.focus)
	}

	f.Update(tea.KeyMsg{Type: tea.KeyRight})
	if f.runtime != sandbox.RuntimeCodex {
		t.Errorf("runtime = %q, want codex", f.runtime)
	}
	f.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if f.runtime != sandbox.RuntimeClaude {
		t.Errorf("runtime = %q, want claude", f.runtime)
	}
}

func TestFocusWrapsThroughEveryRow(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	seen := map[int]bool{f.focus: true}
	for range formRowCount {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
		seen[f.focus] = true
	}
	if len(seen) != formRowCount {
		t.Errorf("visited %d rows, want %d", len(seen), formRowCount)
	}
	if f.focus != formRowDir {
		t.Errorf("focus = %d, want to wrap back to the first row", f.focus)
	}
}

// The runtime row has no cursor, so without a marker there is no way to tell it
// is selected.
func TestFormMarksTheFocusedRow(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	if got := f.View(80); !strings.Contains(got, "▸ directory") {
		t.Errorf("form does not mark the focused row:\n%s", got)
	}

	for range formRowRuntime {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	view := f.View(80)
	if !strings.Contains(view, "▸ runtime") {
		t.Errorf("form does not mark the runtime row:\n%s", view)
	}
	// The hint only mentions the arrows where they do something.
	if !strings.Contains(view, "choose runtime") {
		t.Errorf("form does not say how to change the runtime:\n%s", view)
	}
}

// A toggle silently ignores everything past the second option. There are three
// runtimes now, and Hermes was the one that would have been unreachable.
func TestRuntimeRowCyclesThroughEveryRuntime(t *testing.T) {
	t.Parallel()

	f := newSessionDialogWith("", sandbox.RuntimeClaude, "")
	for range formRowRuntime {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}

	seen := map[sandbox.Runtime]bool{f.runtime: true}
	for range len(formRuntimes) {
		f.Update(tea.KeyMsg{Type: tea.KeyRight})
		seen[f.runtime] = true
	}
	for _, want := range formRuntimes {
		if !seen[want] {
			t.Errorf("runtime %q is unreachable from the form", want)
		}
	}
	if f.runtime != sandbox.RuntimeClaude {
		t.Errorf("cycling did not return to the start, got %q", f.runtime)
	}
}

// Which option on a choice row is live used to be carried by colour and weight
// alone. That was survivable while the live one was cyan among greys; it is not
// now that it is one grey against another, which a low-contrast terminal theme
// may render as no difference at all - and what these rows decide is which
// credential a container is about to be handed and whether the agent asks
// before it edits.
//
// Rendering in a test strips colour, so this asserts exactly what a monochrome
// terminal shows.
func TestChoiceRowsMarkTheLiveOptionWithoutRelyingOnColour(t *testing.T) {
	t.Parallel()

	f := newSessionDialog("", sandbox.RuntimeClaude)
	view := f.View(100)
	for _, want := range []string{"● claude", "○ codex", "● skip (sandboxed)", "○ ask", "● live login"} {
		if !strings.Contains(view, want) {
			t.Errorf("form does not mark %q:\n%s", want, view)
		}
	}

	// And the mark moves with the choice, rather than being decoration on the
	// first option.
	for range formRowRuntime {
		f.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	f.Update(tea.KeyMsg{Type: tea.KeyRight})
	view = f.View(100)
	for _, want := range []string{"○ claude", "● codex"} {
		if !strings.Contains(view, want) {
			t.Errorf("the runtime mark did not follow the choice, want %q:\n%s", want, view)
		}
	}

	f.Update(tea.KeyMsg{Type: tea.KeyTab})
	f.Update(tea.KeyMsg{Type: tea.KeyRight})
	view = f.View(100)
	for _, want := range []string{"○ skip (sandboxed)", "● ask"} {
		if !strings.Contains(view, want) {
			t.Errorf("the permissions mark did not follow the choice, want %q:\n%s", want, view)
		}
	}
}
