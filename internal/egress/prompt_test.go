package egress

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitForQuestion returns the first open question for a session.
func waitForQuestion(t *testing.T, b *Broker, sessionID string) Question {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending := b.Pending(sessionID); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no question arrived")
	return Question{}
}

// The answer a person gives is the answer the guest gets.
func TestAnAnsweredQuestionReturnsThatAnswer(t *testing.T) {
	t.Parallel()

	for _, answer := range []Answer{AnswerAlways, AnswerDeny} {
		b := New(nil)
		got := make(chan Answer, 1)
		go func() { got <- b.Ask(context.Background(), "s1", "paste.example") }()

		q := waitForQuestion(t, b, "s1")
		if !b.Answer(q.ID, answer) {
			t.Fatal("Answer() reported the open question as gone")
		}
		if g := <-got; g != answer {
			t.Errorf("Ask() = %v, want %v", g, answer)
		}
	}
}

// A guest that stopped waiting takes its question with it, and that is a
// refusal nobody chose.
func TestAGuestGivingUpRemovesTheQuestion(t *testing.T) {
	t.Parallel()

	b := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan Answer, 1)
	go func() { got <- b.Ask(ctx, "s1", "paste.example") }()

	q := waitForQuestion(t, b, "s1")
	cancel()
	if g := <-got; g != AnswerUnanswered {
		t.Errorf("Ask() = %v after the guest gave up, want no answer", g)
	}
	if pending := b.Pending(""); len(pending) != 0 {
		t.Errorf("pending = %+v, want none", pending)
	}
	select {
	case <-q.Done():
	default:
		t.Error("a withdrawn question's Done channel is still open")
	}
	if b.Answer(q.ID, AnswerAlways) {
		t.Error("answering a withdrawn question reported success")
	}
}

// The same host asked about by the same session joins the open question: a
// person is asked once for a burst of connections to one registry.
func TestConcurrentAsksForOneHostShareAQuestion(t *testing.T) {
	t.Parallel()

	b := New(nil)
	const askers = 5
	answers := make(chan Answer, askers)
	var started sync.WaitGroup
	started.Add(askers)
	for range askers {
		go func() {
			started.Done()
			answers <- b.Ask(context.Background(), "s1", "registry.example")
		}()
	}
	started.Wait()
	q := waitForQuestion(t, b, "s1")
	// Give the joiners time to arrive; a joiner that opened its own question
	// would show up here.
	time.Sleep(20 * time.Millisecond)
	if pending := b.Pending("s1"); len(pending) != 1 {
		t.Fatalf("pending = %d questions, want 1 shared question", len(pending))
	}
	b.Answer(q.ID, AnswerAlways)
	for range askers {
		if got := <-answers; got != AnswerAlways {
			t.Errorf("a joined asker got %v, want allow always", got)
		}
	}
}

// Questions are per session: another session asking about the same host is
// asked separately, because it is a different agent doing a different thing.
func TestSessionsAreAskedSeparately(t *testing.T) {
	t.Parallel()

	b := New(nil)
	go b.Ask(context.Background(), "s1", "paste.example")
	go b.Ask(context.Background(), "s2", "paste.example")
	waitForQuestion(t, b, "s1")
	waitForQuestion(t, b, "s2")
	if got := len(b.Pending("s1")); got != 1 {
		t.Errorf("s1 has %d questions, want 1", got)
	}
	b.DenyAll("s1")
	b.DenyAll("s2")
}

// DenyAll refuses every open question for one session and no other.
func TestDenyAllIsPerSession(t *testing.T) {
	t.Parallel()

	b := New(nil)
	s1 := make(chan Answer, 2)
	go func() { s1 <- b.Ask(context.Background(), "s1", "a.example") }()
	go func() { s1 <- b.Ask(context.Background(), "s1", "b.example") }()
	s2 := make(chan Answer, 1)
	go func() { s2 <- b.Ask(context.Background(), "s2", "a.example") }()

	waitForQuestion(t, b, "s2")
	deadline := time.Now().Add(2 * time.Second)
	for len(b.Pending("s1")) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	b.DenyAll("s1")
	for range 2 {
		if got := <-s1; got != AnswerUnanswered {
			t.Errorf("s1 got %v, want no answer", got)
		}
	}
	if pending := b.Pending("s2"); len(pending) != 1 {
		t.Fatalf("s2 pending = %d, want its question untouched", len(pending))
	}
	b.DenyAll("s2")
	<-s2
}

// notify names the session whose questions changed, on arrival and on answer.
func TestNotifyNamesTheSession(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	b := New(func(sessionID string) {
		mu.Lock()
		seen = append(seen, sessionID)
		mu.Unlock()
	})
	done := make(chan struct{})
	go func() { b.Ask(context.Background(), "s7", "paste.example"); close(done) }()
	q := waitForQuestion(t, b, "s7")
	b.Answer(q.ID, AnswerDeny)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "s7" || seen[1] != "s7" {
		t.Errorf("notify calls = %q, want s7 on arrival and on answer", seen)
	}
}

// The deadline a guest waits under is carried on the question, so a prompt can
// say when no answer becomes a denial.
func TestTheQuestionCarriesTheDeadline(t *testing.T) {
	t.Parallel()

	b := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	go b.Ask(ctx, "s1", "paste.example")
	q := waitForQuestion(t, b, "s1")
	want, _ := ctx.Deadline()
	if !q.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", q.Deadline, want)
	}
	b.DenyAll("s1")
}

// A nil broker is "nobody to ask", which is a refusal.
func TestANilBrokerDenies(t *testing.T) {
	t.Parallel()

	var b *Broker
	if got := b.Ask(context.Background(), "s1", "paste.example"); got != AnswerUnanswered {
		t.Errorf("nil broker Ask() = %v, want deny", got)
	}
}
