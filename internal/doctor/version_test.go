package doctor

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Version
		ok   bool
	}{
		{name: "sw_vers full", raw: "26.3.1\n", want: Version{26, 3, 1}, ok: true},
		{name: "sw_vers major minor", raw: "26.3", want: Version{26, 3, 0}, ok: true},
		{name: "major only", raw: "27", want: Version{27, 0, 0}, ok: true},
		{
			name: "container cli banner",
			raw:  "container CLI version 1.3.0 (build: release, commit: d6de569)",
			want: Version{1, 3, 0},
			ok:   true,
		},
		{
			name: "container cli newer",
			raw:  "container CLI version 2.0.1 (build: release, commit: abc1234)",
			want: Version{2, 0, 1},
			ok:   true,
		},
		{name: "no digits", raw: "command not found", want: Version{}, ok: false},
		{name: "empty", raw: "", want: Version{}, ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseVersion(tc.raw)
			if ok != tc.ok {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("ParseVersion(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		name string
		got  Version
		min  Version
		want bool
	}{
		{name: "equal", got: Version{1, 3, 0}, min: Version{1, 3, 0}, want: true},
		{name: "newer patch", got: Version{1, 3, 4}, min: Version{1, 3, 0}, want: true},
		{name: "newer minor", got: Version{1, 4, 0}, min: Version{1, 3, 9}, want: true},
		{name: "newer major", got: Version{2, 0, 0}, min: Version{1, 3, 0}, want: true},
		{name: "older patch", got: Version{1, 2, 9}, min: Version{1, 3, 0}, want: false},
		{name: "older major", got: Version{0, 9, 9}, min: Version{1, 0, 0}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got.AtLeast(tc.min); got != tc.want {
				t.Fatalf("%v.AtLeast(%v) = %v, want %v", tc.got, tc.min, got, tc.want)
			}
		})
	}
}

// A newer host must pass. The previous checks matched a "26." prefix and a
// literal "1.3.0" substring, so every future release failed doctor.
func TestMinimumsAcceptNewerReleases(t *testing.T) {
	if !meetsMinimum("27.0", MinimumMacOS) {
		t.Error("macOS 27.0 must satisfy the minimum")
	}
	if !meetsMinimum("26.3.1", MinimumMacOS) {
		t.Error("macOS 26.3.1 must satisfy the minimum")
	}
	if meetsMinimum("25.6", MinimumMacOS) {
		t.Error("macOS 25.6 must not satisfy the minimum")
	}
	if !meetsMinimum("container CLI version 1.4.0 (build: release)", MinimumContainerVersion) {
		t.Error("container 1.4.0 must satisfy the minimum")
	}
	if !meetsMinimum("container CLI version 2.0.0 (build: release)", MinimumContainerVersion) {
		t.Error("container 2.0.0 must satisfy the minimum")
	}
	if meetsMinimum("container CLI version 1.2.9 (build: release)", MinimumContainerVersion) {
		t.Error("container 1.2.9 must not satisfy the minimum")
	}
	if meetsMinimum("", MinimumContainerVersion) {
		t.Error("unparseable output must not satisfy the minimum")
	}
}
