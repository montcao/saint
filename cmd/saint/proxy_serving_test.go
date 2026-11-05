package main

import (
	"context"
	"errors"
	"testing"

	"github.com/montcao/saint/internal/store"
	"github.com/montcao/saint/internal/usage"
)

// The meter is only a measurement while a proxy in this process is the one
// carrying the traffic. Every other outcome leaves it reading zero forever, and
// a caller that rendered that would report every session idle and every total
// nil - answers that are wrong rather than absent.
func TestEmbeddedProxyReportsWhetherItIsServing(t *testing.T) {
	t.Parallel()

	st := store.New(t.TempDir())
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}

	status, serving, shutdown := startEmbeddedProxy(
		context.Background(), st, nil, "", newDebugLogger(st), usage.NewMeter(),
		errors.New("no container network"),
	)
	defer shutdown()

	if serving {
		t.Error("a proxy that never started reported itself as serving")
	}
	if status == "" {
		t.Error("a proxy that never started said nothing about it")
	}
}
