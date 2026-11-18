package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many tunnels at once, with the saved policy changing under them and the
// activity table read while it is written. Run under -race (make check does),
// this is where a data race in the forward path would show: the policy read,
// the decision record, the per-host table and the tunnel byte counts are all
// shared between connections. It also checks the books balance: every CONNECT
// is either one tunnel or one denial, never both or neither.
func TestForwardPathUnderConcurrentLoad(t *testing.T) {
	t.Parallel()

	const workers, perWorker = 16, 12

	var allowed atomic.Bool
	var decisions atomic.Int64
	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), func(cfg *ForwardConfig) {
		cfg.Enforce = true
		cfg.LivePolicy = func() *HostPolicy {
			if allowed.Load() {
				return &HostPolicy{Allow: []string{"stress.example"}}
			}
			return &HostPolicy{}
		}
		cfg.OnDecision = func(DecisionEvent) { decisions.Add(1) }
	})

	stop := make(chan struct{})
	var background sync.WaitGroup
	background.Add(2)
	go func() { // flip the saved rule while requests are in flight
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
				allowed.Store(!allowed.Load())
				time.Sleep(time.Millisecond)
			}
		}
	}()
	go func() { // read the activity table while it is written
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = h.proxy.ForwardActivity()
			}
		}
	}()

	var ok, refused atomic.Int64
	errs := make(chan error, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				status, err := tunnelOnce(h.addr(), "stress.example:443", proxyCredentials(sessionToken))
				switch {
				case err != nil:
					errs <- err
				case status == http.StatusOK:
					ok.Add(1)
				case status == http.StatusForbidden:
					refused.Add(1)
				default:
					errs <- fmt.Errorf("CONNECT status %d", status)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	background.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	total := int64(workers * perWorker)
	t.Logf("%d tunnels, %d refused", ok.Load(), refused.Load())
	if ok.Load()+refused.Load() != total {
		t.Fatalf("ok %d + refused %d != %d requests", ok.Load(), refused.Load(), total)
	}
	if decisions.Load() != total {
		t.Errorf("%d decisions recorded for %d requests", decisions.Load(), total)
	}
	waitFor(t, "every tunnel and denial in the activity table", func() bool {
		row := hostRow(t, h.proxy.ForwardActivity(), sessionID, "stress.example")
		return row.Tunnels == ok.Load() && row.Denials == refused.Load()
	})
}

// tunnelOnce opens a CONNECT, and when it is granted, sends one line through
// the tunnel and checks the echo. It returns errors instead of failing the
// test, because it runs off the test goroutine.
func tunnelOnce(proxyAddr, target, auth string) (int, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", target, target, auth)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		return 0, err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(line, "pong-ping") {
		return 0, fmt.Errorf("tunnel echoed %q", line)
	}
	return http.StatusOK, nil
}
