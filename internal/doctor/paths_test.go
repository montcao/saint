package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCheckout builds a directory tree that looks like a saint checkout.
func fakeCheckout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, runtime := range RuntimeImageNames {
		dir := filepath.Join(root, "images", runtime)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatalf("write Dockerfile: %v", err)
		}
	}
	return root
}

func TestResolveRepoRootFindsCheckoutFromNestedDir(t *testing.T) {
	root := fakeCheckout(t)
	nested := filepath.Join(root, "internal", "doctor")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := ResolveRepoRoot(nested)
	if got != root {
		t.Fatalf("ResolveRepoRoot(%q) = %q, want %q", nested, got, root)
	}
}

func TestResolveRepoRootReturnsEmptyOutsideCheckout(t *testing.T) {
	// A directory with no images/ anywhere above it must not resolve. Returning
	// the working directory here is what produced wrong `container build`
	// commands when doctor was run from an unrelated repo.
	outside := t.TempDir()
	if got := ResolveRepoRoot(outside); got != "" {
		t.Fatalf("ResolveRepoRoot(%q) = %q, want empty", outside, got)
	}
}

func TestResolveRepoRootPrefersFirstMatchingCandidate(t *testing.T) {
	first := fakeCheckout(t)
	second := fakeCheckout(t)

	if got := ResolveRepoRoot("", first, second); got != first {
		t.Fatalf("ResolveRepoRoot = %q, want %q", got, first)
	}
}

func TestResolveRepoRootRejectsPartialCheckout(t *testing.T) {
	root := t.TempDir()
	// Only one of the two runtime images is present.
	dir := filepath.Join(root, "images", RuntimeImageNames[0])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := ResolveRepoRoot(root); got != "" {
		t.Fatalf("ResolveRepoRoot(%q) = %q, want empty", root, got)
	}
}

func TestImageBuildContextUsesPlaceholderWhenRootUnknown(t *testing.T) {
	got := imageBuildContext("", "claude")
	if !strings.Contains(got, UnknownRepoRootPlaceholder) {
		t.Fatalf("imageBuildContext(\"\", claude) = %q, want it to contain %q", got, UnknownRepoRootPlaceholder)
	}

	got = imageBuildContext("/opt/saint", "codex")
	if want := filepath.Join("/opt/saint", "images", "codex"); got != want {
		t.Fatalf("imageBuildContext = %q, want %q", got, want)
	}
}

func TestBuildImagesRefusesUnknownRepoRoot(t *testing.T) {
	err := BuildImages(t.Context(), "/usr/bin/true", "")
	if err == nil {
		t.Fatal("BuildImages with an unknown repo root must return an error")
	}
	if !strings.Contains(err.Error(), "images/") {
		t.Fatalf("error should name what is missing, got %q", err)
	}
}
