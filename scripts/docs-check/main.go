// Command docs-check fails when the repository's documents point at things
// that do not exist, so a rename or a deleted file cannot leave the docs quietly
// wrong. Run it with `make docs-check`.
//
// It checks tracked files only:
//
//   - a relative Markdown link must resolve to a file or directory;
//   - a backticked path under a top-level directory the repository has (for
//     example `internal/tui/palette.go`) must exist;
//   - a `make <target>` in inline code or a code block must be a real target;
//   - a document named as NAME.md, in Markdown or in a Go comment, must be a
//     tracked file;
//   - once docs/index.md exists, every tracked Markdown file must be listed in
//     it with a "Reviewed: YYYY-MM-DD read|edited" note no older than 90 days.
//
// Every failure says what to do next.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	indexPath    = "docs/index.md"
	reviewWindow = 90 * 24 * time.Hour
)

var (
	mdLink      = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	inlineCode  = regexp.MustCompile("`([^`\n]+)`")
	makeCall    = regexp.MustCompile(`(?:^|[\s;&|(])make\s+([a-z][a-z0-9_-]*)`)
	docName     = regexp.MustCompile(`\b([A-Za-z0-9_-]+\.md)\b`)
	reviewedRow = regexp.MustCompile(`Reviewed:\s*(\d{4}-\d{2}-\d{2})\s+(read|edited)`)
	makeTarget  = regexp.MustCompile(`^([a-z][a-z0-9_-]*)\s*:`)
	url         = regexp.MustCompile(`\S+://\S+`)
)

// self is this program's source. It names documents as examples of what it
// checks, so the document-name rule skips it.
const self = "scripts/docs-check/main.go"

type problem struct{ where, msg string }

func main() {
	now := time.Now()
	if v := os.Getenv("DOCS_CHECK_NOW"); v != "" { // for testing the review window
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "DOCS_CHECK_NOW must be YYYY-MM-DD")
			os.Exit(2)
		}
		now = t
	}
	problems, err := check(".", now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docs-check:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Printf("%s: %s\n", p.where, p.msg)
	}
	if len(problems) > 0 {
		fmt.Printf("docs-check: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Println("docs-check: ok")
}

func check(root string, now time.Time) ([]problem, error) {
	files, err := trackedFiles(root)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	dirs := map[string]bool{}
	baseNames := map[string]bool{}
	topLevel := map[string]bool{}
	var mdFiles, goFiles []string
	for _, f := range files {
		tracked[f] = true
		baseNames[path.Base(f)] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
		if i := strings.Index(f, "/"); i > 0 {
			topLevel[f[:i]] = true
		}
		switch {
		case strings.HasSuffix(f, ".md"):
			mdFiles = append(mdFiles, f)
		case strings.HasSuffix(f, ".go"):
			goFiles = append(goFiles, f)
		}
	}
	exists := func(p string) bool { return tracked[p] || dirs[strings.TrimSuffix(p, "/")] }

	targets, err := makeTargets(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}

	var out []problem
	add := func(where, format string, args ...any) {
		out = append(out, problem{where, fmt.Sprintf(format, args...)})
	}

	for _, doc := range mdFiles {
		lines, err := readLines(filepath.Join(root, doc))
		if err != nil {
			return nil, err
		}
		inFence := false
		for i, line := range lines {
			where := fmt.Sprintf("%s:%d", doc, i+1)
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}

			// make targets: in code blocks, and in inline code.
			var commands []string
			if inFence {
				commands = append(commands, line)
			} else {
				for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
					commands = append(commands, m[1])
				}
			}
			for _, c := range commands {
				for _, m := range makeCall.FindAllStringSubmatch(c, -1) {
					if !targets[m[1]] {
						add(where, "`make %s` is not a target in the Makefile; fix the command or add the target", m[1])
					}
				}
			}
			if inFence {
				continue
			}

			for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				target, _, _ = strings.Cut(target, "#")
				resolved := path.Clean(path.Join(path.Dir(doc), target))
				if !exists(resolved) {
					add(where, "link to %q resolves to %s, which is not a tracked file; fix the link or restore the file", m[1], resolved)
				}
			}

			for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
				p := strings.TrimSpace(m[1])
				if !looksLikeRepoPath(p, topLevel) {
					continue
				}
				p = strings.TrimSuffix(p, "/")
				if !exists(p) {
					add(where, "`%s` does not exist in the repository; update the path or remove the reference", p)
				}
			}

			checkDocNames(line, where, baseNames, add)
		}
	}

	for _, src := range goFiles {
		if src == self {
			continue
		}
		lines, err := readLines(filepath.Join(root, src))
		if err != nil {
			return nil, err
		}
		for i, line := range lines {
			_, comment, ok := strings.Cut(line, "//")
			if !ok {
				continue
			}
			checkDocNames(comment, fmt.Sprintf("%s:%d", src, i+1), baseNames, add)
		}
	}

	if tracked[indexPath] {
		out = append(out, checkIndex(root, mdFiles, now)...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].where < out[j].where })
	return out, nil
}

// looksLikeRepoPath accepts only tokens whose first segment is one of the
// repository's own top-level directories, so commands, flags, home paths and
// placeholders are left alone.
func looksLikeRepoPath(p string, topLevel map[string]bool) bool {
	if strings.ContainsAny(p, " <>*{}$=~:") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") && !strings.HasPrefix(p, ".git") {
		return false
	}
	first, _, found := strings.Cut(p, "/")
	return found && topLevel[first]
}

func checkDocNames(text, where string, baseNames map[string]bool, add func(string, string, ...any)) {
	for _, m := range docName.FindAllStringSubmatch(url.ReplaceAllString(text, ""), -1) {
		name := m[1]
		if !baseNames[name] {
			add(where, "%s is named here but no tracked file is called that; update the reference or restore the document", name)
		}
	}
}

func checkIndex(root string, mdFiles []string, now time.Time) []problem {
	var out []problem
	lines, err := readLines(filepath.Join(root, indexPath))
	if err != nil {
		return []problem{{indexPath, err.Error()}}
	}
	listed := map[string]bool{}
	for i, line := range lines {
		// Only table rows list documents; a link in the index's own prose
		// is not an entry and needs no review date.
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
			target, _, _ := strings.Cut(m[1], "#")
			if strings.Contains(target, "://") || !strings.HasSuffix(target, ".md") {
				continue
			}
			doc := path.Clean(path.Join(path.Dir(indexPath), target))
			listed[doc] = true
			where := fmt.Sprintf("%s:%d", indexPath, i+1)
			r := reviewedRow.FindStringSubmatch(line)
			if r == nil {
				out = append(out, problem{where, fmt.Sprintf("%s has no \"Reviewed: YYYY-MM-DD read|edited\" note; add one (edited = last commit date, read = checked against the code)", doc)})
				continue
			}
			when, err := time.Parse("2006-01-02", r[1])
			if err != nil {
				out = append(out, problem{where, fmt.Sprintf("%s: bad Reviewed date %q", doc, r[1])})
				continue
			}
			if now.Sub(when) > reviewWindow {
				out = append(out, problem{where, fmt.Sprintf("%s was last reviewed %s, more than 90 days ago; check it against the code and update the date (see docs/agents/doc-gardener.md)", doc, r[1])})
			}
		}
	}
	for _, doc := range mdFiles {
		if doc != indexPath && !listed[doc] {
			out = append(out, problem{indexPath, fmt.Sprintf("%s is tracked but not listed; add a row saying what it decides and when it was reviewed", doc)})
		}
	}
	return out
}

func trackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func makeTargets(makefile string) (map[string]bool, error) {
	lines, err := readLines(makefile)
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, l := range lines {
		if m := makeTarget.FindStringSubmatch(l); m != nil && !strings.Contains(l, ":=") {
			targets[m[1]] = true
		}
	}
	return targets, nil
}

func readLines(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines, s.Err()
}
