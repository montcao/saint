package inventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montcao/saint/internal/sandbox"
	"github.com/montcao/saint/internal/store"
)

type listSandbox struct{ infos []sandbox.Info }

func (s listSandbox) List(context.Context) ([]sandbox.Info, error) { return s.infos, nil }
func (listSandbox) Create(context.Context, string, sandbox.Runtime, []sandbox.Repo, string, string, sandbox.CreateOptions) error {
	return nil
}
func (listSandbox) CreateNetwork(context.Context, string) (string, error) { return "", nil }
func (listSandbox) RemoveNetwork(context.Context, string) error           { return nil }
func (listSandbox) Stop(context.Context, string) error                    { return nil }
func (listSandbox) Start(context.Context, string) error                   { return nil }
func (listSandbox) Remove(context.Context, string) error                  { return nil }
func (listSandbox) AttachCmd(string, sandbox.AttachOptions) (*exec.Cmd, error) {
	return nil, nil
}

func TestLoadListsSessionsThenOrphans(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	st := store.New(home)
	if _, err := st.EnsureConfig(); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "work", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSessions([]store.Session{
		{ID: "bbbbbbbb", Name: "beta", Runtime: sandbox.RuntimeCodex, Repos: []sandbox.Repo{{Path: repo}}},
		{ID: "aaaaaaaa", Name: "alpha", Runtime: sandbox.RuntimeClaude, Repos: []sandbox.Repo{{Path: repo}}},
	}); err != nil {
		t.Fatal(err)
	}
	loader := &Loader{
		Store: st,
		Sandbox: listSandbox{infos: []sandbox.Info{
			{Name: "saint-aaaaaaaa", State: sandbox.StateRunning},
			{Name: "saint-zzzzzzzz", State: sandbox.StateRunning},
			{Name: "someone-elses", State: sandbox.StateRunning},
		}},
		Inspector: NewRepoInspector(gitPath, nil),
	}
	snap, err := loader.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, row := range snap.Rows {
		got = append(got, row.Name+"="+row.State)
	}
	want := "alpha=running beta=missing saint-zzzzzzzz=orphan (running)"
	if strings.Join(got, " ") != want {
		t.Fatalf("rows = %v, want %s", got, want)
	}
	if snap.Rows[0].Container != "saint-aaaaaaaa" || snap.Rows[0].Repos != "app" {
		t.Errorf("alpha row = %+v", snap.Rows[0])
	}

	table := FormatTable(snap)
	for _, want := range []string{"CONTAINER", "saint-aaaaaaaa", "orphan (running)"} {
		if !strings.Contains(table, want) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}
	if strings.Contains(table, "someone-elses") {
		t.Errorf("table lists a container saint does not own:\n%s", table)
	}
}

func TestFormatTableListsFilesToReview(t *testing.T) {
	table := FormatTable(Snapshot{Rows: []Row{{Name: "alpha", Repos: "app!", ReviewPaths: []string{".git/hooks/pre-commit"}}}})
	if !strings.Contains(table, ".git/hooks/pre-commit") || !strings.Contains(table, "review") {
		t.Fatalf("review paths not shown:\n%s", table)
	}
}
