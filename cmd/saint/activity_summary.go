package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/montcao/saint/internal/store"
)

const summaryHostLimit = 4096
const summarySessionLimit = 128

type summaryState struct {
	hosts    map[string]bool
	overflow bool
	refusals uint64
	dirty    bool
}

// activitySummary coalesces changes without polling or blocking proxy requests
// on herdr. Host/session bounds also bound memory from guest-chosen hosts.
type activitySummary struct {
	mu       sync.Mutex
	states   map[string]*summaryState
	wake     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	store    *store.Store
	run      herdrRunner
	terminal func(context.Context, store.HerdrPane) (string, error)
}

func newActivitySummary(st *store.Store) *activitySummary {
	ctx, cancel := context.WithCancel(context.Background())
	return &activitySummary{states: make(map[string]*summaryState), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), store: st, run: runHerdrAt, terminal: herdrPaneTerminal}
}

func (s *activitySummary) change(id, host string, refusal, refresh bool) {
	if !store.ValidSessionID(id) {
		return
	}
	s.mu.Lock()
	state := s.states[id]
	if state == nil {
		if len(s.states) >= summarySessionLimit {
			s.mu.Unlock()
			return
		}
		state = &summaryState{hosts: make(map[string]bool)}
		s.states[id] = state
	}
	changed := refresh
	if refusal {
		state.refusals++
		changed = true
	}
	if host != "" && !state.hosts[host] {
		if len(state.hosts) < summaryHostLimit {
			state.hosts[host] = true
			changed = true
		} else if !state.overflow {
			state.overflow = true
			changed = true
		}
	}
	if changed {
		state.dirty = true
	}
	s.mu.Unlock()
	if changed {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}
func (s *activitySummary) reached(id, host string) { s.change(id, host, false, false) }
func (s *activitySummary) refused(id string)       { s.change(id, "", true, false) }
func (s *activitySummary) refresh(id string)       { s.change(id, "", false, true) }
func (s *activitySummary) close()                  { s.cancel(); <-s.done }

type summarySnapshot struct {
	hosts    int
	overflow bool
	refusals uint64
}

func summaryTitle(v summarySnapshot, session store.Session) string {
	custody := "guest: proxy placeholder only"
	if session.ProxyAuthOrDefault() == store.ProxyAuthSubscription {
		custody = "guest: real subscription credential"
	}
	suffix := ""
	if v.overflow {
		suffix = "+"
	}
	return fmt.Sprintf("saint proxy | hosts reached %d%s | refusals %d | %s | since proxy start", v.hosts, suffix, v.refusals, custody)
}
func summaryArgs(paneID, title string) []string {
	return []string{"pane", "report-metadata", paneID, "--title", title}
}
func (s *activitySummary) loop() {
	defer close(s.done)
	// Cache successful reports only. Errors retry on the next state change,
	// never on a timer; a new pane identity always gets a fresh report.
	last := make(map[string]string)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		snapshots := make(map[string]summarySnapshot)
		s.mu.Lock()
		for id, v := range s.states {
			if v.dirty {
				snapshots[id] = summarySnapshot{len(v.hosts), v.overflow, v.refusals}
				v.dirty = false
			}
		}
		s.mu.Unlock()
		sessions, err := s.store.LoadSessions()
		if err != nil {
			continue
		}
		for _, session := range sessions {
			v, ok := snapshots[session.ID]
			if !ok || session.Herdr == nil {
				continue
			}
			pane := *session.Herdr
			title := summaryTitle(v, session)
			key := pane.SocketPath + "\x00" + pane.PaneID + "\x00" + pane.TerminalID + "\x00" + title
			if last[session.ID] == key {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
			terminal, err := s.terminal(ctx, pane)
			if err == nil && pane.TerminalID != "" && terminal == pane.TerminalID {
				err = s.run(ctx, pane, summaryArgs(pane.PaneID, title)...)
			} else {
				err = fmt.Errorf("pane identity changed")
			}
			cancel()
			if err == nil {
				last[session.ID] = key
			}
		}
	}
}

func refreshPaneSummary(st *store.Store, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = controlCall(ctx, st.Paths().ControlSocket, controlRequest{Op: controlOpSummary, Session: id})
}
