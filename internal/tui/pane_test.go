package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestEncodeKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{name: "runes", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}, want: "hi"},
		{name: "unicode runes", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, want: "é"},
		{name: "enter", msg: tea.KeyMsg{Type: tea.KeyEnter}, want: "\r"},
		{name: "tab", msg: tea.KeyMsg{Type: tea.KeyTab}, want: "\t"},
		{name: "esc", msg: tea.KeyMsg{Type: tea.KeyEsc}, want: "\x1b"},
		{name: "space", msg: tea.KeyMsg{Type: tea.KeySpace}, want: " "},
		{name: "backspace", msg: tea.KeyMsg{Type: tea.KeyBackspace}, want: "\x7f"},
		{name: "ctrl+c", msg: tea.KeyMsg{Type: tea.KeyCtrlC}, want: "\x03"},
		{name: "ctrl+a", msg: tea.KeyMsg{Type: tea.KeyCtrlA}, want: "\x01"},
		{name: "up", msg: tea.KeyMsg{Type: tea.KeyUp}, want: "\x1b[A"},
		{name: "down", msg: tea.KeyMsg{Type: tea.KeyDown}, want: "\x1b[B"},
		{name: "right", msg: tea.KeyMsg{Type: tea.KeyRight}, want: "\x1b[C"},
		{name: "left", msg: tea.KeyMsg{Type: tea.KeyLeft}, want: "\x1b[D"},
		{name: "shift+tab", msg: tea.KeyMsg{Type: tea.KeyShiftTab}, want: "\x1b[Z"},
		{name: "home", msg: tea.KeyMsg{Type: tea.KeyHome}, want: "\x1b[H"},
		{name: "pgup", msg: tea.KeyMsg{Type: tea.KeyPgUp}, want: "\x1b[5~"},
		{name: "delete", msg: tea.KeyMsg{Type: tea.KeyDelete}, want: "\x1b[3~"},
		{name: "alt rune", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, want: "\x1bb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := string(encodeKey(tc.msg)); got != tc.want {
				t.Fatalf("encodeKey(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestClampPaneSize(t *testing.T) {
	t.Parallel()

	cols, rows := clampPaneSize(0, 0)
	if cols < minPaneCols || rows < minPaneRows {
		t.Fatalf("clampPaneSize(0,0) = %d,%d, want at least %d,%d", cols, rows, minPaneCols, minPaneRows)
	}

	if cols, rows = clampPaneSize(120, 40); cols != 120 || rows != 40 {
		t.Fatalf("clampPaneSize(120,40) = %d,%d, want them untouched", cols, rows)
	}
}

// The pane must always render exactly its own geometry, otherwise the box
// around it tears. This is what stops the embedded agent's escape sequences
// from escaping into saint's frame.
func TestPaneRendersFixedGeometry(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 30, 8)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	// Feed output that a full-screen TUI would emit: a clear, an absolute
	// cursor move, and colour.
	if err := pane.Write([]byte("\x1b[2J\x1b[3;5Hhello\x1b[31mRED\x1b[0m\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	waitForPaneOutput(t, pane)

	assertGeometry(t, pane, 30, 8)

	pane.Resize(50, 12)
	assertGeometry(t, pane, 50, 12)
}

func assertGeometry(t *testing.T, pane *sessionPane, cols, rows int) {
	t.Helper()

	lines := strings.Split(pane.Render(), "\n")
	if len(lines) != rows {
		t.Fatalf("render produced %d lines, want %d", len(lines), rows)
	}
	for idx, line := range lines {
		if width := lipgloss.Width(line); width != cols {
			t.Fatalf("line %d display width = %d, want %d (%q)", idx, width, cols, line)
		}
	}
}

// waitForPaneOutput blocks until the pump has processed at least one chunk.
func waitForPaneOutput(t *testing.T, pane *sessionPane) {
	t.Helper()

	select {
	case <-pane.dirty:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pane output")
	}
}

// fillPaneScrollback writes far more numbered lines than the pane's live grid
// can hold and blocks until the emulator has moved some of them into
// scrollback and settled, so scroll tests have real, stable history to
// scroll into instead of racing the output pump.
func fillPaneScrollback(t *testing.T, pane *sessionPane) {
	t.Helper()

	var input strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&input, "line%02d\n", i)
	}
	if err := pane.Write([]byte(input.String())); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	const settleWindow = 150 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)
	last := -1
	var stableSince time.Time
	for {
		got := pane.emulator.ScrollbackLen()
		if got > 0 && got == last {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= settleWindow {
				return
			}
		} else {
			stableSince = time.Time{}
		}
		last = got
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for scrollback to settle; ScrollbackLen() = %d", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPaneCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 20, 6)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}

	pane.Close()
	pane.Close()

	if err := pane.Write([]byte("x")); err == nil {
		t.Fatal("Write() to a closed pane error = nil, want non-nil")
	}
}

func TestStartSessionPaneRejectsNilCommand(t *testing.T) {
	t.Parallel()

	if _, err := startSessionPane("abc123", "demo", nil, 20, 6); err == nil {
		t.Fatal("startSessionPane(nil) error = nil, want non-nil")
	}
}

// A program running in the pane can ask the terminal questions, and the
// emulator queues its answers in a synchronous pipe. If nobody reads that pipe
// the first answer blocks the emulator with its lock held, which then blocks
// the output pump, the render path, and every key including the one that quits.
func TestEmulatorRepliesDoNotBlockTheEmulator(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Device status report: "where is the cursor?"
		_, _ = pane.emulator.Write([]byte("\x1b[6n"))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the emulator blocked on a reply nobody read")
	}
}

// The deadlock this guards against needed three things: a program that asked
// for in-band resize reports, a terminal resize, and a lock held across the
// emulator call. Modern TUIs ask for resize reports as a matter of course.
func TestResizeDoesNotDeadlockWithResizeReportsEnabled(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	// The program turns on in-band resize reporting (DEC private mode 2048).
	if _, err := pane.emulator.Write([]byte("\x1b[?2048h")); err != nil {
		t.Fatalf("enabling resize reports: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		pane.Resize(60, 20)
		// Render needs the same lock Resize takes; a resize that never released
		// it froze the whole TUI here.
		_ = pane.Render()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Resize deadlocked with in-band resize reports enabled")
	}

	if cols, rows := pane.Size(); cols != 60 || rows != 20 {
		t.Errorf("Size() = %dx%d, want 60x20", cols, rows)
	}
}

// Close is the quit path. It must not be able to hang, whatever the pane is
// doing.
func TestCloseReturnsPromptly(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	if _, err := pane.emulator.Write([]byte("\x1b[?2048h")); err != nil {
		t.Fatalf("enabling resize reports: %v", err)
	}
	pane.Resize(60, 20)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pane.Close()
	}()

	select {
	case <-done:
	case <-time.After(paneCloseTimeout + 3*time.Second):
		t.Fatal("Close() hung")
	}
}

// Every attach starts a reply pump. If closing a pane left it parked on the
// emulator's pipe, a long saint session would accumulate one blocked goroutine
// and one emulator grid per attach.
func TestClosedPanesDoNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 5 {
		pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
		if err != nil {
			t.Fatalf("startSessionPane() error = %v", err)
		}
		pane.Resize(50, 12)
		pane.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after 5 panes, started at %d", got, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScrollUpAndDownClampAtBothEnds(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 20, 5)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	// Nothing to scroll from yet: scrolling down from live must stay at live.
	pane.ScrollDown(10)
	if pane.Scrolled() {
		t.Fatal("Scrolled() = true after ScrollDown with no history, want false")
	}

	fillPaneScrollback(t, pane)

	maxOffset := pane.emulator.ScrollbackLen()
	pane.ScrollUp(maxOffset + 1000)
	if !pane.Scrolled() {
		t.Fatal("Scrolled() = false after ScrollUp, want true")
	}
	pane.mu.Lock()
	offset := pane.scrollOffset
	pane.mu.Unlock()
	if offset != maxOffset {
		t.Fatalf("scrollOffset = %d, want clamped to ScrollbackLen() = %d", offset, maxOffset)
	}

	pane.ScrollDown(maxOffset + 1000)
	if pane.Scrolled() {
		t.Fatal("Scrolled() = true after ScrollDown past live, want false")
	}
}

func TestResizeResetsScrollOffset(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 20, 5)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	fillPaneScrollback(t, pane)

	pane.ScrollUp(5)
	if !pane.Scrolled() {
		t.Fatal("setup: ScrollUp did not scroll the pane")
	}

	pane.Resize(30, 8)
	if pane.Scrolled() {
		t.Fatal("Scrolled() = true after Resize, want reset to live")
	}
}

// Scrolled-up Render output must actually surface older content, and returning
// to live must reproduce exactly what live rendering already produced - the
// two paths must agree once the offset is back to zero.
func TestRenderShowsScrollbackContentAndReturnsToLive(t *testing.T) {
	t.Parallel()

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 20, 5)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	t.Cleanup(pane.Close)

	fillPaneScrollback(t, pane)

	live := pane.Render()
	if strings.Contains(live, "line00") {
		t.Fatalf("live render unexpectedly contains the earliest line:\n%s", live)
	}

	// Scroll by the scrollback's actual depth rather than a fixed number, which
	// clamps to the very top whatever that depth turns out to be.
	//
	// A literal 50 made this test pass only on a machine slow enough that
	// fillPaneScrollback's settle window closed before every line arrived. The
	// pane is fed through a PTY with echo on, so each line lands twice - once
	// echoed, once from cat - and a machine fast enough to deliver all of them
	// leaves more scrollback than 50 rows can climb.
	pane.ScrollUp(pane.emulator.ScrollbackLen())
	scrolled := pane.Render()
	if !strings.Contains(scrolled, "line00") {
		t.Fatalf("scrolled render does not contain early scrollback content:\n%s", scrolled)
	}
	// The pane clamps to a usable minimum, so what to assert is the geometry the
	// pane was actually given and not the one it was asked for: 5 rows becomes
	// minPaneRows. Writing the literal here asserted a shape the pane never has.
	cols, rows := clampPaneSize(20, 5)
	assertGeometry(t, pane, cols, rows)

	pane.ScrollToLive()
	if got := pane.Render(); got != live {
		t.Fatalf("Render() after ScrollToLive = %q, want back to the original live render %q", got, live)
	}
}

// A reply produced before Close must not be mistaken for Close's own.
//
// Close wakes the reply pump by asking the emulator for the cursor position
// and treating the answer as the signal to leave. A resize report already in
// flight can be read after Close sets waking; the pump used to leave on it,
// and Close's question then had no reader and blocked forever - on the quit
// path. The race was found as a flake in TestCloseReturnsPromptly on CI. Here
// it is forced: the pump is held holding the resize report until waking is
// set.
func TestCloseIsNotStrandedByAStaleReply(t *testing.T) {
	// Not parallel: it installs the package-level pump hook.
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func() {
		once.Do(func() {
			close(held)
			<-release
		})
	}
	afterReplyRead.Store(&hook)
	t.Cleanup(func() { afterReplyRead.Store(nil) })

	pane, err := startSessionPane("abc123", "demo", exec.Command("/bin/cat"), 40, 10)
	if err != nil {
		t.Fatalf("startSessionPane() error = %v", err)
	}
	if _, err := pane.emulator.Write([]byte("\x1b[?2048h")); err != nil {
		t.Fatalf("enabling resize reports: %v", err)
	}
	go pane.Resize(60, 20)

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the resize produced no reply for the pump to read")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		pane.Close()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		pane.mu.Lock()
		waking := pane.waking
		pane.mu.Unlock()
		if waking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never set waking")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)

	select {
	case <-pane.repliesDone:
	case <-time.After(paneCloseTimeout + time.Second):
		t.Fatal("the reply pump never left; Close's cursor question was stranded behind a stale reply")
	}
	select {
	case <-closed:
	case <-time.After(paneCloseTimeout + 3*time.Second):
		t.Fatal("Close() hung")
	}

	// The pump leaving is not enough: it must have left on Close's answer. If
	// it left on the stale reply, Close's question is still parked in the
	// emulator holding its lock, and the next thing to touch the emulator -
	// a render, on the way out - wedges.
	touched := make(chan struct{})
	go func() {
		defer close(touched)
		_, _ = pane.emulator.Write([]byte("x"))
	}()
	select {
	case <-touched:
	case <-time.After(2 * time.Second):
		t.Fatal("the emulator is wedged after Close: its cursor question was stranded behind a stale reply")
	}
}
