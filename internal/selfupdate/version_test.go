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
		{name: "leading zero in a component is unparseable", a: "01.2.3", b: "0.32.0", wantErr: true},
		{name: "a zero-padded prerelease identifier is alphanumeric", a: "1.0.0-0", b: "1.0.0-00", want: -1},
		{name: "a zero component is a number", a: "0.0.0", b: "0.0.0", want: 0},
		{name: "zero prerelease identifier is numeric", a: "1.0.0-0", b: "1.0.0-alpha", want: -1},
		{name: "a word prerelease sorts before its release", a: "0.3.0-working", b: "0.3.0", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CompareVersions(tt.a, tt.b)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CompareVersions(%q, %q) = %d, want an error", tt.a, tt.b, got)
				}
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

func TestIsRelease(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{name: "a final release is a release", tag: "0.35.0", want: true},
		{name: "a v-prefixed final release is a release", tag: "v0.35.0", want: true},
		{name: "build metadata does not unmake a release", tag: "v0.35.0+dirty", want: true},
		{name: "a two-component version is not a release", tag: "1.0", want: false},
		{name: "a describe stamp is not a release", tag: "v0.38.0-14-gabc1234", want: false},
		// tag, with --long. The same tree without --long describes as the bare
		// tag, which this would accept -- a locally built, never-published binary
		{name: "a long describe stamp at a tag is not a release", tag: "v0.38.0-0-gabc1234", want: false},
		{name: "a dirty describe stamp is not a release", tag: "v0.38.0-0-gabc1234-dirty", want: false},
		{name: "an rc is not a release", tag: "0.39.0-rc.1", want: false},
		{name: "a dev build is not a release", tag: "dev", want: false},
		{name: "a non-semver tag is not a release", tag: "nightly", want: false},
		{name: "an empty tag is not a release", tag: "", want: false},
		{name: "an unparseable prerelease is not a release", tag: "1.0.0-", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRelease(tt.tag); got != tt.want {
				t.Errorf("IsRelease(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}
