package mcpinit

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/assemble"
)

// labelLength is the measure the search is exercised with: the length of the
// label the value renders to, which is the length the alert's budget spends.
func labelLength(s string) int { return len(assemble.Label(s)) }

// longestFittingPrefix is the answer markerPrefixWithin is expected to give:
// the longest prefix that ends on a rune boundary and measures at most room.
// It is written the slow way — every boundary, one at a time — because it is
// the brute force the fast search is measured against.
func longestFittingPrefix(s string, measure func(string) int, room int) int {
	best := 0
	for l := 0; l <= len(s); l++ {
		// A prefix must end on a rune boundary: the byte at l starts a
		// character only when it is not a continuation byte.
		if l > 0 && l < len(s) && !utf8.RuneStart(s[l]) {
			continue
		}
		if measure(s[:l]) <= room {
			best = l
		}
	}
	return best
}

// TestMarkerPrefixWithinFindsTheLongestFit pins the search the alert's three
// cuts share: it must return the LONGEST prefix that ends on a rune boundary
// and measures at most the room.
//
// The search walks bytes, so a candidate in the middle of a multi-byte
// character has to be moved to a boundary. Moving it down can land at or
// below the lower bound, and stopping there loses the whole rest of the
// value: an emoji at the front puts every byte after it out of reach, so an
// error led by one dropped to an empty field, and a project led by one was
// rendered as an ellipsis although several bytes of it fitted.
func TestMarkerPrefixWithinFindsTheLongestFit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		room  int
	}{
		{"an emoji then two backticks", "\U0001f600``", 26},
		{"a cjk led error", "错误：模型拒绝了这次合并", 20},
		{"an accented project", "café-projet-é", 12},
		{"invalid bytes", "a\xffb\xffc", 3},
		{"a control rune", "pro\x1b`name`", 8},
		{"a bidi control", "pro\u202ename", 10},
		{"a lone quote", `pro"ject`, 6},
		{"plain ascii", "my-project", 5},
		{"a value that fits whole", "short", 40},
		{"a value that fits nothing", "\U0001f600\U0001f601", 2},
	} {
		got := markerPrefixWithin(tc.value, labelLength, tc.room)
		want := longestFittingPrefix(tc.value, labelLength, tc.room)
		if got != want {
			t.Errorf("%s: the search returned %d bytes, want the longest fit %d (room %d, value %q)",
				tc.name, got, want, tc.room, tc.value)
		}
	}
}

// TestMarkerPrefixWithinMatchesTheLongestFitOverRandomValues is the same
// comparison over values the generator builds out of the characters the
// renderers treat differently: ASCII, accented letters, CJK, an emoji, a
// control rune, a backtick, a quote, a bidirectional control and an invalid
// byte. The seed is fixed, so the sweep is the same every run.
//
// The property under test is that the search never returns MORE than the
// room allows and never returns LESS than the longest fit — the first would
// put an over-long value on the line, the second would drop value the
// budget had room for. It runs against all three measures the alert's cuts
// use, because each of them escapes differently.
func TestMarkerPrefixWithinMatchesTheLongestFitOverRandomValues(t *testing.T) {
	pieces := []string{
		"a", "Z", "9", "-", "_", ".", "/",
		"é", "ü", "ß", "中", "文", "\U0001f600", "\U0001f680",
		"\x1b", "\x00", "`", `"`, "\u202e", "\u202d", "\xff", "\xc3",
	}
	measures := map[string]func(string) int{
		"label":  labelLength,
		"token":  func(s string) int { return len(assemble.Token(s)) },
		"quoted": func(s string) int { return len(assemble.Data(s)) },
	}
	rnd := rand.New(rand.NewSource(1)) //nolint:gosec // a fixed seed: the sweep must be the same every run
	for range 1500 {
		var value strings.Builder
		for range rnd.Intn(12) + 1 {
			value.WriteString(pieces[rnd.Intn(len(pieces))])
		}
		s := value.String()
		for name, measure := range measures {
			for room := range 61 {
				got := markerPrefixWithin(s, measure, room)
				want := longestFittingPrefix(s, measure, room)
				if got != want {
					t.Fatalf("the %s measure: value %q at room %d: the search returned %d bytes, want the longest fit %d",
						name, s, room, got, want)
				}
			}
		}
	}
}
