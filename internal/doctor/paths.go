package doctor

import (
	"os"
	"path/filepath"
)

// RuntimeImageNames are the runtime image directories saint builds from, in the
// order doctor reports them.
var RuntimeImageNames = []string{"claude", "codex"}

// UnknownRepoRootPlaceholder stands in for the checkout path in remediation
// commands when saint cannot locate its own source tree - the binary may be
// installed anywhere, and printing the user's current directory instead would
// hand them a command that silently builds the wrong thing.
const UnknownRepoRootPlaceholder = "<path-to-saint-checkout>"

// ResolveRepoRoot locates the saint checkout that owns the images/ directory.
//
// It deliberately does not assume the process was started from the repo root:
// `saint doctor` is expected to be run from whatever repo the user is working
// in, and an installed binary may live far from its source. Each candidate is
// walked upward until a directory containing every runtime image is found.
// Returns "" when the checkout cannot be located, which callers must handle
// rather than substituting the working directory.
func ResolveRepoRoot(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if root := walkUpForImages(candidate); root != "" {
			return root
		}
	}
	return ""
}

// DefaultRepoRootCandidates are the starting points ResolveRepoRoot searches:
// the working directory (covers running from inside the checkout) and the
// directory holding the running binary (covers `go build -o` output left beside
// the source).
func DefaultRepoRootCandidates() []string {
	candidates := make([]string, 0, 2)
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidates = append(candidates, filepath.Dir(exe))
	}
	return candidates
}

func walkUpForImages(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if hasRuntimeImages(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func hasRuntimeImages(dir string) bool {
	for _, name := range RuntimeImageNames {
		info, err := os.Stat(filepath.Join(dir, "images", name, "Dockerfile"))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// imageBuildContext renders the build-context path for a runtime image, falling
// back to a placeholder when the checkout was not found.
func imageBuildContext(repoRoot, runtime string) string {
	if repoRoot == "" {
		return filepath.Join(UnknownRepoRootPlaceholder, "images", runtime)
	}
	return filepath.Join(repoRoot, "images", runtime)
}
