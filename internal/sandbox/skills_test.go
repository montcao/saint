package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestValidateSkillName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "simple", value: "review-checklist"},
		{name: "digits first", value: "2fa-notes"},
		{name: "underscored", value: "code_review"},
		{name: "single char", value: "a"},
		{name: "max length", value: strings.Repeat("a", 64)},
		{name: "empty", value: "", wantErr: true},
		{name: "leading hyphen", value: "-review", wantErr: true},
		{name: "leading underscore", value: "_review", wantErr: true},
		{name: "uppercase", value: "Review", wantErr: true},
		{name: "path traversal", value: "../../etc", wantErr: true},
		{name: "absolute path", value: "/etc/passwd", wantErr: true},
		{name: "space", value: "review checklist", wantErr: true},
		{name: "newline", value: "review\n--force", wantErr: true},
		{name: "too long", value: strings.Repeat("a", 65), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSkillName(tc.value)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateSkillName(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
		})
	}
}

// TestCreateRejectsUnvalidatedSkillNames pins the defensive check that sits
// immediately before exec, so a hostile store.Config entry can never reach
// the container CLI even if the resolve-time filter in internal/session is
// bypassed.
func TestCreateRejectsUnvalidatedSkillNames(t *testing.T) {
	t.Parallel()

	a := NewAppleContainer("/nonexistent/container", nil)
	ctx := context.Background()

	opts := CreateOptions{
		SkillMounts: []SkillMount{{Name: "../../etc", Path: "/tmp/skill"}},
	}
	err := a.Create(ctx, "saint-abc123xy", RuntimeClaude, []Repo{{Path: "/tmp/repo"}}, "/tmp/session", "", opts)
	if err == nil {
		t.Fatal("error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "invalid skill name") {
		t.Fatalf("error = %v, want invalid skill name", err)
	}
}
