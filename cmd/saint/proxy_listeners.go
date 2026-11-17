package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/montcao/saint/internal/proxy"
)

// listenerSet is the proxy's TCP listeners: one per container network it
// serves, each on that network's gateway and the state directory's port.
//
// Each listener has its own http.Server so its connections carry the network
// they arrived on, and a session token is accepted only on its own session's
// network (proxy.WithSessionScope). A session's guest can reach only its own
// gateway, so this costs a guest nothing; what it closes is a token that leaked
// from one session being used from another session's network.
//
// A listener outlives its network's bridge address (measured; see
// docs/research/spikes.md), and the runtime reuses a deleted network's subnet
// for the next one. So a gateway already bound may now belong to a different
// network: the listener is kept and the network it answers for is updated.
type listenerSet struct {
	port    int
	handler http.Handler
	// sessionNetwork reports the network a session runs on, for the scope
	// check. It reads the session records, which a guest cannot write.
	sessionNetwork func(sessionID string) string
	listenTCP      func(addr string) (net.Listener, error)
	// bindWait bounds how long ensure retries a gateway that is not up yet.
	bindWait time.Duration
	logf     func(string, ...any)

	mu    sync.Mutex
	bound map[string]*boundListener // by bind host
}

type boundListener struct {
	addr    string
	network string
	server  *http.Server
}

func newListenerSet(port int, handler http.Handler, sessionNetwork func(string) string, logf func(string, ...any)) *listenerSet {
	return &listenerSet{
		port:           port,
		handler:        handler,
		sessionNetwork: sessionNetwork,
		listenTCP:      func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		bindWait:       5 * time.Second,
		logf:           logf,
		bound:          map[string]*boundListener{},
	}
}

// ensure listens on host for network, if nothing does yet, and returns the
// address. A host whose address is not up yet - the container attaching to the
// network is still starting - is retried until bindWait.
func (s *listenerSet) ensure(ctx context.Context, network, host string) (string, error) {
	s.mu.Lock()
	if existing, ok := s.bound[host]; ok {
		if existing.network != network {
			s.logf("proxy: %s now serves network %s (was %s)", existing.addr, network, existing.network)
			existing.network = network
		}
		s.mu.Unlock()
		return existing.addr, nil
	}
	s.mu.Unlock()

	addr := net.JoinHostPort(host, strconv.Itoa(s.port))
	ln, err := s.bindWithRetry(ctx, addr)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.bound[host]; ok {
		// Another caller bound it first.
		_ = ln.Close()
		existing.network = network
		return existing.addr, nil
	}
	// The listener's own address, which is the port the kernel chose when
	// the configured one is 0.
	addr = ln.Addr().String()
	entry := &boundListener{addr: addr, network: network}
	entry.server = s.newServer(entry)
	s.bound[host] = entry
	go func() {
		if err := entry.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("proxy: listener %s stopped: %v", addr, err)
		}
	}()
	s.logf("proxy: listening on %s for network %s", addr, network)
	return addr, nil
}

func (s *listenerSet) bindWithRetry(ctx context.Context, addr string) (net.Listener, error) {
	deadline := time.Now().Add(s.bindWait)
	for {
		ln, err := s.listenTCP(addr)
		if err == nil {
			return ln, nil
		}
		// EADDRNOTAVAIL is the bridge not being up yet; anything else (the port
		// taken, a permission) will not change by waiting.
		if !errors.Is(err, syscall.EADDRNOTAVAIL) || time.Now().After(deadline) {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("listen on %s: %w", addr, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// newServer builds one listener's server. There is one set of timeouts,
// whichever network it serves.
func (s *listenerSet) newServer(entry *boundListener) *http.Server {
	return &http.Server{
		Handler: s.handler,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return proxy.WithSessionScope(ctx, func(sessionID string) bool {
				s.mu.Lock()
				network := entry.network
				s.mu.Unlock()
				return s.sessionNetwork(sessionID) == network
			})
		},

		// ReadHeaderTimeout bounds Slowloris: a client cannot hold a connection
		// open indefinitely by dribbling out request headers.
		ReadHeaderTimeout: 10 * time.Second,

		// WriteTimeout is deliberately left at 0. Claude Code streams SSE
		// responses that can run for many minutes; any write deadline would
		// truncate a long response mid-stream. Do not "fix" this by setting one.
		WriteTimeout: 0,
	}
}

// addrs lists the addresses listened on, sorted.
func (s *listenerSet) addrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.bound))
	for _, entry := range s.bound {
		out = append(out, entry.addr)
	}
	slices.Sort(out)
	return out
}

// shutdown stops every listener's server, letting requests in flight finish
// until ctx ends.
func (s *listenerSet) shutdown(ctx context.Context) error {
	s.mu.Lock()
	servers := make([]*http.Server, 0, len(s.bound))
	for _, entry := range s.bound {
		servers = append(servers, entry.server)
	}
	s.mu.Unlock()
	var errs []error
	for _, srv := range servers {
		errs = append(errs, srv.Shutdown(ctx))
	}
	return errors.Join(errs...)
}
