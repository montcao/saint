package store

import "testing"

// Zero must mean "the default", not "zero bytes" or "no files". A config
// written before these fields existed carries zeros for all three, and a
// journal that rotated at 0 bytes or kept 0 files would be a silent no-op.
func TestProxyLogSizeDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		maxBytes  int64
		keepFiles int
		wantBytes int64
		wantKeep  int
	}{
		{name: "unset", wantBytes: DefaultProxyLogMaxBytes, wantKeep: DefaultProxyLogKeepFiles},
		{name: "negative", maxBytes: -1, keepFiles: -1, wantBytes: DefaultProxyLogMaxBytes, wantKeep: DefaultProxyLogKeepFiles},
		{name: "configured", maxBytes: 1 << 20, keepFiles: 3, wantBytes: 1 << 20, wantKeep: 3},
		{name: "no history", maxBytes: 1 << 20, keepFiles: 1, wantBytes: 1 << 20, wantKeep: 1},
		{name: "too small to hold a record", maxBytes: 12, keepFiles: 2, wantBytes: minProxyLogMaxBytes, wantKeep: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{ProxyLogMaxBytes: tc.maxBytes, ProxyLogKeepFiles: tc.keepFiles}
			if got := cfg.ProxyLogMaxBytesOrDefault(); got != tc.wantBytes {
				t.Errorf("ProxyLogMaxBytesOrDefault() = %d, want %d", got, tc.wantBytes)
			}
			if got := cfg.ProxyLogKeepFilesOrDefault(); got != tc.wantKeep {
				t.Errorf("ProxyLogKeepFilesOrDefault() = %d, want %d", got, tc.wantKeep)
			}
		})
	}
}

// A fresh install must get the request records; an upgraded one must be able to
// turn them off and have that stick. Both follow from the field being written
// true by DefaultConfig and absent meaning off.
func TestProxyLogRequestsDefault(t *testing.T) {
	t.Parallel()

	if !DefaultConfig().ProxyLogRequestsOrDefault() {
		t.Error("a fresh config must journal requests")
	}
	if (Config{}).ProxyLogRequestsOrDefault() {
		t.Error("an absent proxy_log_requests must mean off, or it could never be turned off")
	}
}

// The journal must not land inside a session directory: those are mounted
// read-write into guests, and a guest that can rewrite the record of what it
// called can erase its own tracks.
func TestProxyLogPathIsOutsideEveryMount(t *testing.T) {
	t.Parallel()

	paths := DefaultPaths(t.TempDir())
	if paths.ProxyLogPath == "" {
		t.Fatal("ProxyLogPath is empty")
	}
	if dir := paths.ProxyLogPath; len(dir) > len(paths.SessionsDir) && dir[:len(paths.SessionsDir)] == paths.SessionsDir {
		t.Fatalf("ProxyLogPath %q sits under the mounted sessions dir %q", paths.ProxyLogPath, paths.SessionsDir)
	}
}
