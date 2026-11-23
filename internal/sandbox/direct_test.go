package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestDirectPaneUsesTheRuntimeTerminal(t *testing.T) {
	t.Parallel()
	argv := createArgv(t, CreateOptions{Direct: true, Network: "saint-abc123"})
	if argv[0] != "create" || slices.Contains(argv, "--detach") || slices.Contains(argv, "-lc") {
		t.Fatalf("direct create arguments: %v", argv)
	}
	for _, want := range []string{"--init", "--tty", "--interactive", "TERM=xterm-256color", "LANG=C.UTF-8", "HOME=/home/agent", "-c"} {
		if !slices.Contains(argv, want) {
			t.Errorf("direct create missing %s", want)
		}
	}
	script := strings.Join(argv[slices.Index(argv, "-c")+1:], "\n")
	if strings.Contains(script, "tmux") || !strings.Contains(script, "/saint/"+DirectProxyReadyFile) || !strings.Contains(script, "exec 'claude'") {
		t.Fatalf("unexpected direct bootstrap: %s", script)
	}
	bin, _ := fakeContainer(t)
	sb := NewAppleContainer(bin, nil)
	cmd, err := sb.AttachCmd("saint-abc123", AttachOptions{DirectContext: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{bin, "start", "--attach", "--interactive", "saint-abc123"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("direct attach = %v, want %v", cmd.Args, want)
	}
}
