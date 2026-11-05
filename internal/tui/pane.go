package tui

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Minimum usable geometry for an embedded session. Anything smaller and tmux
// and the agent TUI inside it start refusing to draw.
const (
	minPaneCols = 20
	minPaneRows = 6
)

// paneCloseTimeout bounds how long Close waits for the output pump to finish.
const paneCloseTimeout = 2 * time.Second

// paneScrollbackLines bounds how much history the pane keeps for scrolling.
// Fixed rather than configurable: there is no user-visible reason yet to
// tune it, and a config knob nobody has asked for is just a knob.
const paneScrollbackLines = 5000

// paneWheelLines is how many rows one mouse-wheel notch moves, matching the
// convention most terminals already use.
const paneWheelLines = 3

// dsrCursorPosition asks the emulator where the cursor is.
//
// It is used only to wake the reply pump on shutdown: the cheapest question a
// terminal is guaranteed to answer, and the answer is what unblocks the pump's
// read so it can notice the pane is closed and return.
var dsrCursorPosition = []byte("\x1b[6n")

// paneDirtyMsg reports that the embedded terminal produced output and the view
// needs a redraw. It carries the session ID so output from a pane the user has
// already detached from cannot resurrect it.
type paneDirtyMsg struct{ id string }

// paneExitedMsg reports that the attached process ended on its own - the agent
// quit, tmux died, or the container stopped underneath us.
type paneExitedMsg struct {
	id  string
	err error
}

// sessionPane is a live agent session embedded in a region of the TUI.
//
// The session runs under a PTY rather than being handed the real terminal, and
// its output is fed to a virtual terminal emulator instead of being printed.
// That indirection is what keeps saint on screen: the agent is a full-screen
// TUI under tmux, so it emits absolute cursor moves, scroll regions and screen
// clears. Written straight to stdout those escape sequences address the whole
// terminal and would trash saint's own frame. The emulator interprets them
// against an off-screen grid that is exactly the size of the pane, and the view
// renders that grid as ordinary text.
type sessionPane struct {
	id   string
	name string

	emulator *vt.SafeEmulator
	ptmx     *os.File
	cmd      *exec.Cmd

	// dirty is a coalescing signal, not a queue: it is buffered to one and sent
	// to without blocking, so a burst of output produces one redraw rather than
	// one per read.
	dirty  chan struct{}
	exited chan error

	// done is closed when pump returns, so Close can wait for the emulator to
	// be idle before shutting it down.
	done chan struct{}

	mu     sync.Mutex
	cols   int
	rows   int
	closed bool

	// scrollOffset is how many lines above the live bottom the view is
	// showing. Zero means following live output, which is the only state
	// that existed before scrolling did, and Render's fast path for it is
	// unchanged.
	scrollOffset int

	// waking is set immediately before Close asks the emulator a question whose
	// answer exists only to unblock the reply pump. It is what tells the pump
	// that a cursor report is now the shutdown signal and not real output.
	waking bool

	// repliesDone is closed when the reply pump returns, so Close can wait for
	// it with a bound instead of assuming it left.
	repliesDone chan struct{}
}

// afterReplyRead runs on the reply pump after each read, before the pump
// decides what the bytes are. It is nil in production; a test sets it before
// starting a pane to hold the pump at the exact point a stale reply races
// Close.
var afterReplyRead atomic.Pointer[func()]

// cursorReport matches the emulator's answer to dsrCursorPosition.
var cursorReport = regexp.MustCompile(`\x1b\[\d+;\d+R`)

// startSessionPane runs cmd under a PTY sized to the pane and begins pumping its
// output into the emulator.
func startSessionPane(id, name string, cmd *exec.Cmd, cols, rows int) (*sessionPane, error) {
	if cmd == nil {
		return nil, errors.New("attach command is nil")
	}
	cols, rows = clampPaneSize(cols, rows)

	// The agent and tmux both key off TERM. Without it they fall back to a dumb
	// terminal and render as unstyled soup inside the pane.
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}

	emulator := vt.NewSafeEmulator(cols, rows)
	emulator.SetScrollbackSize(paneScrollbackLines)

	p := &sessionPane{
		id:       id,
		name:     name,
		emulator: emulator,
		ptmx:     ptmx,
		cmd:      cmd,
		dirty:    make(chan struct{}, 1),
		exited:   make(chan error, 1),
		done:     make(chan struct{}),
		cols:     cols,
		rows:     rows,

		repliesDone: make(chan struct{}),
	}

	go p.pump()
	go p.pumpReplies()
	return p, nil
}

// pumpReplies forwards the emulator's answers back to the program in the pane.
//
// A terminal is not just a screen: programs ask it questions - where is the
// cursor, what are you, what size are you - and block until it answers. The
// emulator queues those answers in a synchronous pipe, so if nobody reads it,
// the first answer blocks the emulator forever, with its lock held.
//
// That is not a hypothetical. Anything that turns on in-band resize reporting
// makes the emulator answer every resize, so a single terminal resize would
// wedge the emulator, then the output pump waiting on its lock, then the render
// path, then the whole TUI - with no way out, because quitting needs the same
// locks.
func (p *sessionPane) pumpReplies() {
	defer close(p.repliesDone)
	buf := make([]byte, 4096)
	for {
		n, err := p.emulator.Read(buf)
		if n > 0 {
			if hook := afterReplyRead.Load(); hook != nil {
				(*hook)()
			}
			p.mu.Lock()
			waking := p.waking
			p.mu.Unlock()
			if waking {
				// Leave on Close's cursor report and nothing else. A reply
				// produced before Close - a resize report, say - can be read
				// after waking is set, and leaving on it would strand Close's
				// question with no reader. The PTY is closed by now, so a
				// stale reply has nowhere to go and is dropped.
				if cursorReport.Match(buf[:n]) {
					return
				}
				if err != nil {
					return
				}
				continue
			}
			// A failed write is not a reason to stop reading, and neither is the
			// pane being closed. This goroutine is the emulator's only reader:
			// if it leaves while anything can still produce a reply, that reply
			// blocks the emulator - and everything waiting on it - permanently.
			// It leaves on the shutdown signal and nothing else.
			_, _ = p.ptmx.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// pump copies PTY output into the emulator until the process goes away.
func (p *sessionPane) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			// SafeEmulator is concurrency safe, which is what lets this run on
			// its own goroutine while the Bubble Tea render loop reads the
			// screen.
			_, _ = p.emulator.Write(buf[:n])
			p.markDirty()
		}
		if err != nil {
			// A closed PTY reports EIO on macOS when the child exits; that is a
			// normal end of session, not a failure worth surfacing.
			p.exited <- p.finish(err)
			close(p.done)
			return
		}
	}
}

// finish reaps the child and decides whether the session ended cleanly.
func (p *sessionPane) finish(readErr error) error {
	waitErr := p.cmd.Wait()

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()

	// A pane the user detached from was killed on purpose, so neither the read
	// error nor the resulting non-zero wait status is interesting.
	if closed {
		return nil
	}
	if waitErr != nil {
		return waitErr
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		// EIO after the child exits is the normal PTY teardown path.
		return nil
	}
	return nil
}

func (p *sessionPane) markDirty() {
	select {
	case p.dirty <- struct{}{}:
	default:
	}
}

// Write forwards user input to the session.
func (p *sessionPane) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("session pane is closed")
	}
	_, err := p.ptmx.Write(data)
	return err
}

// Resize matches both the emulator grid and the PTY window to the pane. Both
// must move together: the emulator decides how saint draws the session, and the
// PTY winsize is what makes the program inside reflow.
func (p *sessionPane) Resize(cols, rows int) {
	cols, rows = clampPaneSize(cols, rows)

	p.mu.Lock()
	if p.closed || (cols == p.cols && rows == p.rows) {
		p.mu.Unlock()
		return
	}
	p.cols, p.rows = cols, rows
	// Row semantics change on reflow, so a scroll position from before the
	// resize would show a misaligned mix of old and new geometry.
	p.scrollOffset = 0
	ptmx := p.ptmx
	p.mu.Unlock()

	// Deliberately outside the lock. Resizing makes the emulator answer the
	// program in the pane, and that answer travels through a pipe drained by
	// another goroutine. Holding p.mu across it means the render path - which
	// needs p.mu to ask how big the pane is - can no longer run, and the TUI
	// stops responding to every key including the one that quits.
	p.emulator.Resize(cols, rows)
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close detaches from the session. It kills the local `container exec` client,
// not the agent: the session keeps running inside tmux in the container, which
// is what makes detach and re-attach cheap.
func (p *sessionPane) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	_ = p.ptmx.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}

	// Wait for the output pump to stop touching the emulator. The wait is
	// bounded: Close is on the quit path, and a quit that can hang is the exact
	// failure this file exists to avoid.
	select {
	case <-p.done:
	case <-time.After(paneCloseTimeout):
	}

	// Wake the reply pump so it returns instead of parking on the emulator's
	// pipe for the rest of the process's life.
	//
	// The pump's read completes only when the emulator produces a reply, so ask
	// it a question. The flag is set first, and only here: the output pump has
	// already exited, so nothing else can produce a reply, which makes the next
	// one the pump sees unambiguously this one. Without that the pump could
	// leave on some earlier reply and this write would block forever - on the
	// quit path, which is the one place that must never hang.
	//
	// The emulator's own Close would also unblock the read, but it sets a flag
	// that Read checks without synchronisation - a data race the detector
	// rightly fails the build on.
	p.mu.Lock()
	p.waking = true
	p.mu.Unlock()

	// The question is asked off this goroutine and the wait is bounded. If the
	// pump has already gone, the write has no reader; that must cost a parked
	// goroutine, never the quit path.
	go func() { _, _ = p.emulator.Write(dsrCursorPosition) }()
	select {
	case <-p.repliesDone:
	case <-time.After(paneCloseTimeout):
	}
}

// Size reports the current pane geometry.
func (p *sessionPane) Size() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// ScrollUp moves the view n lines back into scrollback, clamped to however
// much history the emulator actually has.
//
// If the agent's own program has switched to the alternate screen - a
// full-screen TUI, the way vim or htop would - the emulator's scrollback is
// empty for as long as that lasts. That is standard terminal behaviour, not
// a bug here: there is no history to show until the program returns to the
// primary screen.
func (p *sessionPane) ScrollUp(n int) {
	if n <= 0 {
		return
	}
	maxOffset := p.emulator.ScrollbackLen()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scrollOffset += n
	if p.scrollOffset > maxOffset {
		p.scrollOffset = maxOffset
	}
}

// ScrollDown moves the view n lines toward live output.
func (p *sessionPane) ScrollDown(n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scrollOffset -= n
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
}

// ScrollToLive returns to following live output.
func (p *sessionPane) ScrollToLive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scrollOffset = 0
}

// Scrolled reports whether the view is showing history rather than the live
// screen - the signal the rest of the TUI uses to draw a position badge and
// to decide whether an ordinary keystroke should snap back to live before
// being forwarded.
func (p *sessionPane) Scrolled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scrollOffset > 0
}

// Render returns exactly rows lines, each padded to cols display columns, so the
// pane always fills its box and never pushes the surrounding border out of
// alignment.
func (p *sessionPane) Render() string {
	cols, rows := p.Size()

	p.mu.Lock()
	offset := p.scrollOffset
	p.mu.Unlock()

	var lines []string
	if offset == 0 {
		lines = strings.Split(p.emulator.Render(), "\n")
	} else {
		lines = p.renderScrolled(rows, offset)
	}

	out := make([]string, rows)
	for idx := range out {
		line := ""
		if idx < len(lines) {
			line = lines[idx]
		}
		// lipgloss.Width, not len: the emulator emits SGR sequences inline and
		// the content may be wide or combining runes.
		if pad := cols - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		out[idx] = line
	}
	return strings.Join(out, "\n")
}

// renderScrolled composes rows lines from scrollback and the live screen as
// one buffer, oldest to newest: scrollback line 0, ..., scrollback line
// sb-1, live row 0, ..., live row rows-1. offset is how many lines above the
// live bottom the window's bottom edge sits.
//
// Scrollback lines and live rows are rendered through the same emulator
// styling (Scrollback().Line(i).Render() mirrors what Emulator.Render()
// already does per row), so a line looks identical whether it scrolled off
// the screen a moment ago or is still on it.
func (p *sessionPane) renderScrolled(rows, offset int) []string {
	sb := p.emulator.Scrollback()
	sbLen := 0
	if sb != nil {
		sbLen = sb.Len()
	}

	liveLines := strings.Split(p.emulator.Render(), "\n")

	bottom := sbLen + rows - 1 - offset
	top := bottom - rows + 1

	out := make([]string, rows)
	for i := 0; i < rows; i++ {
		k := top + i
		switch {
		case k < 0:
			out[i] = ""
		case k < sbLen:
			out[i] = sb.Line(k).Render()
		default:
			liveIdx := k - sbLen
			if liveIdx < len(liveLines) {
				out[i] = liveLines[liveIdx]
			}
		}
	}
	return out
}

// waitForPaneEvent blocks until the pane produces output or exits. Re-issued
// after every paneDirtyMsg, this is what drives redraws without polling.
func waitForPaneEvent(p *sessionPane) tea.Cmd {
	if p == nil {
		return nil
	}
	id := p.id
	return func() tea.Msg {
		select {
		case <-p.dirty:
			return paneDirtyMsg{id: id}
		case err := <-p.exited:
			return paneExitedMsg{id: id, err: err}
		}
	}
}

func clampPaneSize(cols, rows int) (int, int) {
	if cols < minPaneCols {
		cols = minPaneCols
	}
	if rows < minPaneRows {
		rows = minPaneRows
	}
	return cols, rows
}

// encodeKey turns a Bubble Tea key event back into the bytes a terminal would
// have sent, so the agent inside the pane sees ordinary terminal input.
//
// Bubble Tea's KeyType encoding makes this mostly mechanical: control keys are
// their own byte value (0-31), backspace is 127, and everything else is a
// negative sentinel that needs an explicit escape sequence.
func encodeKey(msg tea.KeyMsg) []byte {
	var out []byte

	// Alt-modified keys are sent as ESC followed by the unmodified key.
	if msg.Alt {
		out = append(out, 0x1b)
	}

	switch msg.Type {
	case tea.KeyRunes:
		return append(out, []byte(string(msg.Runes))...)
	case tea.KeySpace:
		return append(out, ' ')
	case tea.KeyBackspace:
		return append(out, 0x7f)
	case tea.KeyUp:
		return append(out, "\x1b[A"...)
	case tea.KeyDown:
		return append(out, "\x1b[B"...)
	case tea.KeyRight:
		return append(out, "\x1b[C"...)
	case tea.KeyLeft:
		return append(out, "\x1b[D"...)
	case tea.KeyShiftTab:
		return append(out, "\x1b[Z"...)
	case tea.KeyHome:
		return append(out, "\x1b[H"...)
	case tea.KeyEnd:
		return append(out, "\x1b[F"...)
	case tea.KeyPgUp:
		return append(out, "\x1b[5~"...)
	case tea.KeyPgDown:
		return append(out, "\x1b[6~"...)
	case tea.KeyDelete:
		return append(out, "\x1b[3~"...)
	case tea.KeyInsert:
		return append(out, "\x1b[2~"...)
	}

	// Control keys, plus enter (13), tab (9) and esc (27), which Bubble Tea
	// reports as their control-code values.
	if code := int(msg.Type); code >= 0 && code <= 31 {
		return append(out, byte(code))
	}

	return nil
}

// ForwardWheel hands a wheel notch to the program running in the pane, and
// reports whether the pane consumed it.
//
// This is the only input saint synthesises for a guest, and it is deliberately
// a mouse report rather than a keystroke. Translating a notch into an arrow key
// is what a terminal does when an application has not asked for mouse
// reporting, and it is why scrolling used to walk the agent's prompt history
// instead of its output: an agent cannot tell a synthesised cursor key from a
// typed one.
//
// The emulator is what decides. SendMouse writes nothing unless the program has
// actually requested a mouse mode, so a shell that never asked receives no
// bytes at all - the guarantee that matters here, because unsolicited mouse
// reports would arrive as literal text on its command line.
//
// x and y are pane-relative and one-based, which is what the wire format uses.
func (p *sessionPane) ForwardWheel(up bool, x, y int) bool {
	if p == nil {
		return false
	}

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return false
	}

	// Only while the program owns the whole screen. An alternate-screen program
	// keeps its own history and saint has none for it, so the notch belongs to
	// the guest. On the primary screen the opposite is true: output really does
	// scroll off into saint's scrollback, and scrolling that is the better
	// answer than asking a program that may not be listening.
	if !p.emulator.IsAltScreen() {
		return false
	}

	button := vt.MouseWheelDown
	if up {
		button = vt.MouseWheelUp
	}
	p.emulator.SendMouse(vt.MouseWheel{
		X: max(0, x-1), Y: max(0, y-1), Button: button,
	})
	return true
}
