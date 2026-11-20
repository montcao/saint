package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/montcao/saint/internal/proxy"
	"github.com/montcao/saint/internal/store"
)

const (
	herdrActivityEntrypoint = "activity"
	envActivitySession      = "SAINT_ACTIVITY_SESSION"
	envActivityStream       = "SAINT_ACTIVITY_STREAM"
	activityQueueSize       = 32
	maxActivityPanes        = 128
	activityWriteTimeout    = 2 * time.Second
)

// activityFeed relays live journal events without polling or rereading
// rotations. The journal writer never waits for an activity pane.
type activityFeed struct {
	active atomic.Bool
	mu     sync.Mutex
	panes  map[string]*activitySubscription
}

type activitySubscription struct {
	records chan journalRecord
	done    chan struct{}
	stream  string
	viewing bool // guarded by the feed's mutex
	// Read only after done closes.
	overflow bool
}

func (f *activityFeed) reserve(id, stream string) (*activitySubscription, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old := f.panes[id]; old != nil {
		close(old.done)
	} else if len(f.panes) >= maxActivityPanes {
		return nil, false
	}
	if f.panes == nil {
		f.panes = make(map[string]*activitySubscription)
	}
	pane := &activitySubscription{
		records: make(chan journalRecord, activityQueueSize),
		done:    make(chan struct{}),
		stream:  stream,
	}
	f.panes[id] = pane
	f.active.Store(true)
	return pane, true
}

func (f *activityFeed) subscribe(id, stream string) (*activitySubscription, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pane := f.panes[id]
	if pane == nil || pane.stream != stream || pane.viewing {
		return nil, false
	}
	pane.viewing = true
	return pane, true
}

func (f *activityFeed) unsubscribe(id string, pane *activitySubscription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes[id] == pane {
		delete(f.panes, id)
		close(pane.done)
		f.active.Store(len(f.panes) > 0)
	}
}

func (f *activityFeed) publish(rec journalRecord) {
	if !f.active.Load() {
		return
	}
	switch rec.Kind {
	case journalKindEgress, journalKindUsage, journalKindUpstreamStatus, journalKindDrop:
	default:
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.Kind == journalKindDrop {
		for id, pane := range f.panes {
			f.send(id, pane, rec)
		}
	} else if pane := f.panes[rec.SessionID]; pane != nil {
		f.send(rec.SessionID, pane, rec)
	}
}

func (f *activityFeed) send(id string, pane *activitySubscription, rec journalRecord) {
	select {
	case pane.records <- rec:
	default:
		pane.overflow = true
		delete(f.panes, id)
		close(pane.done)
		f.active.Store(len(f.panes) > 0)
	}
}

type activityMessage struct {
	OK     bool           `json:"ok,omitempty"`
	Error  string         `json:"error,omitempty"`
	Record *journalRecord `json:"record,omitempty"`
}

func (j *journal) serveActivity(req controlRequest, conn *controlConn) {
	defer conn.conn.Close()
	send := func(msg activityMessage) error {
		if err := conn.conn.SetWriteDeadline(time.Now().Add(activityWriteTimeout)); err != nil {
			return err
		}
		return conn.send(msg)
	}
	if !store.ValidSessionID(req.Session) || len(req.Stream) < 26 || len(req.Stream) > 64 || j.closed.Load() {
		_ = send(activityMessage{Error: "session activity is unavailable"})
		return
	}
	owner := req.Op == controlOpActivityOwner
	var pane *activitySubscription
	var ok bool
	if owner {
		pane, ok = j.activity.reserve(req.Session, req.Stream)
	} else {
		pane, ok = j.activity.subscribe(req.Session, req.Stream)
	}
	if !ok {
		_ = send(activityMessage{Error: "activity is unavailable for this attachment"})
		return
	}
	defer j.activity.unsubscribe(req.Session, pane)
	if err := send(activityMessage{OK: true}); err != nil {
		return
	}

	// An idle pane can disappear without another record arriving. The read
	// waits for socket closure and is interrupted by this handler's own close.
	disconnected := make(chan struct{})
	go func() {
		_, _ = conn.reader.ReadByte()
		close(disconnected)
	}()
	if owner {
		select {
		case <-disconnected:
		case <-pane.done:
		case <-j.done:
		}
		return
	}
	for {
		select {
		case <-disconnected:
			return
		case <-j.done:
			return
		case <-pane.done:
			if pane.overflow {
				_ = send(activityMessage{Error: "activity fell behind; see saint proxy log for recent records"})
			}
			return
		case rec := <-pane.records:
			if err := send(activityMessage{Record: &rec}); err != nil {
				return
			}
		}
	}
}

func openActivityPane(pane store.HerdrPane, sessionID string, paths store.Paths) io.Closer {
	ctx, cancel := context.WithTimeout(context.Background(), herdrCallTimeout)
	defer cancel()
	stream := rand.Text()
	owner, err := dialActivity(ctx, paths.ControlSocket, controlRequest{Op: controlOpActivityOwner, Session: sessionID, Stream: stream})
	if err != nil {
		fmt.Fprintf(os.Stderr, "saint: could not open session activity: %s\n", journalText(err.Error()))
		return nil
	}
	args := sessionSplitArgs(herdrActivityEntrypoint, envActivitySession, pane.PaneID, sessionID, paths.Root)
	args = append(args, "--env", envActivityStream+"="+stream)
	if err := runHerdrAt(ctx, pane, args...); err != nil {
		_ = owner.conn.Close()
		fmt.Fprintf(os.Stderr, "saint: could not open session activity: %s\n", journalText(err.Error()))
		return nil
	}
	return owner.conn
}

func runHerdrActivity(ctx context.Context, socketPath, sessionID, stream string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer stop()
	err := streamActivity(ctx, socketPath, sessionID, stream, out)
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(out, "saint: %s\n", journalText(err.Error()))
		return err
	}
	return nil
}

func dialActivity(ctx context.Context, socketPath string, req controlRequest) (*controlConn, error) {
	dial := net.Dialer{Timeout: activityWriteTimeout}
	raw, err := dial.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("reach the proxy: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	conn := newControlConn(raw)
	_ = raw.SetDeadline(time.Now().Add(activityWriteTimeout))
	if err := conn.send(req); err != nil {
		_ = raw.Close()
		return nil, err
	}
	var ack activityMessage
	if err := conn.receive(&ack); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if !ack.OK {
		_ = raw.Close()
		if strings.HasPrefix(ack.Error, "unknown operation") {
			return nil, errors.New("restart the older saint proxy and reopen the session for activity")
		}
		return nil, errors.New(journalText(ack.Error))
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

func streamActivity(ctx context.Context, socketPath, sessionID, stream string, out io.Writer) error {
	conn, err := dialActivity(ctx, socketPath, controlRequest{Op: controlOpActivity, Session: sessionID, Stream: stream})
	if err != nil {
		return err
	}
	defer conn.conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.conn.Close() })
	defer stop()
	if _, err := fmt.Fprintln(out, "Session activity · Ctrl-C closes this pane"); err != nil {
		return err
	}
	view := activityView{out: out}
	for {
		var msg activityMessage
		if err := conn.receive(&msg); err != nil {
			if ctx.Err() == nil && errors.Is(err, io.EOF) {
				return view.flushRepeats()
			}
			return err
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.Record != nil {
			if err := view.render(*msg.Record); err != nil {
				return err
			}
		}
	}
}

type activityView struct {
	out           io.Writer
	host, verdict string
	repeats       int
}

func (v *activityView) flushRepeats() error {
	if v.repeats > 0 {
		_, err := fmt.Fprintf(v.out, "%s %s (%d more)\n", v.host, v.verdict, v.repeats)
		v.repeats = 0
		return err
	}
	return nil
}

func (v *activityView) render(rec journalRecord) error {
	switch rec.Kind {
	case journalKindEgress:
		host := journalText(rec.Path)
		if proxy.IsClaudeTelemetryHost(rec.Path) {
			host += " [telemetry]"
		}
		verdict := activityVerdict(rec.Decision, rec.Reason)
		if v.host == host && v.verdict == verdict {
			v.repeats++
			return nil
		}
		if err := v.flushRepeats(); err != nil {
			return err
		}
		v.host, v.verdict = host, verdict
		_, err := fmt.Fprintf(v.out, "%s %s\n", host, verdict)
		return err
	case journalKindUpstreamStatus:
		if err := v.flushRepeats(); err != nil {
			return err
		}
		_, err := fmt.Fprintf(v.out, "%s HTTP %d\n", journalText(rec.Provider), rec.Status)
		return err
	case journalKindUsage:
		if err := v.flushRepeats(); err != nil {
			return err
		}
		rec.Model = journalText(rec.Model)
		what, result := journalSummary(rec)
		_, err := fmt.Fprintf(v.out, "%s %s %s\n", journalText(rec.Provider), what, result)
		return err
	case journalKindDrop:
		if err := v.flushRepeats(); err != nil {
			return err
		}
		v.host, v.verdict = "", ""
		_, err := fmt.Fprintf(v.out, "activity gap (%d proxy records dropped)\n", rec.Dropped)
		return err
	default:
		return nil
	}
}

func activityVerdict(decision, reason string) string {
	if decision == proxy.DecisionDeny.String() {
		return "refused (" + egressReasonText(reason) + ")"
	}
	switch reason {
	case proxy.ReasonDefault:
		return "allowed (default)"
	case proxy.ReasonRule:
		return "allowed (saved rule)"
	case proxy.ReasonAnswer:
		return "asked: allowed"
	default:
		return "allowed (" + egressReasonText(reason) + ")"
	}
}
