// Package egress carries the question "may this session reach this host?" from
// the proxy, which cannot answer it, to the interface, which can.
//
// It is its own package so the dependency runs one way. The proxy takes a
// function and knows nothing about a terminal; the interface takes a queue and
// knows nothing about sockets. Neither imports the other, and a headless saint
// simply has no broker, which the proxy already treats as "nobody to ask".
package egress

import (
	"context"
	"sync"
	"time"
)

// Answer is what a person said about a host.
type Answer int

const (
	// AnswerDeny refuses this connection and is not remembered.
	AnswerDeny Answer = iota
	// AnswerOnce permits this connection and nothing after it.
	AnswerOnce
	// AnswerSession permits the host for the rest of this saint process.
	AnswerSession
	// AnswerAlways permits the host and writes it to the configuration.
	AnswerAlways
	// AnswerNever refuses the host and writes that to the configuration, so it
	// is never asked about again.
	AnswerNever
)

// Question is one pending decision.
type Question struct {
	ID        uint64
	SessionID string
	Host      string
	// AskedAt is when the guest started waiting, so the interface can show how
	// long it has been blocked rather than just that it is.
	AskedAt time.Time

	reply chan Answer
}

// Broker holds the questions waiting for a person.
//
// The zero value is not usable; call New.
type Broker struct {
	mu      sync.Mutex
	pending map[uint64]*Question
	order   []uint64
	nextID  uint64

	// notify wakes the interface when a question arrives. It is a function
	// rather than a channel so the caller can decide what waking means -
	// a Bubble Tea program send, in saint's case - without this package
	// knowing.
	notify func()

	// now is overridable for tests.
	now func() time.Time
}

// New returns a broker that calls notify whenever a question is added or
// answered, so the interface can redraw.
func New(notify func()) *Broker {
	return &Broker{
		pending: map[uint64]*Question{},
		notify:  notify,
		now:     time.Now,
	}
}

// Ask blocks until somebody answers or ctx ends.
//
// The returned bool is whether the host may be reached. A context that ends
// first takes the question with it: the guest has stopped waiting, so there is
// no longer anyone the answer could help, and leaving the question on screen
// would invite a person to decide something that no longer matters.
func (b *Broker) Ask(ctx context.Context, sessionID, host string) (allow bool, remember Answer) {
	if b == nil {
		return false, AnswerDeny
	}

	b.mu.Lock()
	b.nextID++
	q := &Question{
		ID:        b.nextID,
		SessionID: sessionID,
		Host:      host,
		AskedAt:   b.now(),
		reply:     make(chan Answer, 1),
	}
	b.pending[q.ID] = q
	b.order = append(b.order, q.ID)
	b.mu.Unlock()

	b.wake()

	select {
	case answer := <-q.reply:
		return answer == AnswerOnce || answer == AnswerSession || answer == AnswerAlways, answer
	case <-ctx.Done():
		b.drop(q.ID)
		b.wake()
		return false, AnswerDeny
	}
}

// Pending lists the questions waiting, oldest first.
func (b *Broker) Pending() []Question {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Question, 0, len(b.order))
	for _, id := range b.order {
		if q, ok := b.pending[id]; ok {
			out = append(out, *q)
		}
	}
	return out
}

// Answer resolves one question. Answering a question that has already gone -
// because the guest stopped waiting - does nothing rather than failing: the
// person pressed a key against a prompt that was true when they read it.
func (b *Broker) Answer(id uint64, answer Answer) {
	if b == nil {
		return
	}
	b.mu.Lock()
	q, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		b.order = removeID(b.order, id)
	}
	b.mu.Unlock()

	if ok {
		q.reply <- answer
	}
	b.wake()
}

func (b *Broker) drop(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, id)
	b.order = removeID(b.order, id)
}

func (b *Broker) wake() {
	if b.notify != nil {
		b.notify()
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
