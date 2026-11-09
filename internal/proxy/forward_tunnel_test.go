package proxy

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// http.Server.Shutdown does not touch hijacked connections, so a tunnel
// outlives the server that created it unless the proxy closes it itself. The
// client has to see the tunnel end, and the tracking set has to drain.
func TestCloseTunnelsEndsLiveTunnelsAndDrainsTheSet(t *testing.T) {
	t.Parallel()

	// An httptest server that answers nothing keeps the far end of the tunnel
	// open and silent, which is the state a quit finds a long-lived agent
	// tunnel in.
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(target.Close)

	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), nil)

	conn, reader, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if got := h.proxy.forward.tunnels.len(); got != 1 {
		t.Fatalf("tracked tunnels = %d, want 1", got)
	}

	// Nothing is sent through the tunnel, so this read blocks until something
	// ends the connection. If the read returns before CloseTunnels is called,
	// the tunnel was never live and the rest of this test proves nothing.
	ended := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		ended <- err
	}()

	select {
	case err := <-ended:
		t.Fatalf("tunnel ended on its own before CloseTunnels: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}

	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("read through a closed tunnel returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client never saw the tunnel close")
	}

	waitFor(t, "the tunnel set to drain", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})

	// The bytes that did cross are still in the activity table: closing tunnels
	// is a shutdown step, not a way to forget what was observed.
	if row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "example.test"); row.Tunnels != 1 {
		t.Errorf("Tunnels = %d after CloseTunnels, want 1", row.Tunnels)
	}
	conn.Close()
}

// A CONNECT that arrives after the sweep must not become a live tunnel: it
// would be exactly the connection shutdown just guaranteed does not exist.
func TestConnectAfterCloseTunnelsDoesNotOpenATunnel(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}

	conn, err := net.Dial("tcp", h.addr())
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer conn.Close()

	request := "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n" +
		"Proxy-Authorization: " + proxyCredentials(sessionToken) + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT error = %v", err)
	}

	// The connection is abandoned before the 200 is written, so the client sees
	// the socket close rather than a tunnel that works for a moment.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		t.Fatal("the proxy answered a CONNECT after CloseTunnels")
	}
	if got := h.proxy.forward.tunnels.len(); got != 0 {
		t.Errorf("tracked tunnels = %d after a refused CONNECT, want 0", got)
	}
}

// A tunnel that ends on its own has to unregister itself, or the set is a leak
// with a different name.
func TestFinishedTunnelLeavesTheTrackingSet(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	conn, reader, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through tunnel error = %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read through tunnel error = %v", err)
	}
	conn.Close()

	waitFor(t, "the finished tunnel to unregister", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})
}

// Shutdown must be callable whatever state the proxy is in - twice, with no
// tunnels, with forwarding off, and on a zero value.
func TestCloseTunnelsIsSafeWithNothingToClose(t *testing.T) {
	t.Parallel()

	var zero *Proxy
	if err := zero.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() on a nil proxy error = %v", err)
	}

	noForward := newProxy(t, newUpstream(t, &recordedRequest{}), nil)
	if err := noForward.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() with forwarding off error = %v", err)
	}

	h := newForwardHarness(t, "", nil)
	if err := h.proxy.CloseTunnels(); err != nil {
		t.Errorf("CloseTunnels() with no tunnels error = %v", err)
	}
	if err := h.proxy.CloseTunnels(); err != nil {
		t.Errorf("second CloseTunnels() error = %v", err)
	}
}

// Tunnels open while the sweep runs. Neither side may deadlock, and every
// tunnel that was accepted has to be gone afterwards.
func TestCloseTunnelsRacesNewTunnels(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(target.Close)
	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), nil)

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", h.addr())
			if err != nil {
				return
			}
			defer conn.Close()
			request := "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n" +
				"Proxy-Authorization: " + proxyCredentials(sessionToken) + "\r\n\r\n"
			// Every outcome here is legitimate - tunnelled then closed, or
			// refused because the sweep won the race - so nothing is asserted
			// about an individual connection. What is asserted is below.
			_, _ = conn.Write([]byte(request))
			_, _ = bufio.NewReader(conn).ReadByte()
		}()
	}

	if err := h.proxy.CloseTunnels(); err != nil {
		t.Fatalf("CloseTunnels() error = %v", err)
	}
	wg.Wait()

	waitFor(t, "every tunnel to be gone", func() bool {
		return h.proxy.forward.tunnels.len() == 0
	})
}
