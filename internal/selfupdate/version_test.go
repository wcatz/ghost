package selfupdate

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		want    int
		wantErr bool
	}{
		{name: "identical", a: "0.32.0", b: "0.32.0", want: 0},
		{name: "v prefix is not significant", a: "v0.32.0", b: "0.32.0", want: 0},
		{name: "build metadata is not significant", a: "0.32.0+dirty", b: "v0.32.0", want: 0},
		{name: "older patch", a: "0.31.9", b: "0.32.0", want: -1},
		{name: "newer patch", a: "0.32.0", b: "0.31.9", want: 1},
		// Lexicographic comparison gets both of these backwards, which is how a
		// guard built on string ordering would happily "upgrade" 0.9.0 to
		// 0.10.0 backwards.
		{name: "minor compares numerically", a: "0.9.0", b: "0.10.0", want: -1},
		{name: "major compares numerically", a: "9.0.0", b: "10.0.0", want: -1},
		{name: "older minor", a: "1.9.9", b: "1.10.0", want: -1},
		{name: "older major", a: "0.99.99", b: "1.0.0", want: -1},
		{name: "newer major", a: "1.0.0", b: "0.99.99", want: 1},
		{name: "prerelease sorts before its release", a: "1.0.0-rc.1", b: "1.0.0", want: -1},
		{name: "release sorts after its prerelease", a: "1.0.0", b: "1.0.0-rc.1", want: 1},
		{name: "prerelease numbers compare numerically", a: "1.0.0-rc.2", b: "1.0.0-rc.10", want: -1},
		{name: "prerelease words compare alphabetically", a: "1.0.0-alpha", b: "1.0.0-beta", want: -1},
		{name: "fewer prerelease identifiers sort first", a: "1.0.0-alpha", b: "1.0.0-alpha.1", want: -1},
		{name: "numeric prerelease identifier sorts below a word", a: "1.0.0-1", b: "1.0.0-alpha", want: -1},
		{name: "dev running version is unparseable", a: "0.32.0", b: "dev", wantErr: true},
		{name: "empty running version is unparseable", a: "0.32.0", b: "", wantErr: true},
		{name: "missing patch is unparseable", a: "0.32", b: "0.32.0", wantErr: true},
		{name: "extra component is unparseable", a: "0.32.0.1", b: "0.32.0", wantErr: true},
		{name: "non-numeric component is unparseable", a: "0.x.0", b: "0.32.0", wantErr: true},
		{name: "negative component is unparseable", a: "0.-1.0", b: "0.32.0", wantErr: true},
		{name: "nightly tag is unparseable", a: "nightly", b: "0.32.0", wantErr: true},
		{name: "surrounding space is unparseable", a: " 0.32.0", b: "0.32.0", wantErr: true},
		// v0.3.0-working is a tag this repository has really used, so a
		// word-shaped prerelease has to parse rather than look like garbage.
		{name: "a word prerelease sorts before its release", a: "0.3.0-working", b: "0.3.0", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareVersions(tt.a, tt.b)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CompareVersions(%q, %q) = %d, want an error", tt.a, tt.b, got)
				}
				// The guard's only way to explain a refusal to a user is a
				// version string it could not read, so the error has to name
				// one of the two it was given.
				if !strings.Contains(err.Error(), tt.a) && !strings.Contains(err.Error(), tt.b) {
					t.Errorf("error %q should name one of the versions it was given (%q, %q)", err, tt.a, tt.b)
				}
				return
			}
			if err != nil {
				t.Fatalf("CompareVersions(%q, %q): %v", tt.a, tt.b, err)
			}
			if got != tt.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCompareVersionsIsAntisymmetric(t *testing.T) {
	// A guard that is not antisymmetric inverts somewhere: a comparison that
	// answers "the release is newer" for the pair (a, b) and also for (b, a)
	// is the string-equality bug this replaced.
	versions := []string{"0.9.0", "0.10.0", "0.32.0", "0.32.1", "1.0.0-rc.1", "1.0.0", "1.0.0-rc.2"}
	for _, a := range versions {
		for _, b := range versions {
			ab, err := CompareVersions(a, b)
			if err != nil {
				t.Fatalf("CompareVersions(%q, %q): %v", a, b, err)
			}
			ba, err := CompareVersions(b, a)
			if err != nil {
				t.Fatalf("CompareVersions(%q, %q): %v", b, a, err)
			}
			if ab != -ba {
				t.Errorf("CompareVersions(%q, %q) = %d but CompareVersions(%q, %q) = %d", a, b, ab, b, a, ba)
			}
		}
	}
}

// TestIsPrerelease is the fact the pre-release guard refuses on, and it is not
// the same question as CompareVersions: that one orders two versions, this one
// asks whether a single tag names a release that is still a candidate. An
// unorderable tag answers false in both directions — CompareVersions reports it
// as an error and IsPrerelease reports that nothing says it is one — so a
// developer build keeps upgrading and a malformed prerelease is not read as
// consent to install.
func TestIsPrerelease(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{name: "a final release is not a prerelease", tag: "0.35.0", want: false},
		{name: "a v-prefixed final release is not a prerelease", tag: "v0.35.0", want: false},
		{name: "build metadata does not make a prerelease", tag: "v0.35.0+dirty", want: false},
		{name: "an rc is a prerelease", tag: "0.35.0-rc.1", want: true},
		{name: "a v-prefixed rc is a prerelease", tag: "v0.35.0-rc.1", want: true},
		{name: "a beta is a prerelease", tag: "v0.36.0-beta", want: true},
		{name: "a dotted alpha is a prerelease", tag: "1.0.0-alpha.2", want: true},
		{name: "a dev build is not a prerelease", tag: "dev", want: false},
		{name: "a non-semver tag is not a prerelease", tag: "nightly", want: false},
		{name: "an empty tag is not a prerelease", tag: "", want: false},
		// Unorderable, not a prerelease: a bare hyphen is not a prerelease
		// identifier, so the version cannot be read at all.
		{name: "an empty prerelease is not a prerelease", tag: "1.0.0-", want: false},
		{name: "a two-component version is not a prerelease", tag: "1.0-rc.1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPrerelease(tt.tag); got != tt.want {
				t.Errorf("IsPrerelease(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

// TestIsPrereleaseAgreesWithCompareVersions pins the two answers together. The
// guard in decideUpgrade refuses a release that is a prerelease and a release
// that is older, and it reports which one it refused, so the two questions must
// not be able to disagree about a version: a tag IsPrerelease calls one has to
// compare as newer than the final release it would replace, or the refusals
// would contradict each other.
func TestIsPrereleaseAgreesWithCompareVersions(t *testing.T) {
	for _, tag := range []string{
		"0.0.1", "0.9.0", "0.10.0", "0.35.0", "0.35.0-rc.1", "0.35.0-rc.2",
		"0.36.0-beta", "1.0.0-alpha.2", "v1.0.0-rc.1", "0.35.0+dirty",
	} {
		// A prerelease is always older than its own final release, never newer:
		// that is the semver rule, and the guard leans on it by refusing every
		// prerelease regardless of direction.
		if IsPrerelease(tag) {
			core := strings.TrimPrefix(strings.SplitN(tag, "-", 2)[0], "v")
			cmp, err := CompareVersions(tag, core)
			if err != nil {
				t.Fatalf("CompareVersions(%q, %q): %v", tag, core, err)
			}
			if cmp >= 0 {
				t.Errorf("IsPrerelease(%q) is true but %q does not rank below its final release %q (%d)",
					tag, tag, core, cmp)
			}
		}
	}
}
