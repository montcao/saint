package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReportsSharedBuilderOnSuccessAndFailure(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "container")
		script := "#!/bin/sh\n[ \"$1\" = build ] || exit 42\necho build-output\n"
		if fail {
			script += "exit 1\n"
		}
		if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := BuildRuntimeImage(t.Context(), bin, dir, "claude", &out)
		if (err != nil) != fail {
			t.Fatalf("error=%v fail=%t", err, fail)
		}
		if !strings.Contains(out.String(), "build-output") || !strings.Contains(out.String(), "Once every build is finished") {
			t.Fatalf("missing guidance: %s", &out)
		}
	}
}
