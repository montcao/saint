package doctor

import (
	"regexp"
	"strconv"
	"strings"
)

// Requirements are expressed as minimums, never as exact or prefix matches.
//
// These numbers are the oldest versions saint is known to work on, not the
// versions it was developed against: a user on a newer macOS or a newer
// container CLI must pass, not be told to downgrade. Matching on a "26." prefix
// or a literal "1.3.0" substring would fail every future release.
var (
	// MinimumMacOS is the oldest macOS that ships Apple's container runtime.
	MinimumMacOS = Version{26, 0, 0}

	// MinimumContainerVersion is the oldest container CLI whose JSON output
	// shape saint parses (`container ls -a --format json`).
	MinimumContainerVersion = Version{1, 3, 0}
)

// Version is a dotted numeric version, compared field by field. Any trailing
// build metadata or pre-release suffix is ignored - only the numbers order.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// AtLeast reports whether v is greater than or equal to min.
func (v Version) AtLeast(min Version) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	if v.Minor != min.Minor {
		return v.Minor > min.Minor
	}
	return v.Patch >= min.Patch
}

// versionPattern finds the first dotted numeric run in a string, so it works on
// a bare "26.3" from sw_vers and on the container CLI's chattier
// "container CLI version 1.3.0 (build: release, commit: ...)" alike, without
// hardcoding either output format.
var versionPattern = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// ParseVersion extracts the first version number found in raw. Missing minor or
// patch components are treated as zero, so "26" parses as 26.0.0.
func ParseVersion(raw string) (Version, bool) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return Version{}, false
	}
	return Version{
		Major: atoiOrZero(match[1]),
		Minor: atoiOrZero(match[2]),
		Patch: atoiOrZero(match[3]),
	}, true
}

// meetsMinimum reports whether raw parses to a version at or above min.
func meetsMinimum(raw string, min Version) bool {
	got, ok := ParseVersion(raw)
	return ok && got.AtLeast(min)
}

func atoiOrZero(value string) int {
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
