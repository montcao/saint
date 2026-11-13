package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// runHerdrEgress is the `egress` plugin pane: an overlay the proxy opens on a
// session's pane when that session asks to reach a host nobody has ruled on.
// It shows each pending host in turn, sends the answer, and exits when nothing
// is left, which closes the overlay.
//
// Every way out of it is an answer: Esc, q, Ctrl-C and closing the pane all
// deny, and so does the proxy's timeout.
func runHerdrEgress(socketPath string, in *os.File, out io.Writer) error {
	sessionID := os.Getenv(envEgressSession)
	conn, err := dialEgress(socketPath, sessionID)
	if err != nil {
		// herdr closes a pane when its command exits, so a failure waits to
		// be read.
		fmt.Fprintf(out, "saint: %v\n\nPress Enter to close.", err)
		_, _ = bufio.NewReader(in).ReadString('\n')
		return err
	}
	defer conn.conn.Close()

	if term.IsTerminal(int(in.Fd())) {
		if state, err := term.MakeRaw(int(in.Fd())); err == nil {
			defer func() { _ = term.Restore(int(in.Fd()), state) }()
		}
	}
	return egressPrompt(conn, in, out, egressArmDelay)
}

// egressArmDelay is how long after a question is drawn a key starts to
// count. Opening the prompt takes focus from wherever the person was typing,
// and a keystroke meant for that pane must not answer a question nobody read.
const egressArmDelay = 750 * time.Millisecond

func dialEgress(socketPath, sessionID string) (*controlConn, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%s is not set; the proxy opens this pane", envEgressSession)
	}
	raw, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the proxy: %w", err)
	}
	conn := newControlConn(raw)
	if err := conn.send(controlRequest{Op: controlOpEgress, Session: sessionID}); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// egressKey is what one key press means.
type egressKey int

const (
	keyNone egressKey = iota
	keyAllow
	keyDeny
)

func classifyEgressKey(b byte) egressKey {
	switch b {
	case 'a', 'A':
		return keyAllow
	case 'd', 'D', 'n', 'N', 'q', 'Q', 0x1b, 0x03, 0x04:
		// Esc, Ctrl-C and Ctrl-D leave the prompt, and leaving is a no.
		return keyDeny
	}
	return keyNone
}

// egressPrompt runs the question loop over an established connection.
func egressPrompt(conn *controlConn, in io.Reader, out io.Writer, armDelay time.Duration) error {
	keys := make(chan byte)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			if n == 1 {
				keys <- buf[0]
			}
		}
	}()

	messages := make(chan controlResponse)
	go func() {
		defer close(messages)
		for {
			var resp controlResponse
			if err := conn.receive(&resp); err != nil {
				return
			}
			messages <- resp
		}
	}()

	var current *controlQuestion
	var armedAt time.Time
	for {
		select {
		case msg, ok := <-messages:
			if !ok {
				return errors.New("the proxy closed the prompt")
			}
			switch {
			case msg.Done:
				return nil
			case msg.Question != nil:
				current = msg.Question
				armedAt = time.Now().Add(armDelay)
				renderEgressQuestion(out, *current)
			case msg.Expired != 0 && current != nil && msg.Expired == current.ID:
				fmt.Fprintf(out, "\r\n  No answer in time: %s was denied.\r\n", printable(current.Host))
				current = nil
			}
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			if current == nil || time.Now().Before(armedAt) {
				continue
			}
			switch classifyEgressKey(b) {
			case keyAllow:
				_ = conn.send(controlAnswer{ID: current.ID, Allow: true})
				current = nil
			case keyDeny:
				_ = conn.send(controlAnswer{ID: current.ID, Allow: false})
				current = nil
				if b == 0x1b || b == 0x03 || b == 0x04 || b == 'q' || b == 'Q' {
					// Leaving the prompt: the proxy denies anything else this
					// session is waiting on when the connection closes.
					return nil
				}
			}
		}
	}
}

func renderEgressQuestion(out io.Writer, q controlQuestion) {
	host := printable(q.Host)
	lines := []string{
		"",
		"  saint: network access",
		"",
		"  Session  " + printable(q.SessionName),
		"  Host     " + host,
		"",
		"  The agent in this session is asking to reach a host that is not on",
		"  your allowlist. The request waits for your answer.",
	}
	if !q.Deadline.IsZero() {
		lines = append(lines, "  With no answer by "+q.Deadline.Local().Format("15:04:05")+", it is denied.")
	}
	lines = append(lines,
		"",
		"  [a] Allow always   saves "+host+" for every session",
		"  [d] Deny           refuses this request only",
		"",
	)
	// Clear and home, then raw-mode line endings.
	fmt.Fprint(out, "\x1b[2J\x1b[H"+strings.Join(lines, "\r\n"))
}

// printable drops control characters from text that came from a guest (the
// host) or a config file (the session name), so it cannot drive the terminal.
func printable(value string) string {
	return journalText(value)
}
