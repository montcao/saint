package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/montcao/saint/internal/egress"
	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

// fakeHerdr records herdr commands. Opening the prompt pane runs prompt
// against a pipe served by the prompter, the way the real pane connects to
// the control socket.
type fakeHerdr struct {
	mu      sync.Mutex
	calls   [][]string
	failOn  string
	prompt  func(conn *controlConn)
	prompts *egressPrompter
	t       *testing.T
}

func (f *fakeHerdr) run(_ context.Context, pane store.HerdrPane, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{pane.SocketPath}, args...))
	f.mu.Unlock()
	if f.failOn != "" && slices.Contains(args, f.failOn) {
		return errors.New(`{"error":{"code":"pane_not_found","message":"pane w1:p2 not found"}}`)
	}
	if len(args) > 2 && args[0] == "plugin" && f.prompt != nil {
		server, client := net.Pipe()
		sessionID := strings.TrimPrefix(args[slices.Index(args, "--env")+1], envEgressSession+"=")
		go f.prompts.servePrompt(sessionID, newControlConn(server))
		go func() {
			defer client.Close()
			f.prompt(newControlConn(client))
		}()
	}
	return nil
}

func (f *fakeHerdr) commands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeHerdr) waitFor(what string, match func([]string) bool) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range f.commands() {
			if match(call) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("no herdr call for %s; calls: %q", what, f.commands())
}

func testPrompter(t *testing.T, sessions map[string]store.Session) (*egressPrompter, *fakeHerdr, *[]string) {
	t.Helper()
	saved := &[]string{}
	var savedMu sync.Mutex
	fake := &fakeHerdr{t: t}
	p := &egressPrompter{
		session: func(id string) (store.Session, bool) {
			s, ok := sessions[id]
			return s, ok
		},
		allow: func(host string) error {
			savedMu.Lock()
			defer savedMu.Unlock()
			*saved = append(*saved, host)
			return nil
		},
		run:      fake.run,
		stateDir: "/state",
		logf:     t.Logf,
		now:      time.Now,
		state:    map[string]*promptState{},
	}
	p.broker = egress.New(func(id string) { go p.sync(id) })
	fake.prompts = p
	return p, fake, saved
}

var herdrSession = store.Session{
	ID: "s1", Name: "web", Runtime: "codex",
	Herdr: &store.HerdrPane{PaneID: "w1:p2", SocketPath: "/herdr/one.sock"},
}

// The whole round trip: the pane is marked blocked naming the host, a prompt
// opens on it, "Allow always" lets the request through and saves the host,
// and the pane is released afterwards.
func TestAskingInHerdrAllowsAndSaves(t *testing.T) {
	t.Parallel()

	p, fake, saved := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	fake.prompt = func(conn *controlConn) {
		var resp controlResponse
		if err := conn.receive(&resp); err != nil || resp.Question == nil {
			t.Errorf("prompt got %+v, %v; want a question", resp, err)
			return
		}
		if resp.Question.Host != "paste.example" || resp.Question.SessionName != "web" {
			t.Errorf("question = %+v", resp.Question)
		}
		_ = conn.send(controlAnswer{ID: resp.Question.ID, Allow: true})
		if err := conn.receive(&resp); err != nil || !resp.Done {
			t.Errorf("after answering, prompt got %+v, %v; want done", resp, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionAllow {
		t.Fatalf("Ask() = %v, want allow", got)
	}
	if !slices.Equal(*saved, []string{"paste.example"}) {
		t.Errorf("saved = %v, want the allowed host", *saved)
	}

	fake.waitFor("release", func(c []string) bool { return slices.Contains(c, "release-agent") })
	calls := fake.commands()
	want := [][]string{
		{"/herdr/one.sock", "pane", "report-agent", "w1:p2", "--source", "saint", "--agent", "codex", "--state", "blocked", "--message", "saint: allow network access to paste.example?"},
		{"/herdr/one.sock", "plugin", "pane", "open", "--plugin", "saint", "--entrypoint", "egress", "--placement", "split", "--direction", "down", "--target-pane", "w1:p2", "--no-focus", "--env", "SAINT_EGRESS_SESSION=s1", "--env", "SAINT_HOME=/state"},
		{"/herdr/one.sock", "pane", "release-agent", "w1:p2", "--source", "saint", "--agent", "codex"},
	}
	if len(calls) != len(want) {
		t.Fatalf("herdr calls = %q, want %q", calls, want)
	}
	for i := range want {
		if !slices.Equal(calls[i], want[i]) {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}

// "Deny" refuses the request and saves nothing.
func TestAskingInHerdrDenies(t *testing.T) {
	t.Parallel()

	p, fake, saved := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	fake.prompt = func(conn *controlConn) {
		var resp controlResponse
		if err := conn.receive(&resp); err != nil || resp.Question == nil {
			return
		}
		_ = conn.send(controlAnswer{ID: resp.Question.ID, Allow: false})
		_ = conn.receive(&resp)
	}
	if got := p.Ask(context.Background(), "s1", "paste.example"); got != proxy.DecisionDeny {
		t.Fatalf("Ask() = %v, want deny", got)
	}
	if len(*saved) != 0 {
		t.Errorf("a denial saved %v", *saved)
	}
}

// A session with no herdr pane has nobody to ask: refused at once, as
// unanswered, without calling herdr.
func TestNoPaneIsUnanswered(t *testing.T) {
	t.Parallel()

	plain := herdrSession
	plain.Herdr = nil
	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": plain})
	if got := p.Ask(context.Background(), "s1", "paste.example"); got != proxy.DecisionAsk {
		t.Fatalf("Ask() = %v, want unanswered (DecisionAsk)", got)
	}
	if calls := fake.commands(); len(calls) != 0 {
		t.Errorf("herdr was called for a session with no pane: %q", calls)
	}
}

// A pane that herdr no longer has cannot be asked in, so the request is
// refused at once rather than waiting out the timeout.
func TestAGonePaneIsUnansweredAtOnce(t *testing.T) {
	t.Parallel()

	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	var forgotten []store.HerdrPane
	var forgetMu sync.Mutex
	p.forget = func(_ string, pane store.HerdrPane) {
		forgetMu.Lock()
		forgotten = append(forgotten, pane)
		forgetMu.Unlock()
	}
	fake.failOn = "report-agent"
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionAsk {
		t.Fatalf("Ask() = %v, want unanswered", got)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %s for a pane that does not exist", waited)
	}
	forgetMu.Lock()
	defer forgetMu.Unlock()
	if len(forgotten) != 1 || forgotten[0] != *herdrSession.Herdr {
		t.Errorf("forgotten = %+v, want the stale pane record cleared", forgotten)
	}
}

// Nobody answering within the deadline is a refusal, recorded as unanswered,
// and the pane is released.
func TestNoAnswerInTimeIsUnanswered(t *testing.T) {
	t.Parallel()

	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionAsk {
		t.Fatalf("Ask() = %v, want unanswered", got)
	}
	fake.waitFor("release", func(c []string) bool { return slices.Contains(c, "release-agent") })
}

// Closing the prompt without answering refuses what it was showing.
func TestClosingThePromptRefuses(t *testing.T) {
	t.Parallel()

	p, fake, _ := testPrompter(t, map[string]store.Session{"s1": herdrSession})
	fake.prompt = func(conn *controlConn) {
		var resp controlResponse
		_ = conn.receive(&resp)
		// The person closes the pane.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Ask(ctx, "s1", "paste.example"); got != proxy.DecisionAsk {
		t.Fatalf("Ask() = %v, want unanswered", got)
	}
	if ctx.Err() != nil {
		t.Fatal("the closed prompt was not noticed; the request waited out its deadline")
	}
}

func TestBlockedMessageCountsTheRest(t *testing.T) {
	t.Parallel()

	got := blockedMessage([]egress.Question{{Host: "a.example"}, {Host: "b.example"}, {Host: "c.example"}})
	if got != "saint: allow network access to a.example? (+2 more)" {
		t.Errorf("message = %q", got)
	}
}

// The proxy may have been started from a pane of another herdr server, so its
// inherited herdr variables are dropped and the pane's own socket is used.
func TestHerdrEnvTargetsThePanesServer(t *testing.T) {
	t.Parallel()

	env := herdrEnv([]string{"PATH=/bin", "HERDR_SOCKET_PATH=/other.sock", "HERDR_PANE_ID=w9:p9"}, store.HerdrPane{SocketPath: "/mine.sock"})
	if !slices.Equal(env, []string{"PATH=/bin", "HERDR_SOCKET_PATH=/mine.sock"}) {
		t.Errorf("env = %q", env)
	}
	if env := herdrEnv([]string{"HERDR_SOCKET_PATH=/other.sock"}, store.HerdrPane{}); len(env) != 0 {
		t.Errorf("env = %q, want herdr's own default socket", env)
	}
}

// The prompt pane: a key answers, leaving answers no, and a host from the guest
// cannot drive the terminal.
func TestEgressPromptKeys(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key       string
		wantAllow bool
	}{{"a", true}, {"d", false}, {"\x1b", false}, {"\x03", false}} {
		server, client := net.Pipe()
		srv := newControlConn(server)
		out := &lockedBuffer{}
		done := make(chan error, 1)
		keys, typeKey := net.Pipe()
		go func() { done <- egressPrompt(newControlConn(client), keys, out, 0) }()

		_ = srv.send(controlResponse{OK: true, Question: &controlQuestion{ID: 7, SessionName: "web", Host: "evil\x1b[2Jhost.example"}})
		// A key pressed before the question is drawn is ignored, so a stray
		// key cannot answer a question nobody has read.
		deadline := time.Now().Add(2 * time.Second)
		for !strings.Contains(out.String(), "Allow always") && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		_, _ = typeKey.Write([]byte(tc.key))
		var answer controlAnswer
		if err := srv.receive(&answer); err != nil {
			t.Fatalf("key %q: no answer sent: %v", tc.key, err)
		}
		if answer.ID != 7 || answer.Allow != tc.wantAllow {
			t.Errorf("key %q: answer = %+v, want allow=%v", tc.key, answer, tc.wantAllow)
		}
		if tc.key == "a" || tc.key == "d" {
			_ = srv.send(controlResponse{OK: true, Done: true})
		}
		if err := <-done; err != nil {
			t.Errorf("key %q: prompt returned %v", tc.key, err)
		}
		if strings.Contains(out.String(), "evil\x1b[2J") {
			t.Errorf("a control sequence from the host reached the terminal: %q", out.String())
		}
		server.Close()
		typeKey.Close()
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The prompt takes focus from wherever the person was typing, so keys that
// arrive in the moment after it is drawn are dropped rather than answering.
func TestEgressPromptIgnoresKeysJustAfterDrawing(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer server.Close()
	srv := newControlConn(server)
	keys, typeKey := net.Pipe()
	defer typeKey.Close()
	out := &lockedBuffer{}
	go func() { _ = egressPrompt(newControlConn(client), keys, out, 300*time.Millisecond) }()

	_ = srv.send(controlResponse{OK: true, Question: &controlQuestion{ID: 9, Host: "paste.example"}})
	for !strings.Contains(out.String(), "Allow always") {
		time.Sleep(time.Millisecond)
	}
	_, _ = typeKey.Write([]byte("a"))
	time.Sleep(400 * time.Millisecond)
	_, _ = typeKey.Write([]byte("d"))
	var answer controlAnswer
	if err := srv.receive(&answer); err != nil {
		t.Fatal(err)
	}
	if answer.Allow {
		t.Error("a key typed as the prompt appeared allowed the host")
	}
}
