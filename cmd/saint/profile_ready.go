package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/montcao/saint/internal/store"
)

func markDirectProxyReady(st *store.Store, id, marker string) error {
	if !store.ValidSessionID(id) || marker != "direct-proxy-ready" {
		return errors.New("invalid direct readiness target")
	}
	dir := filepath.Join(st.Paths().SessionsDir, id)
	f, err := os.CreateTemp(dir, ".direct-ready-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Close(); err != nil {
		return err
	}
	// Rename replaces a guest-planted symlink/FIFO without following it.
	return os.Rename(f.Name(), filepath.Join(dir, marker))
}

func requireProfileProxy(ctx context.Context, deps sessionDeps) error {
	resp, err := controlCall(ctx, deps.svc.Store.Paths().ControlSocket, controlRequest{Op: controlOpPing})
	if err != nil {
		return err
	}
	if !resp.ProfileV1 {
		return errors.New("experimental profiles require an updated Saint proxy; existing proxy and sessions were left untouched. Update it after other sessions stop, or use an isolated SAINT_HOME")
	}
	return nil
}

func listenProfile(ctx context.Context, deps sessionDeps, id string) (string, error) {
	resp, err := controlCall(ctx, deps.svc.Store.Paths().ControlSocket, controlRequest{Op: controlOpProfileListen, Session: id})
	return resp.Addr, err
}
