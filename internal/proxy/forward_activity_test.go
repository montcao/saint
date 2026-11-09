package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Many tunnels at once, run under -race, must produce counts that add up.
func TestConcurrentTunnelsAreCountedExactly(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	const tunnels = 24
	var wg sync.WaitGroup
	for i := range tunnels {
		wg.Add(1)
		go func() {
			defer wg.Done()

			host := fmt.Sprintf("host-%d.test:443", i%4)
			conn, reader, resp := sendConnect(t, h.addr(), host, proxyCredentials(sessionToken))
			if resp.StatusCode != http.StatusOK {
				t.Errorf("CONNECT %s: status = %d, want 200", host, resp.StatusCode)
				return
			}
			if _, err := conn.Write([]byte("ping\n")); err != nil {
				t.Errorf("write to %s error = %v", host, err)
				return
			}
			if _, err := reader.ReadString('\n'); err != nil {
				t.Errorf("read from %s error = %v", host, err)
			}
			conn.Close()
		}()
	}
	wg.Wait()

	total := func() (tunnelCount, bytesTo, bytesFrom int64) {
		for _, session := range h.proxy.ForwardActivity().Sessions {
			for _, row := range session.Hosts {
				tunnelCount += row.Tunnels
				bytesTo += row.BytesToHost
				bytesFrom += row.BytesFromHost
			}
		}
		return
	}

	waitFor(t, "every tunnel's bytes", func() bool {
		_, bytesTo, bytesFrom := total()
		return bytesTo == tunnels*5 && bytesFrom == tunnels*10
	})
	if got, _, _ := total(); got != tunnels {
		t.Fatalf("tunnels recorded = %d, want %d", got, tunnels)
	}
	report := h.proxy.ForwardActivity()
	if len(report.Sessions) != 1 || len(report.Sessions[0].Hosts) != 4 {
		t.Fatalf("report = %+v, want one session with four hosts", report)
	}
	if report.Tracked != 4 || report.Dropped != 0 {
		t.Errorf("Tracked/Dropped = %d/%d, want 4/0", report.Tracked, report.Dropped)
	}
}

// At the cap the table stops adding rows, keeps counting the ones it has, says
// how many observations it lost - and does not interfere with the traffic.
func TestHostTableCapStopsGrowingAndSaysSo(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(target.Close)

	h := newForwardHarness(t, strings.TrimPrefix(target.URL, "http://"), func(c *ForwardConfig) {
		c.MaxTrackedHosts = 2
	})
	proxyURL, err := url.Parse("http://saint:" + sessionToken + "@" + h.addr())
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	for i := range 5 {
		resp, err := client.Get(fmt.Sprintf("http://host-%d.test/", i))
		if err != nil {
			t.Fatalf("Get() through a full table error = %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200: a full table must not refuse traffic", i, resp.StatusCode)
		}
	}

	report := h.proxy.ForwardActivity()
	if report.Tracked != 2 || report.Max != 2 {
		t.Fatalf("Tracked/Max = %d/%d, want 2/2", report.Tracked, report.Max)
	}
	if report.Dropped != 3 {
		t.Fatalf("Dropped = %d, want 3: the report has to say what it stopped recording", report.Dropped)
	}
	if len(report.Sessions) != 1 || len(report.Sessions[0].Hosts) != 2 {
		t.Fatalf("report = %+v, want one session with the two rows it kept", report)
	}

	// The rows it kept keep counting.
	if got := report.Sessions[0].Hosts[0].Requests; got != 1 {
		t.Errorf("kept row Requests = %d, want 1", got)
	}
}

// The table is the only thing here that grows, so a session that ends has to be
// removable.
func TestForgetSessionDropsItsRows(t *testing.T) {
	t.Parallel()

	echo := echoServer(t)
	h := newForwardHarness(t, echo.Addr().String(), nil)

	conn, _, resp := sendConnect(t, h.addr(), "example.test:443", proxyCredentials(sessionToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	conn.Close()

	h.proxy.ForgetSession(sessionID)
	report := h.proxy.ForwardActivity()
	if len(report.Sessions) != 0 || report.Tracked != 0 {
		t.Fatalf("report after ForgetSession = %+v, want empty", report)
	}
}
