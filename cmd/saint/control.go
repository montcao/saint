package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// The proxy's control channel: a unix socket in the state directory, through
// which the processes around the proxy talk to it.
//
// Two things need it. A session's network gateway exists only once its
// container runs (docs/research/spikes.md), so the proxy cannot know in
// advance where to listen: the pane that started the container asks it to.
// And when the proxy needs a person to decide about a host, the prompt that
// asks them is a separate process in a herdr pane, which collects its
// questions and sends back the answers here.
//
// The socket is 0600 in a 0700 directory, so only this user's processes reach
// it, and it is not under any directory mounted into a guest. Messages are one
// JSON object per line in each direction.

const (
	controlOpPing   = "ping"
	controlOpListen = "listen"
	controlOpEgress = "egress"

	// controlTimeout bounds a ping or listen exchange. Listening can wait for a
	// session's gateway to come up, which takes under a second when measured.
	controlTimeout = 15 * time.Second

	// unixPathMax is the longest socket path macOS accepts, including the
	// terminating NUL in sockaddr_un.sun_path.
	unixPathMax = 103
)

// errProxyRunning means another proxy already serves this state directory.
var errProxyRunning = errors.New("a saint proxy is already running for this state directory")

type controlRequest struct {
	Op      string `json:"op"`
	Session string `json:"session,omitempty"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// ping
	PID       int      `json:"pid,omitempty"`
	Listeners []string `json:"listeners,omitempty"`

	// listen
	Addr string `json:"addr,omitempty"`

	// egress
	Question *controlQuestion `json:"question,omitempty"`
	Expired  uint64           `json:"expired,omitempty"`
	Done     bool             `json:"done,omitempty"`
}

// controlQuestion is one host a session is waiting on, as the prompt shows it.
type controlQuestion struct {
	ID          uint64    `json:"id"`
	SessionID   string    `json:"session_id"`
	SessionName string    `json:"session_name"`
	Host        string    `json:"host"`
	Deadline    time.Time `json:"deadline"`
}

// controlAnswer is the prompt's reply to one question.
type controlAnswer struct {
	ID    uint64 `json:"id"`
	Allow bool   `json:"allow"`
}

// controlHandlers is what the proxy process answers with.
type controlHandlers struct {
	listeners func() []string
	listen    func(ctx context.Context, sessionID string) (string, error)
	egress    func(sessionID string, conn *controlConn)
}

// controlConn is one connection's line-delimited JSON, in both directions.
type controlConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
}

func newControlConn(conn net.Conn) *controlConn {
	return &controlConn{conn: conn, reader: bufio.NewReader(conn)}
}

func (c *controlConn) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.conn.Write(append(data, '\n'))
	return err
}

func (c *controlConn) receive(v any) error {
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return err
		}
	}
	return json.Unmarshal(line, v)
}

// listenControl opens the control socket. A socket file left by a proxy that
// died is removed; one that still answers means a proxy is running, and this
// one must not take its place.
func listenControl(path string) (net.Listener, error) {
	if len(path) > unixPathMax {
		return nil, fmt.Errorf("control socket path %s is %d bytes, over the %d macOS allows; use a shorter SAINT_HOME", path, len(path), unixPathMax)
	}
	if _, err := os.Lstat(path); err == nil {
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, errProxyRunning
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale control socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	return ln, nil
}

// serveControl answers connections until ln is closed.
func serveControl(ln net.Listener, h controlHandlers, logf func(string, ...any)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(conn, h, logf)
	}
}

func handleControl(conn net.Conn, h controlHandlers, logf func(string, ...any)) {
	defer conn.Close()
	c := newControlConn(conn)

	_ = conn.SetReadDeadline(time.Now().Add(controlTimeout))
	var req controlRequest
	if err := c.receive(&req); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	switch req.Op {
	case controlOpPing:
		resp := controlResponse{OK: true, PID: os.Getpid()}
		if h.listeners != nil {
			resp.Listeners = h.listeners()
		}
		_ = c.send(resp)
	case controlOpListen:
		if h.listen == nil {
			_ = c.send(controlResponse{Error: "this proxy does not serve sessions"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
		defer cancel()
		addr, err := h.listen(ctx, req.Session)
		if err != nil {
			logf("control: listen for session %s: %v", req.Session, err)
			_ = c.send(controlResponse{Error: err.Error()})
			return
		}
		_ = c.send(controlResponse{OK: true, Addr: addr})
	case controlOpEgress:
		if h.egress == nil {
			_ = c.send(controlResponse{Error: "this proxy asks nobody about hosts"})
			return
		}
		h.egress(req.Session, c)
	default:
		_ = c.send(controlResponse{Error: fmt.Sprintf("unknown operation %q", req.Op)})
	}
}

// controlCall sends one request and reads one response.
func controlCall(ctx context.Context, path string, req controlRequest) (controlResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return controlResponse{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(controlTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	c := newControlConn(conn)
	if err := c.send(req); err != nil {
		return controlResponse{}, err
	}
	var resp controlResponse
	if err := c.receive(&resp); err != nil {
		if errors.Is(err, io.EOF) {
			return controlResponse{}, errors.New("the proxy closed the control connection without answering")
		}
		return controlResponse{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// pingProxy reports whether a proxy answers on the control socket.
func pingProxy(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := controlCall(ctx, path, controlRequest{Op: controlOpPing})
	return err == nil
}
