package store

import (
	"path/filepath"
	"testing"
)

// SkillPath must return the configured value trimmed, and empty for a name
// that was never registered - Create treats that as "refuse the session,"
// so the distinction between unset and blank has to survive this call.
func TestSkillPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		cfg    Config
		lookup string
		want   string
	}{
		{name: "configured", cfg: Config{Skills: map[string]string{"apple-design": "/skills/apple-design"}}, lookup: "apple-design", want: "/skills/apple-design"},
		{name: "whitespace trimmed", cfg: Config{Skills: map[string]string{"apple-design": "  /skills/apple-design  "}}, lookup: "apple-design", want: "/skills/apple-design"},
		{name: "not configured", cfg: Config{Skills: map[string]string{"apple-design": "/skills/apple-design"}}, lookup: "release-notes", want: ""},
		{name: "nil map", cfg: Config{}, lookup: "apple-design", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.SkillPath(tc.lookup); got != tc.want {
				t.Fatalf("SkillPath(%q) = %q, want %q", tc.lookup, got, tc.want)
			}
		})
	}
}

// DefaultSkillsOrEmpty must hand back a copy: a caller mutating the result
// must not be able to reach into the config's own slice.
func TestDefaultSkillsOrEmpty(t *testing.T) {
	t.Parallel()

	cfg := Config{DefaultSkills: []string{"apple-design", "release-notes"}}
	got := cfg.DefaultSkillsOrEmpty()
	if len(got) != 2 || got[0] != "apple-design" || got[1] != "release-notes" {
		t.Fatalf("DefaultSkillsOrEmpty() = %v, want [apple-design release-notes]", got)
	}

	got[0] = "tampered"
	if cfg.DefaultSkills[0] != "apple-design" {
		t.Fatalf("mutating the returned slice changed the config: %v", cfg.DefaultSkills)
	}
}

// A config with no DefaultSkills must return an empty result a caller can
// range over without a nil check.
func TestDefaultSkillsOrEmptyWhenUnset(t *testing.T) {
	t.Parallel()

	got := (Config{}).DefaultSkillsOrEmpty()
	if len(got) != 0 {
		t.Fatalf("DefaultSkillsOrEmpty() = %v, want empty", got)
	}
}

// SkillsRootOrDefault must fall back to ~/.saint/skills - the same directory
// EnsureConfig-adjacent code already treats as saint's own state - so an
// install that never set skills_root still has a single place skills can
// come from.
func TestSkillsRootOrDefault(t *testing.T) {
	t.Parallel()

	home := "/home/user"
	want := filepath.Join(home, ".saint", "skills")

	cases := []struct {
		name string
		root string
		want string
	}{
		{name: "unset", root: "", want: want},
		{name: "whitespace only", root: "   ", want: want},
		{name: "configured", root: "/custom/skills", want: "/custom/skills"},
		{name: "configured with whitespace", root: "  /custom/skills  ", want: "/custom/skills"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{SkillsRoot: tc.root}
			if got := cfg.SkillsRootOrDefault(home); got != tc.want {
				t.Fatalf("SkillsRootOrDefault(%q) = %q, want %q", home, got, tc.want)
			}
		})
	}
}
