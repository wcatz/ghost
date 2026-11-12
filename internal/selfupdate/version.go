package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// CompareVersions compares two semantic versions and returns -1 if a is older
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}

	for _, pair := range [][2]int{
		{pa.major, pb.major},
		{pa.minor, pb.minor},
		{pa.patch, pb.patch},
	} {
		if c := compareInt(pair[0], pair[1]); c != 0 {
			return c, nil
		}
	}
	return comparePrerelease(pa.prerelease, pb.prerelease), nil
}

// IsPrerelease reports whether a release tag names a prerelease — a candidate
func IsPrerelease(s string) bool {
	v, err := parseVersion(s)
	return err == nil && v.prerelease != ""
}

// IsRelease reports whether a version string names a published release: a
func IsRelease(s string) bool {
	v, err := parseVersion(s)
	return err == nil && v.prerelease == ""
}

type version struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

func parseVersion(s string) (version, error) {
	bad := func() (version, error) {
		return version{}, fmt.Errorf("cannot parse %q as a semantic version", s)
	}

	rest := strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		rest = rest[:i]
	}
	prerelease := ""
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		prerelease, rest = rest[i+1:], rest[:i]
		if !validPrerelease(prerelease) {
			return bad()
		}
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return bad()
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, ok := parseNumeric(p)
		if !ok {
			return bad()
		}
		nums[i] = n
	}
	return version{major: nums[0], minor: nums[1], patch: nums[2], prerelease: prerelease}, nil
}

func parseNumeric(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func validPrerelease(s string) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func comparePrerelease(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}

	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := comparePrereleaseIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return compareInt(len(as), len(bs))
}

func comparePrereleaseIdentifier(a, b string) int {
	an, aNum := parseNumeric(a)
	bn, bNum := parseNumeric(b)
	switch {
	case aNum && bNum:
		return compareInt(an, bn)
	case aNum:
		return -1 // numeric identifiers always have lower precedence
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
