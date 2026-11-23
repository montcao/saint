package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/montcao/saint/internal/toolprofile"
)

func profileFixture(t *testing.T) (*AppleContainer, string, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact := func(name, body string, mode os.FileMode) toolprofile.Artifact {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(body))
		return toolprofile.Artifact{Path: p, SHA256: hex.EncodeToString(h[:])}
	}
	adapter := artifact("adapter", "#!/bin/sh\n[ \"$1\" != remove ] || exit 19\nexit 0\n", 0o700)
	cli := artifact("container", "#!/bin/sh\nif [ \"$1\" = --version ]; then echo "+toolprofile.RuntimeCommit+"; else printf '[]\\n'; fi\n", 0o700)
	tools := artifact("tools", "tools", 0o600)
	d := "sha256:" + strings.Repeat("1", 64)
	m := toolprofile.Manifest{Schema: 1, RuntimeCommit: toolprofile.RuntimeCommit, Format: "erofs", Adapter: adapter, Tools: tools, ToolSHA256: tools.SHA256, Root: artifact("root", "root", 0o600), Kernel: artifact("kernel", "kernel", 0o600), Review: artifact("review", "test only", 0o600), ImageReference: "example.invalid/image@" + d, ImageDigest: d, InitReference: "example.invalid/init@" + d, InitDigest: d}
	data, _ := json.Marshal(m)
	manifest := artifact("manifest", string(data), 0o600)
	a := NewAppleContainer(cli.Path, nil)
	a.SetProfileRoot(filepath.Join(dir, "protected"))
	return a, manifest.Path + "#" + manifest.SHA256, dir
}
func TestProfileReservationIsExclusive(t *testing.T) {
	a, ref, _ := profileFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- a.ReserveProfile(context.Background(), "saint-test", ref) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("reservation winners = %d", successes)
	}
	if err := a.ReserveProfile(context.Background(), "saint-second", ref); err != nil {
		t.Fatal(err)
	}
	first, err := a.profileRecord("saint-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.profileRecord("saint-second")
	if err != nil {
		t.Fatal(err)
	}
	if first.Owner == second.Owner || first.Directory == second.Directory {
		t.Fatal("sessions share writable identity")
	}
}
func TestProfileOwnershipFailureNeverFallsBackToLegacy(t *testing.T) {
	a, ref, _ := profileFixture(t)
	if err := a.RequireProfile("saint-test", ref); err == nil {
		t.Fatal("missing owner record accepted")
	}
	if err := a.ReserveProfile(context.Background(), "saint-test", ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(context.Background(), "saint-test"); err == nil {
		t.Fatal("helper removal failure ignored")
	}
	if err := a.RequireProfile("saint-test", ref); err != nil {
		t.Fatal("failed removal lost record", err)
	}
	p := a.profilePath("saint-test")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r profileRecord
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	r.Name = "saint-someone-else"
	data, _ = json.Marshal(r)
	if err = os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = a.Stop(context.Background(), "saint-test"); err == nil {
		t.Fatal("foreign ownership fell back to legacy CLI")
	}
}
