package proxy

import (
	"errors"
	"net"
	"sync"
)

// CloseTunnels closes every hijacked connection this proxy is holding open, and
// stops it accepting new ones.
//
// http.Server.Shutdown does not touch hijacked connections - its documentation
// says so outright - and a CONNECT tunnel is hijacked by definition. Without
// this, quitting saint while a tunnel is open returns from Shutdown with the
// socket, its two copy goroutines and the guest's connection all still live:
// the server is gone and the thing it created has outlived it. In a running
// saint that is a process that will not exit, which is the failure this
// codebase treats as the worst one it can ship. In a test it is a leaked
// goroutine that makes some unrelated test flake.
//
// It is a shutdown operation and it is final. After it returns, a CONNECT that
// was mid-hijack is closed rather than tunnelled, and later ones are refused
// the same way, because a tunnel that opened just after the sweep would be one
// the shutdown believed it had closed. Calling it twice, on a proxy with no
// tunnels, or on one with forward proxying switched off, is safe and does
// nothing.
//
// The error is what closing the sockets reported, joined. Connections the far
// end had already closed are left out of it: that is the state this method was
// asking for, not a failure to reach it.
func (p *Proxy) CloseTunnels() error {
	if p == nil || p.forward == nil {
		return nil
	}
	return p.forward.tunnels.closeAll()
}

// liveTunnels holds the hijacked connections the forward path currently has
// open, so shutdown can reach them. Proxy.CloseTunnels says why anything has
// to.
//
// There is no configured ceiling here, and that is a deliberate difference from
// forwardTable below rather than an omission. That table is capped because its
// keys accumulate for the life of the process and both halves - session IDs and
// guest-supplied hostnames - are attacker-influenced. This set holds one entry
// per socket that is open at this instant, and every entry removes itself when
// its tunnel ends, so its size is the number of live tunnels: a quantity the
// file descriptor limit already bounds far below anything a map cares about. A
// number on top of that could only do one of two things and both are worse than
// no cap. Refusing a tunnel at the ceiling would drop legitimate agent traffic
// to protect a map. Serving it untracked would leave running exactly the
// connection CloseTunnels exists to close.
type liveTunnels struct {
	mu     sync.Mutex
	closed bool
	next   uint64
	open   map[uint64]tunnelPair
}

// tunnelPair is the two ends of one tunnel. Both are kept, not just the
// guest's: closing only the client end leaves the goroutine copying from the
// target blocked on a read that nothing is going to answer.
type tunnelPair struct {
	client   net.Conn
	upstream net.Conn
}

func newLiveTunnels() *liveTunnels {
	return &liveTunnels{open: make(map[uint64]tunnelPair)}
}

// add registers a live tunnel and returns the function that unregisters it.
//
// The key is a counter rather than the connection itself, so a tunnel removes
// exactly the entry it added even if the runtime hands out an identical
// net.Conn value later.
//
// ok is false once closeAll has run, and the caller must then abandon the
// tunnel rather than serve it.
func (s *liveTunnels) add(client, upstream net.Conn) (remove func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	id := s.next
	s.next++
	s.open[id] = tunnelPair{client: client, upstream: upstream}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.open, id)
	}, true
}

// closeAll ends every tunnel and closes the set to new ones.
func (s *liveTunnels) closeAll() error {
	s.mu.Lock()
	s.closed = true
	pairs := make([]tunnelPair, 0, len(s.open))
	for _, pair := range s.open {
		pairs = append(pairs, pair)
	}
	clear(s.open)
	s.mu.Unlock()

	// The sockets are closed outside the lock. Close on a connection with a
	// blocked reader takes as long as the kernel takes, and holding the lock
	// across all of them would stall every tunnel trying to unregister itself
	// at the same moment - which, during a quit, is all of them.
	var errs []error
	for _, pair := range pairs {
		errs = append(errs, closeTunnelEnd(pair.client), closeTunnelEnd(pair.upstream))
	}
	return errors.Join(errs...)
}

// closeTunnelEnd closes one end and drops the error that only says it was
// already closed. A tunnel finishing on its own at the same moment reports
// that, and passing it on from a method whose entire job is to close things
// would be noise no caller can act on.
func closeTunnelEnd(conn net.Conn) error {
	if conn == nil {
		return nil
	}
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// len reports how many tunnels are open. Tests use it to check that the set
// drains rather than inferring it from the client side.
func (s *liveTunnels) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}
