// Package archtest holds structural tests: rules about the shape of the code
// that docs/design/principles.md, ARCHITECTURE.md and package comments state in
// prose, checked so that breaking one fails `make check` instead of waiting for
// a reviewer to remember it.
//
// Each failure names the file and line and says what to do instead.
package archtest

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/montcao/saint"

// repoRoot is the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// goFiles lists the repository's tracked Go files, relative to root. Tracked
// files only, so a scratch file in a worktree cannot fail the build.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// The palette is the only file that names a colour. Everywhere else refers to
// its col* constants, which is what keeps "one colour, one meaning" true: a
// literal elsewhere is a colour nobody reviewed against the rest.
func TestOnlyThePaletteNamesAColour(t *testing.T) {
	root := repoRoot(t)
	const palette = "internal/tui/palette.go"
	fset := token.NewFileSet()

	for _, rel := range goFiles(t, root) {
		if rel == palette || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		// Parsed under the relative name so failures name the file the
		// same way on every machine.
		file, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Color" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "lipgloss" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				t.Errorf("%s: lipgloss.Color(%s) names a colour outside %s; add a col* constant there and use it",
					fset.Position(lit.Pos()), lit.Value, palette)
			}
			return true
		})
	}
}

// importRules are the package boundaries the code states. The egress package
// exists so the proxy and the interface never import each other: the proxy
// takes a function and knows nothing about a terminal, the interface takes a
// queue and knows nothing about sockets (internal/egress/prompt.go).
var importRules = []struct {
	pkg       string
	forbidden []string
	instead   string
}{
	{"internal/egress", []string{"internal/proxy", "internal/tui"}, "egress carries questions between them and must depend on neither"},
	{"internal/proxy", []string{"internal/tui", "internal/egress"}, "take a function (see proxy.ForwardConfig.Ask) instead of importing the side that answers"},
	{"internal/tui", []string{"internal/proxy"}, "consume proxy data through internal/usage or a queue such as internal/egress"},
}

func TestPackageBoundaries(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	for _, rule := range importRules {
		matches, err := filepath.Glob(filepath.Join(root, rule.pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Fatalf("%s has no Go files; update importRules if the package moved", rule.pkg)
		}
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, imp := range file.Imports {
				target, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range rule.forbidden {
					if target == module+"/"+bad {
						rel, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d: %s imports %s; %s",
							rel, fset.Position(imp.Pos()).Line, rule.pkg, bad, rule.instead)
					}
				}
			}
		}
	}
}

// maxLines is the most a Go file may hold. Files already past it are held at
// their current length so they can shrink but not grow; splitting them is
// tracked in docs/plans/tech-debt.md. Lower an entry when a file shrinks.
const maxLines = 800

var grandfathered = map[string]int{
	"internal/tui/model.go":          1859,
	"internal/proxy/forward.go":      1195,
	"internal/proxy/forward_test.go": 1053,
	"internal/session/service.go":    900,
	"internal/store/store.go":        918,
	"internal/tui/rail.go":           819,
}

func TestFilesDoNotGrowPastTheLimit(t *testing.T) {
	root := repoRoot(t)
	tracked := map[string]bool{}
	for _, rel := range goFiles(t, root) {
		tracked[rel] = true
		n, err := countLines(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		limit, old := grandfathered[rel]
		if !old {
			limit = maxLines
		}
		if n > limit {
			if old {
				t.Errorf("%s: %d lines, but it is held at %d; move code out of it rather than adding to it",
					rel, n, limit)
			} else {
				t.Errorf("%s: %d lines, over the %d-line limit; split it by responsibility",
					rel, n, maxLines)
			}
		}
	}
	for rel := range grandfathered {
		if !tracked[rel] {
			t.Errorf("grandfathered entry %s no longer exists; remove it from the map", rel)
		}
	}
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for s.Scan() {
		n++
	}
	return n, s.Err()
}

var (
	homePath  = regexp.MustCompile(`/Users/[A-Za-z0-9._-]+`)
	privateIP = regexp.MustCompile(`\b(10\.\d{1,3}|192\.168|172\.(1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b`)
)

// Nothing machine-local in code or tests (docs/design/principles.md). No file may name a real
// home directory, in code or in a comment; tests may use the fake home
// /Users/tester. Production code may hold no private-network address in a
// string literal, because those are discovered at runtime. Comments may cite
// one as an observation, and tests may use recorded addresses in fixtures,
// which are data being parsed rather than values saint relies on.
func TestNothingMachineLocal(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range goFiles(t, root) {
		isTest := strings.HasSuffix(rel, "_test.go")
		if rel == "internal/archtest/archtest_test.go" {
			continue // the patterns themselves
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			where := fmt.Sprintf("%s:%d", rel, i+1)
			for _, m := range homePath.FindAllString(line, -1) {
				if isTest && m == "/Users/tester" {
					continue
				}
				t.Errorf("%s: %q is a real home directory; use t.TempDir() or os.UserHomeDir(), or the fixture /Users/tester in a test", where, m)
			}
		}
		if isTest {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, data, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if m := privateIP.FindString(lit.Value); m != "" {
				t.Errorf("%s: %q is a private-network address in a string literal; discover it at runtime (see internal/sandbox/network.go)",
					fset.Position(lit.Pos()), m)
			}
			return true
		})
	}
}
