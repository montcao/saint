package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// The proxy port is chosen once per state directory and then kept. It cannot
// be a fixed number, because two copies of saint with different state
// directories would then fight over it; and it cannot be chosen per start,
// because every session's guest has the proxy URL, port included, written into
// its environment at creation. So the first EnsureConfig asks the kernel for a
// free port and writes it into config.json, and every later start reads it
// back. A port the user set by hand is never replaced.

func validProxyPort(port int) bool {
	return port > 0 && port <= 65535
}

// pickProxyPort asks the kernel for a port that is free right now. Nothing
// holds it afterwards; it is persisted, not reserved.
func pickProxyPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("choose proxy port: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !validProxyPort(addr.Port) {
		return 0, fmt.Errorf("choose proxy port: unexpected listener address %s", ln.Addr())
	}
	return addr.Port, nil
}

// ensureProxyPort gives a loaded config a port if it has none (or an unusable
// one) and saves it, so the next load returns the same port.
func (s *Store) ensureProxyPort(cfg Config) (Config, error) {
	if validProxyPort(cfg.ProxyPort) {
		return cfg, nil
	}
	port, err := pickProxyPort()
	if err != nil {
		return Config{}, err
	}
	cfg.ProxyPort = port
	if err := s.SaveConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// createConfig writes the first config for a state directory. A herdr pane and
// the proxy it starts can both reach here on a fresh install, and each would
// pick a different port; the file is therefore created only if absent, and the
// loser reads back the winner's port instead of overwriting it.
func (s *Store) createConfig() (Config, error) {
	cfg := DefaultConfig()
	port, err := pickProxyPort()
	if err != nil {
		return Config{}, err
	}
	cfg.ProxyPort = port

	err = writeJSONExclusive(s.paths.ConfigPath, cfg, 0o600)
	if errors.Is(err, fs.ErrExist) {
		loaded, err := s.LoadConfig()
		if err != nil {
			return Config{}, err
		}
		return s.ensureProxyPort(loaded)
	}
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// writeJSONExclusive writes value to path only if path does not exist yet,
// returning an error wrapping fs.ErrExist otherwise. The content is written to
// a temporary file first and hard-linked into place, so a reader never sees a
// partly written file.
func writeJSONExclusive(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	return nil
}
