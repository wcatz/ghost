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
