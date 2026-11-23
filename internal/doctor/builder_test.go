package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuilderResultDistinguishesRunningStoppedAndUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "containers", "buildkit"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, response, want string
		ok                   bool
	}{
		{"running", "echo buildkit", "running; builder container disk usage", false},
		{"stopped", "exit 0", "no running builder", true},
		{"failed", "exit 1", "state unknown", false},
		{"unexpected response", "echo unexpected", "state unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "container")
			script := "#!/bin/sh\n[ \"$*\" = 'builder status --quiet' ] || exit 2\n" + tc.response + "\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			got := builderResult(t.Context(), bin, root)
			if got.OK != tc.ok || !strings.HasPrefix(got.Detail, tc.want) {
				t.Fatalf("builder result = %+v", got)
			}
			if tc.name == "running" && (!strings.HasSuffix(got.Detail, " KiB") || !strings.Contains(got.Fix, "when every build is finished")) {
				t.Fatalf("running builder result = %+v", got)
			}
		})
	}
	if got := builderDiskUsage(t.Context(), root+"/absent"); !strings.HasSuffix(got, "unknown") {
		t.Errorf("missing builder directory result = %q", got)
	}
}
