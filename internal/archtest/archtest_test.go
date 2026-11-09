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
	"sort"
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

// importRules are the package boundaries the code states. Each is checked
// transitively: a package may not reach a forbidden one through any chain of
// imports, because an indirect dependency couples them as tightly as a direct
// one. The egress package
// exists so the proxy never imports whatever answers its questions: the proxy
// takes a function and knows nothing about a terminal, the answering side takes
// a queue and knows nothing about sockets (internal/egress/prompt.go).
var importRules = []struct {
	pkg       string
	forbidden []string
	instead   string
}{
	{"internal/egress", []string{"internal/proxy"}, "egress carries questions from the proxy and must not depend on it"},
	{"internal/proxy", []string{"internal/egress"}, "take a function (see proxy.ForwardConfig.Ask) instead of importing the side that answers"},
}

func TestPackageBoundaries(t *testing.T) {
	root := repoRoot(t)
	graph := importGraph(t, root)

	for _, rule := range importRules {
		if _, ok := graph[rule.pkg]; !ok {
			t.Fatalf("%s has no Go files; update importRules if the package moved", rule.pkg)
		}
		for _, bad := range rule.forbidden {
			if chain := importChain(graph, rule.pkg, bad); chain != nil {
				first := graph[rule.pkg][chain[1]]
				t.Errorf("%s: %s reaches %s through %s; %s",
					first, rule.pkg, bad, strings.Join(chain, " -> "), rule.instead)
			}
		}
	}
}

// importGraph maps each package in the module, by its path relative to the
// module root, to the module packages its non-test files import, each with the
// file and line of one import that brings it in. Test files are left out: a
// test may import whatever it needs to exercise a package, and the boundaries
// are about what ships.
func importGraph(t *testing.T, root string) map[string]map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	graph := map[string]map[string]string{}
	for _, rel := range goFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if graph[pkg] == nil {
			graph[pkg] = map[string]string{}
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, imp := range file.Imports {
			target, _ := strconv.Unquote(imp.Path.Value)
			dep, ok := strings.CutPrefix(target, module+"/")
			if !ok {
				continue
			}
			if _, seen := graph[pkg][dep]; !seen {
				graph[pkg][dep] = fmt.Sprintf("%s:%d", rel, fset.Position(imp.Pos()).Line)
			}
		}
	}
	return graph
}

// importChain returns the shortest chain of imports from one package to
// another, both included, or nil when there is none. Shortest, so the message
// names the most direct route to cut.
func importChain(graph map[string]map[string]string, from, to string) []string {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		deps := make([]string, 0, len(graph[pkg]))
		for dep := range graph[pkg] {
			deps = append(deps, dep)
		}
		sort.Strings(deps)
		for _, dep := range deps {
			if _, seen := prev[dep]; seen {
				continue
			}
			prev[dep] = pkg
			if dep == to {
				chain := []string{to}
				for p := pkg; p != ""; p = prev[p] {
					chain = append([]string{p}, chain...)
				}
				return chain
			}
			queue = append(queue, dep)
		}
	}
	return nil
}

// maxLines is the most a Go file may hold. Files already past it are held at
// their current length so they can shrink but not grow; splitting them is
// tracked in docs/plans/tech-debt.md. Lower an entry when a file shrinks.
const maxLines = 800

var grandfathered = map[string]int{}

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
