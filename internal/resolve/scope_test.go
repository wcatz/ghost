package resolve

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// ids are 32 lowercase hex characters, the shape memory.Store's id column
// produces, so a prefix here is the same kind of thing an operator would paste
// from a report.
const (
	scopedA = "aaaaaaaabbbbccccddddeeeeffff0000"
	scopedB = "11111111222233334444555566667777"
	scopedC = "99999999888877776666555544443333"
	scopedD = "deadbeefcafebabe0123456789abcdef"
)

func scopedPool() []memory.Memory {
	return []memory.Memory{
		{ID: scopedA, Content: "note a"},
		{ID: scopedB, Content: "note b"},
		{ID: scopedC, Content: "note c"},
	}
}

func scopedIDs(ms []memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func eqIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestScopeSelectsOnlyNamedMemories: --only is how a repair is pointed at the
// rows a supersede withdrawal actually touched. It must judge those rows and
// leave every other already-resolved row alone — the whole point of the flag is
// that an unscoped pass un-hides memories that were resolved for good reasons.
func TestScopeSelectsOnlyNamedMemories(t *testing.T) {
	for _, tc := range []struct {
		name string
		only []string
		want []string
	}{
		{"exact id", []string{scopedB}, []string{scopedB}},
		{"eight char prefix", []string{scopedC[:8]}, []string{scopedC}},
		{"two selectors", []string{scopedB, scopedC[:8]}, []string{scopedB, scopedC}},
		{"uppercase hex names the same row", []string{strings.ToUpper(scopedA[:8])}, []string{scopedA}},
		// Pool order, not selector order: the report has to read the same way
		// every run, and the store's own order is what the unscoped pass uses.
		{"selectors out of pool order", []string{scopedC, scopedA[:9]}, []string{scopedA, scopedC}},
		{"a repeated selector judges one row once", []string{scopedB, scopedB}, []string{scopedB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, misses, err := Scope{Only: tc.only}.Select(scopedPool())
			if err != nil {
				t.Fatalf("Select(%v): %v", tc.only, err)
			}
			if len(misses) != 0 {
				t.Errorf("misses = %v, want none", misses)
			}
			if ids := scopedIDs(got); !eqIDs(ids, tc.want) {
				t.Errorf("Select(%v) = %v, want %v", tc.only, ids, tc.want)
			}
		})
	}
}

// TestEmptyScopeSelectsEverything: the zero Scope is the unscoped pass, so it
// returns the pool untouched and never reports a miss.
func TestEmptyScopeSelectsEverything(t *testing.T) {
	pool := scopedPool()
	got, misses, err := Scope{}.Select(pool)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(misses) != 0 {
		t.Errorf("misses = %v, want none", misses)
	}
	if ids := scopedIDs(got); !eqIDs(ids, scopedIDs(pool)) {
		t.Errorf("Select() = %v, want the whole pool %v", ids, scopedIDs(pool))
	}
}

// TestScopeMissIsReportedNotFatal: an id that is not resolved, or not in this
// project, is something the operator typed wrong or something resolve has
// already cleared — not a reason to refuse the run. The rest of the scope is
// still judged, and the miss comes back so the report can name it (#698).
func TestScopeMissIsReportedNotFatal(t *testing.T) {
	got, misses, err := Scope{Only: []string{scopedB, "ffffffffffffffffffffffffffffffff"}}.Select(scopedPool())
	if err != nil {
		t.Fatalf("a selector that matches nothing must not fail the run: %v", err)
	}
	if ids := scopedIDs(got); !eqIDs(ids, []string{scopedB}) {
		t.Errorf("Select() = %v, want the one row that exists", ids)
	}
	if len(misses) != 1 {
		t.Fatalf("misses = %v, want exactly the unmatched selector", misses)
	}
	if misses[0].Spec != "ffffffffffffffffffffffffffffffff" {
		t.Errorf("misses[0].Spec = %q, want the selector the operator typed", misses[0].Spec)
	}
	if !strings.Contains(misses[0].Reason, "no already-resolved memory") {
		t.Errorf("misses[0].Reason = %q, must say the row was not found rather than why not", misses[0].Reason)
	}
}

// TestScopeAmbiguousPrefixIsAnError: the two halves of a selector have opposite
// failure modes. A prefix that matches nothing is a typo the operator can see in
// the report; a prefix that matches two rows is not a typo the pass can resolve
// on its own, and silently judging one of them (or both) is exactly the
// un-hiding this flag exists to prevent — so it stops the run.
func TestScopeAmbiguousPrefixIsAnError(t *testing.T) {
	pool := []memory.Memory{
		{ID: "abcdef00111111111111111111111111", Content: "one"},
		{ID: "abcdef00222222222222222222222222", Content: "two"},
	}
	got, _, err := (Scope{Only: []string{"abcdef00"}}).Select(pool)
	if err == nil {
		t.Fatal("Select: an ambiguous prefix must be an error")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "abcdef00") {
		t.Errorf("error %q must name the ambiguous selector and say so", err)
	}
	if got != nil {
		t.Errorf("Select returned %v alongside the error, want nothing judged", scopedIDs(got))
	}
}

// TestScopeRejectsMalformedSelectors: below the prefix floor a selector cannot
// be a deliberate choice — one hex character matches most of a project — so it is
// a usage error rather than a miss. So is a non-hex selector, which is either a
// truncated paste or a word; neither can name a row, and reporting it as a miss
// would bury the mistake in a list of skips.
func TestScopeRejectsMalformedSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		only string
		want string
	}{
		{"too short", "abcdef0", "8"},
		{"one character", "a", "8"},
		{"not hex", "zzzzzzzz", "hex"},
		{"a word", "resolved", "hex"},
		{"empty", "", "hex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := (Scope{Only: []string{tc.only}}).Select(scopedPool())
			if err == nil {
				t.Fatalf("Select(%q) must fail", tc.only)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must mention %q", err, tc.want)
			}
		})
	}
}

// TestScopeAcceptsEveryHexLengthAtOrAboveTheFloor: the floor is a length, not a
// spelling, so a 9- or 32-character selector is a prefix like any other.
func TestScopeAcceptsEveryHexLengthAtOrAboveTheFloor(t *testing.T) {
	for _, only := range []string{scopedB[:8], scopedB[:9], scopedB[:31], scopedB} {
		got, misses, err := (Scope{Only: []string{only}}).Select(scopedPool())
		if err != nil {
			t.Fatalf("Select(%q): %v", only, err)
		}
		if len(misses) != 0 || !eqIDs(scopedIDs(got), []string{scopedB}) {
			t.Errorf("Select(%q) = %v misses %v, want [%s]", only, scopedIDs(got), misses, scopedB)
		}
	}
}
