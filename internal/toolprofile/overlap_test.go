package toolprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverlapIdentities(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "State")
	project := filepath.Join(base, "Project")
	for _, p := range []string{state, project} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	artifact := filepath.Join(state, "Artifact")
	if err := os.WriteFile(artifact, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{state, artifact}, {artifact, state}, {base, state}, {state, base}, {state, state}} {
		if !Overlaps(pair[0], pair[1]) {
			t.Fatal("overlap accepted")
		}
	}
	if inside, err := Contains(base, state); err != nil || inside {
		t.Fatal("ancestor passed containment allowlist")
	}
	if Overlaps(project, state) || Overlaps(project, artifact) {
		t.Fatal("disjoint project refused")
	}
	if !Overlaps(project, filepath.Join(base, "missing")) {
		t.Fatal("inspection failure accepted")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	if !Overlaps(project, alias) {
		t.Fatal("symlink accepted")
	}
	link := filepath.Join(project, "linked")
	if err := os.Link(artifact, link); err != nil {
		t.Fatal(err)
	}
	if !Overlaps(artifact, link) {
		t.Fatal("hardlink identity missed")
	}
	if err := PrivatePath(artifact, false); err == nil {
		t.Fatal("hardlinked artifact accepted")
	}
}

func TestOverlapCaseInsensitiveAliases(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "ProtectedState")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "protectedstate")
	a, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("requires case-insensitive filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(state)
	if err != nil || !os.SameFile(a, b) {
		t.Fatal("fixture is not an alias")
	}
	artifact := filepath.Join(state, "ToolDisk")
	if err := os.WriteFile(artifact, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(state, "Child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{alias, state}, {filepath.Join(alias, "child"), state}, {alias, artifact}, {filepath.Join(alias, "tooldisk"), artifact}} {
		if !Overlaps(pair[0], pair[1]) || !Overlaps(pair[1], pair[0]) {
			t.Fatal("case alias accepted")
		}
	}
	project := filepath.Join(base, "Project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if Overlaps(project, state) || Overlaps(project, artifact) {
		t.Fatal("disjoint project refused")
	}
}
