package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostReachedWithoutUsageCallback(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer upstream.Close()
	cfg := testConfig(t, upstream, nil)
	var got string
	cfg.OnHostReached = func(id, host string) {
		if id != sessionID {
			t.Errorf("wrong session %s", id)
		}
		got = host
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", sessionToken)
	p.ServeHTTP(httptest.NewRecorder(), req)
	if got == "" {
		t.Fatal("upstream response was not observed")
	}
}
