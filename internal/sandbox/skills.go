package sandbox

import (
	"fmt"
	"regexp"
)

// skillNamePattern mirrors containerNamePattern's shape: names reach the
// container CLI from sessions.json and store.Config, both of which a guest
// with a mounted repo can influence indirectly. There is no shell involved so
// this is not command injection, but an unvalidated name is still a way to
// smuggle flags into `container run --volume`. Validation lives at resolve
// time in internal/session and is repeated here, immediately before exec, so
// no future caller can skip it.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateSkillName rejects any skill name that was not already checked
// against store.Config.Skills or DefaultSkills.
func ValidateSkillName(name string) error {
	if !skillNamePattern.MatchString(name) {
		display := name
		if len(display) > 40 {
			display = display[:40] + "..."
		}
		return fmt.Errorf("invalid skill name %q", display)
	}
	return nil
}

// SkillMount is a single host skill directory bound into a guest, always
// read-only. Path is a host absolute path already resolved and checked
// against store.Config.SkillsRoot; Hash is the content hash taken at mount
// time, recorded on the session so drift can be detected later.
type SkillMount struct {
	Name string
	Path string
	Hash string
}
