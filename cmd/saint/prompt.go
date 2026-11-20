package main

import (
	"bufio"
	"io"
	"os"

	"golang.org/x/term"
)

// A pane's questions are answered by keys pressed after the question, never
// by keys pressed before it.
//
// A pane can spend minutes building an image with nothing to answer, and
// Enter pressed then waits in the terminal until something reads it. Every
// question defaults to yes on Enter, so without this the first question after
// a long step would be accepted unread, and so would the next.

// prompter reads the answers to a pane's questions.
type prompter struct {
	*bufio.Reader
	// drain discards input typed before the question about to be asked. Nil
	// discards nothing, which is what a test feeding scripted answers wants.
	drain func()
}

// discardPending drops what was typed before now. Call it before printing a
// question, so only keys pressed after it can answer.
func (p *prompter) discardPending() {
	if p != nil && p.drain != nil {
		p.drain()
	}
}

// newPrompter reads answers from in. When in is a terminal, input typed
// before a question is discarded: what the reader has buffered and what the
// terminal holds unread. Anything else, such as a pipe, is read as it comes,
// since its input was written on purpose.
func newPrompter(in io.Reader) *prompter {
	p := &prompter{Reader: bufio.NewReader(in)}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.drain = func() {
			_, _ = p.Discard(p.Buffered())
			_ = flushTerminalInput(f)
		}
	}
	return p
}
