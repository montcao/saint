package session

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

// recordingSandbox captures what a create would have asked the container CLI
// for, without a container runtime being present.
type recordingSandbox struct {
	opts sandbox.CreateOptions
	// removed records that the container was replaced rather than reused, which
	// is the whole mechanism behind renewing a credential.
	removed bool
}

func (r *recordingSandbox) Create(_ context.Context, _ string, _ sandbox.Runtime, _ []sandbox.Repo, _, _ string, opts sandbox.CreateOptions) error {
	r.opts = opts
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

// The setting is an install-wide choice, so a session must carry whatever the
// config says at the moment it is created - in both directions. A create that
// ignored the config in the off direction would point guests at a proxy that is
// not serving them.
func TestCreatePassesTheConfiguredEgressSetting(t *testing.T) {
	t.Parallel()

	for _, egress := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[egress], func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			st := store.New(home)
			cfg, err := st.EnsureConfig()
			if err != nil {
				t.Fatalf("EnsureConfig() error = %v", err)
			}
			cfg.ProxyGuestEgress = egress
			if err := st.SaveConfig(cfg); err != nil {
				t.Fatalf("SaveConfig() error = %v", err)
			}

			sb := &recordingSandbox{}
			svc := &Service{Store: st, Sandbox: sb, ProxyURL: "http://192.0.2.1:8787"}
			repo := gitRepo(t, filepath.Join(home, "project"))

			if _, err := svc.Create(context.Background(), CreateRequest{
				Repos: []sandbox.Repo{{Path: repo}},
			}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if sb.opts.ProxyEgress != egress {
				t.Errorf("CreateOptions.ProxyEgress = %v, want %v", sb.opts.ProxyEgress, egress)
			}
		})
	}
}
