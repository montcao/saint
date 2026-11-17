package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// recordingSandbox captures what a create would have asked the container CLI
// for, without a container runtime being present.
type recordingSandbox struct {
	opts     sandbox.CreateOptions
	proxyURL string
	// removed records that the container was replaced rather than reused, which
	// is the whole mechanism behind renewing a credential.
	removed bool

	networks        []string
	removedNetworks []string
	createErr       error
}

func (r *recordingSandbox) Create(_ context.Context, _ string, _ sandbox.Runtime, _ []sandbox.Repo, _, proxyURL string, opts sandbox.CreateOptions) error {
	r.opts = opts
	r.proxyURL = proxyURL
	return r.createErr
}

func (r *recordingSandbox) CreateNetwork(_ context.Context, name string) (string, error) {
	r.networks = append(r.networks, name)
	// A documentation address (RFC 5737): what the runtime reports is
	// discovered, never written down, so a test uses one that cannot be real.
	return "192.0.2.1", nil
}

func (r *recordingSandbox) RemoveNetwork(_ context.Context, name string) error {
	r.removedNetworks = append(r.removedNetworks, name)
	return nil
}

func (r *recordingSandbox) List(context.Context) ([]sandbox.Info, error) { return nil, nil }
func (r *recordingSandbox) Stop(context.Context, string) error           { return nil }
func (r *recordingSandbox) Start(context.Context, string) error          { return nil }
func (r *recordingSandbox) Remove(context.Context, string) error {
	r.removed = true
	return nil
}
func (r *recordingSandbox) AttachCmd(string, sandbox.AttachOptions) (*exec.Cmd, error) {
	return nil, nil
}

func newEgressService(t *testing.T) (*Service, *recordingSandbox, string) {
	t.Helper()
	home := t.TempDir()
	st := store.New(home)
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig() error = %v", err)
	}
	sb := &recordingSandbox{}
	return &Service{Store: st, Sandbox: sb}, sb, gitRepo(t, filepath.Join(home, "project"))
}

// Every session gets an internal network of its own, and its guest is pointed
// at the proxy on that network's gateway for both its provider API and its
// other HTTP traffic: on an internal network there is no other way out.
func TestCreatePutsTheSessionOnItsOwnNetwork(t *testing.T) {
	t.Parallel()

	svc, sb, repo := newEgressService(t)
	created, err := svc.Create(context.Background(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	want := "saint-" + created.ID
	if created.Network != want {
		t.Errorf("session network = %q, want %q", created.Network, want)
	}
	if !slices.Equal(sb.networks, []string{want}) {
		t.Errorf("networks created = %v, want [%s]", sb.networks, want)
	}
	if sb.opts.Network != want {
		t.Errorf("container network = %q, want %q", sb.opts.Network, want)
	}
	if !sb.opts.ProxyEgress {
		t.Error("guest was not pointed at the proxy for its HTTP traffic")
	}

	cfg, err := svc.Store.EnsureConfig()
	if err != nil {
		t.Fatal(err)
	}
	if wantURL := "http://192.0.2.1:" + strconv.Itoa(cfg.ProxyPort); sb.proxyURL != wantURL {
		t.Errorf("proxy URL = %q, want %q (the network's gateway and the store's port)", sb.proxyURL, wantURL)
	}

	stored, err := svc.Find(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Network != want {
		t.Errorf("stored network = %q, want %q", stored.Network, want)
	}
}

// A container that fails to start leaves no network behind.
func TestFailedCreateRemovesTheNetwork(t *testing.T) {
	t.Parallel()

	svc, sb, repo := newEgressService(t)
	sb.createErr = errors.New("image not found")
	if _, err := svc.Create(context.Background(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}}); err == nil {
		t.Fatal("Create() succeeded with a failing sandbox")
	}
	if !slices.Equal(sb.removedNetworks, sb.networks) || len(sb.networks) != 1 {
		t.Errorf("created %v, removed %v; a failed create must clean up its network", sb.networks, sb.removedNetworks)
	}
}

// Removing a session removes its network, after its container.
func TestDeleteRemovesTheNetwork(t *testing.T) {
	t.Parallel()

	svc, sb, repo := newEgressService(t)
	created, err := svc.Create(context.Background(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !sb.removed {
		t.Error("container was not removed")
	}
	if !slices.Equal(sb.removedNetworks, []string{created.Network}) {
		t.Errorf("networks removed = %v, want [%s]", sb.removedNetworks, created.Network)
	}
}

// A session from before per-session networks shares the runtime's network,
// which saint must never delete.
func TestDeleteLeavesASharedNetworkAlone(t *testing.T) {
	t.Parallel()

	svc, sb, _ := newEgressService(t)
	if err := svc.Store.AddSession(store.Session{ID: "legacy01", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), "legacy01"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if len(sb.removedNetworks) != 0 {
		t.Errorf("removed networks %v for a session that had none of its own", sb.removedNetworks)
	}
}

// A restarted guest must wait for the proxy again, not find the signal its
// previous run left behind.
func TestStartClearsTheProxyReadySignal(t *testing.T) {
	t.Parallel()

	svc, _, repo := newEgressService(t)
	created, err := svc.Create(context.Background(), CreateRequest{Repos: []sandbox.Repo{{Path: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(svc.Store.Paths().SessionsDir, created.ID, sandbox.ProxyReadyFile)
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ready); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("proxy-ready survived a restart (stat err = %v)", err)
	}
}
