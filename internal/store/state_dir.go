package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// StateDirEnv names the environment variable that moves saint's state
// directory away from ~/.saint, so a second copy of saint (another worktree, a
// development build) keeps its own sessions, tokens, config and proxy port.
// Child processes saint starts, such as the detached proxy, inherit it.
const StateDirEnv = "SAINT_HOME"

// NewFromEnv is New with the state directory taken from SAINT_HOME when it is
// set. The value must be absolute: a relative one would resolve against
// whatever directory each process happened to start in, and the pane and the
// proxy it starts would disagree about where the state is.
func NewFromEnv(home string) (*Store, error) {
	root := os.Getenv(StateDirEnv)
	if root == "" {
		return New(home), nil
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%s must be an absolute path, got %q", StateDirEnv, root)
	}
	return &Store{paths: pathsAt(home, filepath.Clean(root))}, nil
}
