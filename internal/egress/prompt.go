// Package egress carries the question "may this session reach this host?" from
// the proxy, which cannot answer it, to a person, who can.
//
// It is its own package so the dependency runs one way. The proxy takes a
// function and knows nothing about who answers; the side that asks a person
// (the herdr prompt in cmd/saint) takes this queue and knows nothing about
// sockets. Neither imports the other, and a proxy with nobody to ask simply has
// no broker, which it already treats as a refusal.
package egress

import (
	"context"
	"sync"
	"time"
)

// Answer is what a person said about a host.
type Answer int

const (
	// AnswerUnanswered is a refusal nobody chose: the guest stopped waiting,
	// or the place to answer was closed or could not be opened.
	AnswerUnanswered Answer = iota
	// AnswerDeny is a person refusing this request. It is not remembered.
	AnswerDeny
	// AnswerAlways permits the host and is saved, so it is never asked again.
	AnswerAlways
)

func (a Answer) String() string {
	switch a {
	case AnswerAlways:
		return "allow always"
	case AnswerDeny:
		return "deny"
	}
	return "no answer"
}

// Question is one pending decision. The same host asked about by the same
// session while a question is open joins that question rather than asking
// again: a package manager opens several connections to one registry at once,
// and a person should be asked once.
type Question struct {
	ID        uint64
	SessionID string
	Host      string
	// AskedAt is when the guest started waiting.
	AskedAt time.Time
	// Deadline is when the question is withdrawn and the request refused, or
	// zero when the asker set none.
	Deadline time.Time

	done   chan struct{}
	answer Answer
}

// Done is closed when the question is answered or withdrawn.
func (q Question) Done() <-chan struct{} { return q.done }

// Broker holds the questions waiting for a person.
//
// The zero value is not usable; call New.
type Broker struct {
	mu      sync.Mutex
	pending map[uint64]*Question
	order   []uint64
	nextID  uint64

	// notify is told which session's questions changed, so the caller can mark
	// that session's pane blocked or release it. It is called without the
	// broker's lock held.
	notify func(sessionID string)

	// now is overridable for tests.
	now func() time.Time
}

// New returns a broker that calls notify whenever a session's set of pending
// questions changes.
func New(notify func(sessionID string)) *Broker {
	return &Broker{
		pending: map[uint64]*Question{},
		notify:  notify,
		now:     time.Now,
	}
}

// Ask blocks until somebody answers or ctx ends, and returns the answer. A
// context that ends first is AnswerUnanswered, a refusal: the guest has stopped waiting, so there
// is no longer anyone the answer could help, and the question is withdrawn
// rather than left for a person to decide something that no longer matters.
func (b *Broker) Ask(ctx context.Context, sessionID, host string) Answer {
	if b == nil {
		return AnswerUnanswered
	}

	b.mu.Lock()
	q, joined := b.find(sessionID, host)
	if !joined {
		b.nextID++
		q = &Question{
			ID:        b.nextID,
			SessionID: sessionID,
			Host:      host,
			AskedAt:   b.now(),
			done:      make(chan struct{}),
		}
		if deadline, ok := ctx.Deadline(); ok {
			q.Deadline = deadline
		}
		b.pending[q.ID] = q
		b.order = append(b.order, q.ID)
	}
	b.mu.Unlock()

	if !joined {
		b.wake(sessionID)
	}

	select {
	case <-q.done:
		b.mu.Lock()
		defer b.mu.Unlock()
		return q.answer
	case <-ctx.Done():
		if !joined {
			// The asker that opened the question owns its lifetime.
			if b.resolve(q.ID, AnswerUnanswered) {
				b.wake(sessionID)
			}
		}
		return AnswerUnanswered
	}
}

// find returns the open question for a session and host. Callers hold b.mu.
func (b *Broker) find(sessionID, host string) (*Question, bool) {
	for _, id := range b.order {
		if q := b.pending[id]; q != nil && q.SessionID == sessionID && q.Host == host {
			return q, true
		}
	}
	return nil, false
}

// Pending lists one session's open questions, oldest first. An empty
// sessionID lists every session's.
func (b *Broker) Pending(sessionID string) []Question {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Question, 0, len(b.order))
	for _, id := range b.order {
		if q, ok := b.pending[id]; ok && (sessionID == "" || q.SessionID == sessionID) {
			out = append(out, *q)
		}
	}
	return out
}

// Answer resolves one question and reports whether it was still open.
// Answering a question that has already gone - because the guest stopped
// waiting - does nothing: the person answered a prompt that was true when they
// read it.
func (b *Broker) Answer(id uint64, answer Answer) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	q := b.pending[id]
	b.mu.Unlock()
	if q == nil || !b.resolve(id, answer) {
		return false
	}
	b.wake(q.SessionID)
	return true
}

// DenyAll refuses every open question for a session as unanswered, for when
// the place to answer them is gone (the prompt was closed, or could not be
// opened).
func (b *Broker) DenyAll(sessionID string) {
	if b == nil {
		return
	}
	denied := false
	for _, q := range b.Pending(sessionID) {
		if b.resolve(q.ID, AnswerUnanswered) {
			denied = true
		}
	}
	if denied {
		b.wake(sessionID)
	}
}

// resolve closes a question with an answer, once.
func (b *Broker) resolve(id uint64, answer Answer) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	q, ok := b.pending[id]
	if !ok {
		return false
	}
	delete(b.pending, id)
	b.order = removeID(b.order, id)
	q.answer = answer
	close(q.done)
	return true
}

func (b *Broker) wake(sessionID string) {
	if b.notify != nil {
		b.notify(sessionID)
	}
}

func removeID(ids []uint64, id uint64) []uint64 {
	out := ids[:0]
	for _, candidate := range ids {
		if candidate != id {
			out = append(out, candidate)
		}
	}
	return out
}
