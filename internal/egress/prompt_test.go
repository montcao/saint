package egress

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The answer a person gives is the answer the guest gets.
func TestAnAnsweredQuestionReturnsThatAnswer(t *testing.T) {
	t.Parallel()

	cases := map[Answer]bool{
		AnswerOnce:    true,
		AnswerSession: true,
		AnswerAlways:  true,
		AnswerDeny:    false,
		AnswerNever:   false,
	}

	for answer, wantAllow := range cases {
		b := New(nil)
		var wg sync.WaitGroup
		wg.Add(1)

		var allow bool
		var remember Answer
		go func() {
			defer wg.Done()
			allow, remember = b.Ask(context.Background(), "s1", "paste.example")
		}()

		id := waitForQuestion(t, b)
		b.Answer(id, answer)
		wg.Wait()

		if allow != wantAllow {
			t.Errorf("answer %v gave allow=%v, want %v", answer, allow, wantAllow)
		}
		if remember != answer {
			t.Errorf("remember = %v, want %v", remember, answer)
		}
	}
}

// A guest that stopped waiting takes its question with it. Leaving it on screen
// would invite a person to decide something that no longer matters.
func TestAGuestGivingUpRemovesTheQuestion(t *testing.T) {
	t.Parallel()

	b := New(nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() {
		allow, _ := b.Ask(ctx, "s1", "paste.example")
		done <- allow
	}()

	waitForQuestion(t, b)
	cancel()

	select {
	case allow := <-done:
		if allow {
			t.Error("an abandoned question was answered as allow")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ask() did not return when the guest gave up")
	}

	if got := len(b.Pending()); got != 0 {
		t.Errorf("%d questions still pending after the guest gave up", got)
	}
}

// Answering a question that has already gone is a person pressing a key against
// a prompt that was true when they read it. It must not panic or block.
func TestAnsweringAVanishedQuestionIsHarmless(t *testing.T) {
	t.Parallel()

	b := New(nil)
	b.Answer(9999, AnswerAlways)
	if got := len(b.Pending()); got != 0 {
		t.Errorf("Pending() = %d, want 0", got)
	}
}

// Questions are answered oldest first, because that is the guest that has been
// blocked longest.
func TestPendingIsOldestFirst(t *testing.T) {
	t.Parallel()

	b := New(nil)
	for _, host := range []string{"first.example", "second.example", "third.example"} {
		go b.Ask(context.Background(), "s1", host)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(b.Pending()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	pending := b.Pending()
	if len(pending) != 3 {
		t.Fatalf("Pending() has %d questions, want 3", len(pending))
	}
	for i := 1; i < len(pending); i++ {
		if pending[i].ID < pending[i-1].ID {
			t.Errorf("questions are not oldest first: %v", pending)
		}
	}
}

// A nil broker is what a headless saint has, and it must deny rather than panic.
func TestANilBrokerDenies(t *testing.T) {
	t.Parallel()

	var b *Broker
	if allow, _ := b.Ask(context.Background(), "s1", "paste.example"); allow {
		t.Error("a nil broker allowed a host")
	}
	if got := b.Pending(); got != nil {
		t.Errorf("Pending() = %v, want nil", got)
	}
	b.Answer(1, AnswerAlways)
}

func waitForQuestion(t *testing.T, b *Broker) uint64 {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := b.Pending(); len(pending) > 0 {
			return pending[0].ID
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no question arrived")
	return 0
}
