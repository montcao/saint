package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRuntimeDockerfilesUseTheirArtifactPins(t *testing.T) {
	root := repoRoot(t)
	pin := regexp.MustCompile(`\Aversion=[0-9]+\.[0-9]+\.[0-9]+\nsha_arm64=[0-9a-f]{64}\nsha_amd64=[0-9a-f]{64}\n\z`)
	for runtime, artifact := range map[string]string{
		"claude": `claude-code-linux-${arch}-${version}.tgz`,
		"codex":  `codex-${version}-linux-${arch}.tgz`,
	} {
		t.Run(runtime, func(t *testing.T) {
			dir := filepath.Join(root, "images", runtime)
			manifest, err := os.ReadFile(filepath.Join(dir, "runtime.lock"))
			if err != nil {
				t.Fatal(err)
			}
			if !pin.Match(manifest) {
				t.Fatal("runtime.lock must pin the version and both architecture checksums")
			}
			dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"COPY runtime.lock /tmp/runtime.lock", "&& . /tmp/runtime.lock",
				"arm64) arch=arm64;", "amd64) arch=x64;", "sha=$sha_arm64", "sha=$sha_amd64",
				artifact, `"$sha" | sha256sum -c -`,
			} {
				if !strings.Contains(string(dockerfile), want) {
					t.Errorf("Dockerfile no longer consumes runtime.lock: missing %q", want)
				}
			}
			final := strings.Split(string(dockerfile), "\nFROM ")[1]
			for _, tool := range []string{"ca-certificates", "curl", "git", "openssh-client", "ripgrep", "less", "python3", "tmux"} {
				if !strings.Contains(final, tool) {
					t.Errorf("final stage missing %s", tool)
				}
			}

		})
	}
}
