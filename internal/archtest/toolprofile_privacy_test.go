package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

// Experiment evidence was previously mixed into public documentation. Keep this
// area generic; adding another public artifact requires an explicit policy review.
func TestToolProfilePublicDocumentsExcludeEvidence(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "docs/research/tool-profile").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range strings.Fields(string(out)) {
		if path != "docs/research/tool-profile/README.md" {
			t.Errorf("%s is not an approved generic document; keep private evidence outside the checkout", path)
		}
	}
}
