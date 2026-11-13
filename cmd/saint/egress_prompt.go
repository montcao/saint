package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/montcao/saint/internal/egress"
	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// Asking a person about a host, in herdr.
//
// When a session asks to reach a host nothing has ruled on, the proxy holds
// the request (for at most the proxy's ask timeout) and the prompter:
//
//  1. marks the session's herdr pane blocked, with a message naming the host,
//     so the person sees which session needs them;
//  2. opens a split plugin pane beside that pane without taking focus (`saint
//     herdr egress`), which reads the session's questions from the control
//     socket and sends back "Allow always" or "Deny". The blocked status is
//     what draws the person over; the prompt never moves them;
//  3. once nothing is pending, releases its report so herdr's own detection
//     of the agent's state resumes.
//
// Each step happens when something changes and never on a timer: blocked
// means a person has to act, and nothing else is reported.
//
// A session with no herdr pane - started with `saint new` in a plain terminal,
// or whose pane has gone - has nobody to ask, and its request is refused with
// the reason in the proxy's output. `saint allow <host>` is the way to permit
// a host for it.

const (
	herdrEgressEntrypoint = "egress"
	envEgressSession      = "SAINT_EGRESS_SESSION"

	// herdrCallTimeout bounds one herdr command. A herdr that does not answer
	// must not hold a guest's request past its own deadline.
	herdrCallTimeout = 5 * time.Second

	// promptConnectTimeout is how long an opened prompt has to connect before
	// a new question may open another one.
	promptConnectTimeout = 10 * time.Second

	// herdrReportSource names saint as the reporter of a pane's agent state.
	herdrReportSource = "saint"
)

// herdrRunner runs one herdr command against the server that owns a pane.
type herdrRunner func(ctx context.Context, pane store.HerdrPane, args ...string) error

type egressPrompter struct {
	broker  *egress.Broker
	session func(id string) (store.Session, bool)
	allow   func(host string) error
	// forget clears a session's herdr pane record if it still names pane,
	// once herdr has said the pane is gone.
	forget   func(sessionID string, pane store.HerdrPane)
	run      herdrRunner
	stateDir string
	logf     func(string, ...any)
	now      func() time.Time

	// syncMu serialises herdr calls, so reports reach herdr in the order the
	// questions changed.
	syncMu sync.Mutex

	mu    sync.Mutex
	state map[string]*promptState
}

// promptState is what has been shown for one session.
type promptState struct {
	pane  store.HerdrPane
	agent string
	// reported is the blocked message last sent; empty means not reported.
	reported string
	// promptOpened is when a prompt was opened and has not connected yet.
	promptOpened time.Time
	// promptConnected means a prompt is showing this session's questions.
	promptConnected bool
}

func newEgressPrompter(st *store.Store, logf func(string, ...any)) *egressPrompter {
	p := &egressPrompter{
		session: func(id string) (store.Session, bool) {
			sessions, err := st.LoadSessions()
			if err != nil {
				return store.Session{}, false
			}
			for _, s := range sessions {
				if s.ID == id {
					return s, true
				}
			}
			return store.Session{}, false
		},
		allow: func(host string) error {
			_, err := st.AllowHost(host)
			return err
		},
		forget: func(sessionID string, pane store.HerdrPane) {
			_ = st.UpdateSession(sessionID, func(s *store.Session) {
				if s.Herdr != nil && *s.Herdr == pane {
					s.Herdr = nil
				}
			})
		},
		run:      runHerdrAt,
		stateDir: st.Paths().Root,
		logf:     logf,
		now:      time.Now,
		state:    map[string]*promptState{},
	}
	p.broker = egress.New(func(sessionID string) { go p.sync(sessionID) })
	return p
}

// Ask is the proxy's ForwardConfig.Ask.
func (p *egressPrompter) Ask(ctx context.Context, sessionID, host string) proxy.Decision {
	session, ok := p.session(sessionID)
	if !ok || session.Herdr == nil || session.Herdr.PaneID == "" {
		p.logf("session=%s denied %s: no herdr pane to ask in (allow it with: saint allow %s)", sessionID, host, host)
		return proxy.DecisionAsk
	}
	p.mu.Lock()
	st := p.stateFor(sessionID)
	st.pane = *session.Herdr
	st.agent = string(session.Runtime)
	if st.agent == "" {
		st.agent = string(sandbox.RuntimeClaude)
	}
	p.mu.Unlock()

	switch p.broker.Ask(ctx, sessionID, host) {
	case egress.AnswerAlways:
		return proxy.DecisionAllow
	case egress.AnswerDeny:
		return proxy.DecisionDeny
	default:
		// Not an answer: the proxy records it as unanswered and refuses.
		p.logf("session=%s denied %s: no answer", sessionID, host)
		return proxy.DecisionAsk
	}
}

func (p *egressPrompter) stateFor(sessionID string) *promptState {
	st, ok := p.state[sessionID]
	if !ok {
		st = &promptState{}
		p.state[sessionID] = st
	}
	return st
}

// sync brings the pane in line with the session's pending questions.
func (p *egressPrompter) sync(sessionID string) {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()

	pending := p.broker.Pending(sessionID)

	p.mu.Lock()
	st := p.stateFor(sessionID)
	pane, agent, reported := st.pane, st.agent, st.reported
	p.mu.Unlock()
	if pane.PaneID == "" {
		return
	}

	if len(pending) == 0 {
		if reported != "" {
			if err := p.herdr(pane, releaseAgentArgs(pane.PaneID, agent)...); err != nil {
				p.logf("session=%s herdr release-agent: %v", sessionID, err)
			}
			p.mu.Lock()
			st.reported = ""
			p.mu.Unlock()
		}
		return
	}

	message := blockedMessage(pending)
	if message != reported {
		if err := p.herdr(pane, reportBlockedArgs(pane.PaneID, agent, message)...); err != nil {
			// The pane is gone or herdr is not answering: there is nobody to
			// ask, and waiting out the timeout would only delay the refusal.
			p.logf("session=%s herdr report-agent: %v; denying its pending hosts", sessionID, err)
			if strings.Contains(err.Error(), "pane_not_found") && p.forget != nil {
				// A pane closed outright never got to clear its record.
				p.forget(sessionID, pane)
				p.mu.Lock()
				st.pane = store.HerdrPane{}
				p.mu.Unlock()
			}
			p.broker.DenyAll(sessionID)
			return
		}
		p.mu.Lock()
		st.reported = message
		p.mu.Unlock()
	}

	p.mu.Lock()
	open := !st.promptConnected && (st.promptOpened.IsZero() || p.now().Sub(st.promptOpened) > promptConnectTimeout)
	if open {
		st.promptOpened = p.now()
	}
	p.mu.Unlock()
	if !open {
		return
	}
	if err := p.herdr(pane, promptOpenArgs(pane.PaneID, sessionID, p.stateDir)...); err != nil {
		p.logf("session=%s herdr could not open the prompt: %v; denying its pending hosts", sessionID, err)
		p.mu.Lock()
		st.promptOpened = time.Time{}
		p.mu.Unlock()
		p.broker.DenyAll(sessionID)
	}
}

func (p *egressPrompter) herdr(pane store.HerdrPane, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), herdrCallTimeout)
	defer cancel()
	return p.run(ctx, pane, args...)
}

// servePrompt feeds one prompt pane its session's questions and applies its
// answers. It returns when nothing is pending or the prompt goes away; a
// prompt closed with questions still open has refused them.
func (p *egressPrompter) servePrompt(sessionID string, conn *controlConn) {
	session, _ := p.session(sessionID)
	name := session.Name
	if name == "" {
		name = sessionID
	}

	p.mu.Lock()
	st := p.stateFor(sessionID)
	st.promptConnected = true
	st.promptOpened = time.Time{}
	p.mu.Unlock()

	answers := make(chan controlAnswer)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			var a controlAnswer
			if err := conn.receive(&a); err != nil {
				return
			}
			select {
			case answers <- a:
			case <-time.After(time.Second):
			}
		}
	}()

	defer func() {
		p.mu.Lock()
		st.promptConnected = false
		p.mu.Unlock()
		// Questions left open when the prompt goes away were seen and not
		// allowed; nobody is left to answer them.
		p.broker.DenyAll(sessionID)
	}()

	for {
		pending := p.broker.Pending(sessionID)
		if len(pending) == 0 {
			// Close the window in which a question that arrives now would see
			// a prompt about to exit and not open another one.
			p.mu.Lock()
			pending = p.broker.Pending(sessionID)
			if len(pending) == 0 {
				st.promptConnected = false
			}
			p.mu.Unlock()
			if len(pending) == 0 {
				_ = conn.send(controlResponse{OK: true, Done: true})
				return
			}
		}
		q := pending[0]
		if err := conn.send(controlResponse{OK: true, Question: &controlQuestion{
			ID: q.ID, SessionID: sessionID, SessionName: name, Host: q.Host, Deadline: q.Deadline,
		}}); err != nil {
			return
		}

		select {
		case a := <-answers:
			if a.ID != q.ID {
				continue
			}
			answer := egress.AnswerDeny
			if a.Allow {
				answer = egress.AnswerAlways
				// Saved before the guest is let through, so the next request
				// to this host from any session is not asked about again.
				if err := p.allow(q.Host); err != nil {
					p.logf("session=%s allowed %s, but saving it failed: %v", sessionID, q.Host, err)
				}
			}
			if p.broker.Answer(q.ID, answer) {
				p.logf("session=%s %s: %s", sessionID, q.Host, answer)
			}
		case <-q.Done():
			_ = conn.send(controlResponse{OK: true, Expired: q.ID})
		case <-closed:
			return
		}
	}
}

// blockedMessage is what the pane shows while hosts wait.
func blockedMessage(pending []egress.Question) string {
	message := "saint: allow network access to " + pending[0].Host + "?"
	if more := len(pending) - 1; more > 0 {
		message += fmt.Sprintf(" (+%d more)", more)
	}
	return message
}

// The herdr commands, built here so a test can read them. herdr 0.9.5 rejects
// release-agent with the pane ID after its options, so the pane comes first in
// both.

func reportBlockedArgs(paneID, agent, message string) []string {
	return []string{"pane", "report-agent", paneID,
		"--source", herdrReportSource, "--agent", agent, "--state", "blocked", "--message", message}
}

func releaseAgentArgs(paneID, agent string) []string {
	return []string{"pane", "release-agent", paneID, "--source", herdrReportSource, "--agent", agent}
}

// promptOpenArgs opens the prompt as a split beside the session's pane and
// leaves focus where the person put it. An overlay would need the session's
// pane made active first (herdr opens overlays only on the active pane), which
// switches workspace under whoever is typing elsewhere.
func promptOpenArgs(paneID, sessionID, stateDir string) []string {
	return []string{
		"plugin", "pane", "open",
		"--plugin", herdrPluginID,
		"--entrypoint", herdrEgressEntrypoint,
		"--placement", "split",
		"--direction", "down",
		"--target-pane", paneID,
		"--no-focus",
		"--env", envEgressSession + "=" + sessionID,
		// The prompt is started by herdr, not by this process, so it is told
		// which state directory - and so which proxy - it answers.
		"--env", store.StateDirEnv + "=" + stateDir,
	}
}

// runHerdrAt runs herdr against the server that owns pane. The proxy may have
// been started from a pane of a different herdr server, so the herdr variables
// it inherited are dropped and the pane's own socket is used.
func runHerdrAt(ctx context.Context, pane store.HerdrPane, args ...string) error {
	bin := pane.BinPath
	if bin == "" {
		bin = "herdr"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = herdrEnv(os.Environ(), pane)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func herdrEnv(environ []string, pane store.HerdrPane) []string {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, "HERDR_") {
			continue
		}
		env = append(env, kv)
	}
	if pane.SocketPath != "" {
		env = append(env, "HERDR_SOCKET_PATH="+pane.SocketPath)
	}
	return env
}
