package doctor

import (
	"strings"
	"testing"
)

func TestHooksPathResult(t *testing.T) {
	t.Parallel()

	const home = "/Users/tester"
	const saintRoot = "/Users/tester/.saint"

	tests := []struct {
		name        string
		value       string
		wantOK      bool
		wantDetail  string
		wantEscape  bool
		wantOverrid bool
	}{
		{
			name:       "unset is the good state",
			value:      "",
			wantOK:     true,
			wantDetail: "unset",
		},
		{
			name:        "set outside saint warns about override",
			value:       "/Users/tester/dotfiles/hooks",
			wantOK:      false,
			wantOverrid: true,
		},
		{
			name:       "absolute path inside saint is a sandbox-escape risk",
			value:      "/Users/tester/.saint/hooks",
			wantOK:     false,
			wantEscape: true,
		},
		{
			name:       "tilde path inside saint is a sandbox-escape risk",
			value:      "~/.saint/hooks",
			wantOK:     false,
			wantEscape: true,
		},
		{
			name:       "saint root itself is a sandbox-escape risk",
			value:      "/Users/tester/.saint",
			wantOK:     false,
			wantEscape: true,
		},
		{
			name:        "sibling directory is not inside saint",
			value:       "/Users/tester/.saint-other/hooks",
			wantOK:      false,
			wantOverrid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := hooksPathResult(tc.value, home, saintRoot)
			if got.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (detail %q)", got.OK, tc.wantOK, got.Detail)
			}
			if got.Fix != "git config --global --unset core.hooksPath" {
				t.Fatalf("Fix = %q", got.Fix)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("Detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			if tc.wantEscape && !strings.Contains(got.Detail, "sandbox-escape risk") {
				t.Fatalf("Detail = %q, want sandbox-escape warning", got.Detail)
			}
			if tc.wantOverrid {
				if !strings.Contains(got.Detail, "overrides every repo") {
					t.Fatalf("Detail = %q, want override warning", got.Detail)
				}
				if strings.Contains(got.Detail, "sandbox-escape risk") {
					t.Fatalf("Detail = %q, unexpected sandbox-escape warning", got.Detail)
				}
			}
		})
	}
}

func TestHooksPathResultNeverRecommendsSettingIt(t *testing.T) {
	t.Parallel()

	// Regression guard: saint must never tell a user to point global
	// core.hooksPath anywhere, least of all into ~/.saint.
	for _, value := range []string{"", "/somewhere/hooks", "~/.saint/hooks"} {
		got := hooksPathResult(value, "/Users/tester", "/Users/tester/.saint")
		if !strings.Contains(got.Fix, "--unset") {
			t.Fatalf("Fix = %q, want an --unset remediation", got.Fix)
		}
		if strings.Contains(got.Fix, ".saint") {
			t.Fatalf("Fix = %q, must not point core.hooksPath into ~/.saint", got.Fix)
		}
	}
}
