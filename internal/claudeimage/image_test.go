package claudeimage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	clauderecipe "github.com/montcao/saint/images/claude"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionParsing(t *testing.T) {
	for _, s := range []string{"2.1.332 (Claude Code)\n", "2.1.332"} {
		if v, e := ParseVersion(s); e != nil || v != "2.1.332" {
			t.Fatal(v, e)
		}
	}
	for _, s := range []string{"latest", "2.1.3;touch file", "2.1.3\n2.1.4", "02.1.3", "2.1.3-beta", "2.1.3+build", "--help", "2.1.3 (Other)", strings.Repeat("1", 1000)} {
		if _, e := ParseVersion(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestCacheIdentity(t *testing.T) {
	a, _ := Select("2.1.3", "arm64", []byte("recipe"))
	for _, c := range []struct{ v, a, r string }{{"2.1.4", "arm64", "recipe"}, {"2.1.3", "amd64", "recipe"}, {"2.1.3", "arm64", "changed"}} {
		b, e := Select(c.v, c.a, []byte(c.r))
		if e != nil || a.Image == b.Image {
			t.Fatal("cache collision", e)
		}
	}
	b, _ := Select("2.1.3", "arm64", []byte("recipe"))
	if a != b {
		t.Fatal("unstable identity")
	}
	if !ValidSelection(a.Version, a.Architecture, a.Recipe, a.Image) {
		t.Fatal("selection rejected")
	}
	if _, e := Select("latest", "arm64", nil); e == nil {
		t.Fatal("latest accepted")
	}
	if _, e := Select("2.1.3", "unknown", nil); e == nil {
		t.Fatal("architecture accepted")
	}
}
func TestPrepareExactVersionAndCache(t *testing.T) {
	for _, scenario := range []string{"build", "cached", "unavailable", "mismatch", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "claude")
			os.WriteFile(bin, []byte("synthetic"), 0700)
			recipe := filepath.Join(dir, "Dockerfile")
			os.WriteFile(recipe, []byte("FROM synthetic\nARG CLAUDE_VERSION\n"), 0600)
			var progress bytes.Buffer
			built, verified := 0, 0
			m := Manager{Container: "container", LockDir: filepath.Join(dir, "locks"), Architecture: "arm64", Progress: &progress, LookPath: func(string) (string, error) { return bin, nil }}
			m.Run = func(_ context.Context, b string, args []string, out io.Writer) (string, error) {
				if len(args) == 1 && args[0] == "--version" {
					return "2.1.3 (Claude Code)", nil
				}
				switch args[0] {
				case "image":
					selected, _ := Select("2.1.3", "arm64", clauderecipe.Recipe())
					if args[1] == "list" {
						if built > 0 || scenario == "cached" {
							return selected.Image + "\n", nil
						}
						return "", nil
					}
					return `[ {"configuration":{"name":"` + args[2] + `","descriptor":{"digest":"sha256:` + strings.Repeat("a", 64) + `"}}} ]`, nil
				case "build":
					built++
					joined := strings.Join(args, " ")
					if !strings.Contains(joined, "CLAUDE_VERSION=2.1.3") || strings.Contains(joined, "latest") {
						t.Fatal(joined)
					}
					entries, e := os.ReadDir(args[len(args)-1])
					if e != nil || len(entries) != 1 || entries[0].Name() != "Dockerfile" {
						t.Fatal("unsafe build context", entries, e)
					}
					if scenario == "unavailable" {
						return "", errors.New("not published")
					}
					return "", nil
				case "network":
					return "", nil
				case "run":
					verified++
					joined := strings.Join(args, " ")
					if strings.Contains(joined, "--volume") || strings.Contains(joined, "--ssh") || !strings.Contains(joined, "DISABLE_UPDATES=1") || !strings.Contains(joined, "@sha256:") {
						t.Fatal("unsafe verification")
					}
					if scenario == "mismatch" {
						return "2.1.2 (Claude Code)", nil
					}
					if scenario == "unsupported" {
						return "", errors.New("exec format error")
					}
					return "2.1.3 (Claude Code)", nil
				}
				t.Fatal(args)
				return "", nil
			}
			result, err := m.Prepare(t.Context())
			fail := scenario == "unavailable" || scenario == "mismatch" || scenario == "unsupported"
			if (err != nil) != fail {
				t.Fatalf("error=%v", err)
			}
			if scenario == "unavailable" && verified != 0 {
				t.Fatal("verified unavailable image")
			}
			if scenario == "cached" && (built != 0 || !result.Cached) {
				t.Fatal("cache not reused")
			}
			if !fail && (verified != 1 || result.Version != "2.1.3") {
				t.Fatal(result)
			}
		})
	}
}
func TestCaptureBoundCannotBeBypassed(t *testing.T) {
	var b boundedBuffer
	if _, e := io.Copy(&b, strings.NewReader(strings.Repeat("x", 70000))); e == nil {
		t.Fatal("unbounded capture")
	}
	if b.data.Len() > 65536 {
		t.Fatal("capture exceeded bound")
	}
}

func TestRecipeCannotBeOverriddenByWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.MkdirAll(filepath.Join(dir, "images", "claude"), 0700)
	os.WriteFile(filepath.Join(dir, "images", "claude", "Dockerfile"), []byte("FROM untrusted"), 0600)
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("fixture"), 0700)
	m := Manager{LookPath: func(string) (string, error) { return bin, nil }, Run: func(context.Context, string, []string, io.Writer) (string, error) { return "2.1.3", nil }}
	_, recipe, e := m.Resolve(t.Context())
	if e != nil || !bytes.Equal(recipe, clauderecipe.Recipe()) || bytes.Contains(recipe, []byte("untrusted")) {
		t.Fatal("caller recipe used", e)
	}
}
func TestCacheInspectionErrorsNeverBuild(t *testing.T) {
	for _, failure := range []string{"list", "inspect", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "claude")
			os.WriteFile(bin, []byte("fixture"), 0700)
			selected, _ := Select("2.1.3", "arm64", clauderecipe.Recipe())
			m := Manager{Container: "container", LockDir: dir, Architecture: "arm64", LookPath: func(string) (string, error) { return bin, nil }, Run: func(_ context.Context, _ string, a []string, _ io.Writer) (string, error) {
				if a[0] == "--version" {
					return "2.1.3", nil
				}
				if a[0] != "image" {
					t.Fatal("mutated after uncertain cache", a)
				}
				if a[1] == failure {
					return "", errors.New("inspection failed")
				}
				if a[1] == "list" {
					return selected.Image, nil
				}
				return "not JSON", nil
			}}
			if _, e := m.Prepare(t.Context()); e == nil {
				t.Fatal("cache inspection failure ignored")
			}
		})
	}
}
func TestRetainedDigestChangeStopsBeforeVerification(t *testing.T) {
	s, _ := Select("2.1.3", "arm64", clauderecipe.Recipe())
	s.Digest = "sha256:" + strings.Repeat("a", 64)
	m := Manager{Run: func(_ context.Context, _ string, a []string, _ io.Writer) (string, error) {
		if a[0] != "image" {
			t.Fatal("mutated retained image")
		}
		if a[1] == "list" {
			return s.Image, nil
		}
		b, _ := json.Marshal([]any{map[string]any{"configuration": map[string]any{"name": s.Image, "descriptor": map[string]string{"digest": "sha256:" + strings.Repeat("b", 64)}}}})
		return string(b), nil
	}}
	if e := m.VerifyRetained(t.Context(), s); e == nil {
		t.Fatal("changed digest accepted")
	}
}

func TestAliasMismatchStopsBeforeGuestExecution(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			selected, _ := Select("2.1.3", "arm64", []byte("recipe"))
			selected.Digest = "sha256:" + strings.Repeat("a", 64)
			ref := Reference(selected.Image, selected.Digest)
			tagged := false
			m := Manager{Run: func(_ context.Context, _ string, a []string, _ io.Writer) (string, error) {
				if a[0] != "image" {
					t.Fatal("guest resource allocated for wrong alias", a)
				}
				switch a[1] {
				case "list":
					if existing {
						return ref, nil
					}
					return "", nil
				case "tag":
					tagged = true
					return "", nil
				case "inspect":
					return `[{"configuration":{"name":"` + ref + `","descriptor":{"digest":"sha256:` + strings.Repeat("b", 64) + `"}}}]`, nil
				}
				t.Fatal(a)
				return "", nil
			}}
			if err := m.verify(t.Context(), selected); err == nil {
				t.Fatal("wrong alias accepted")
			}
			if existing && tagged {
				t.Fatal("existing alias overwritten")
			}
		})
	}
}
