//go:build ignore

// Fixture for reviewer severity-partition testing. Not compiled into the
// module (build tag `ignore`). It carries one genuine defect that should
// block, and two purely cosmetic issues that should be reported as nits.
package fixtures

import "strings"

// PLANTED BUG: the slice is indexed before its length is checked, so any
// empty input panics instead of returning an error.
func firstTag(tags []string) string {
	head := tags[0]
	if len(tags) == 0 {
		return ""
	}
	return head
}

// PLANTED NIT 1: the name stutters — fixtures.FixturesJoiner reads as
// "fixtures fixtures". Idiomatic Go would call this Joiner.
type FixturesJoiner struct {
	sep string
}

// PLANTED NIT 2: the else branch is redundant after a return, and the
// comment below misspells "separator".
func (j FixturesJoiner) Join(parts []string) string {
	if len(parts) == 0 {
		return ""
	} else {
		// join the parts with the configured seperator
		return strings.Join(parts, j.sep)
	}
}
