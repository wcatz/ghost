package resolve

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// An imported artifact writes its ids verbatim — `ghost import` takes them from
// the file and ImportMemory refuses only an empty one — so a stored id is not
// necessarily 32 hex characters. The scope must accept one in full, because the
// supersede withdrawal that orphans such a memory prints
// `ghost resolve --only <that id>` as the only way to clear it, and a pass that
// refuses its own repair's selector is worse than no repair.
const (
	importedSpace = "imported note; rm -rf /"
	importedShort = "abc"
)

// recase uppercases the second eight characters of an id, so the result is
// neither the stored lowercase id nor its fully-uppercased twin: it is a
// spelling only a case-insensitive match can recognise. scopedB is all digits,
// so every casing case below uses scopedA.
func recase(id string) string { return id[:8] + strings.ToUpper(id[8:16]) + id[16:] }

// TestScopeAcceptsAFullIDWhateverItsShape: the two shape rules (hex alphabet,
// 8-character floor) are rules about a PREFIX. Applied to a full id they refused
// the rows this flag exists to name. A case-folded spelling resolves to the
// STORED id, because a selector names a row rather than a spelling.
func TestScopeAcceptsAFullIDWhateverItsShape(t *testing.T) {
	pool := []memory.Memory{
		{ID: importedSpace, Content: "an imported note whose id is not hex"},
		{ID: importedShort, Content: "an imported note with a three-character id"},
		{ID: scopedA, Content: "note a"},
	}
	for _, tc := range []struct{ spec, want string }{
		{spec: importedSpace, want: importedSpace},
		{spec: importedShort, want: importedShort},
		{spec: recase(scopedA), want: scopedA},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			got, misses, err := (Scope{Only: []string{tc.spec}}).Select(pool)
			if err != nil {
				t.Fatalf("Select(%q): %v", tc.spec, err)
			}
			if len(misses) != 0 || !eqIDs(scopedIDs(got), []string{tc.want}) {
				t.Errorf("Select(%q) = %v misses %v, want [%s]", tc.spec, scopedIDs(got), misses, tc.want)
			}
		})
	}
}

// The floor is still a floor for a PREFIX, and a short hex selector that is not
// a stored id is still the too-short error rather than a miss — a miss would
// bury the mistake in a list of skips.
func TestScopeKeepsThePrefixRulesForAnythingThatIsNotAFullID(t *testing.T) {
	for _, tc := range []struct {
		name, only, want string
	}{
		{"too short", "abcdef0", "too short"},
		{"one character", "a", "too short"},
		{"not hex", "zzzzzzzz", "not hex"},
		{"a word", "resolved", "not hex"},
		{"whitespace only", "   ", "empty"},
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

// A full id is matched against the STORED id, not against the selector's
// spelling, and a single case-folded match still resolves — ids are lowercase
// hex and an operator pasting from a terminal has no reason to know it. Two rows
// differing only in case are not a silent coin toss: the exact reading declines,
// and the prefix rules then report the ambiguity the operator has to settle.
func TestScopeResolvesOneCaseFoldedIDAndRefusesTwo(t *testing.T) {
	got, misses, err := (Scope{Only: []string{recase(scopedA)}}).Select(scopedPool())
	if err != nil || len(misses) != 0 || !eqIDs(scopedIDs(got), []string{scopedA}) {
		t.Errorf("one case-folded match: %v misses %v err %v, want [%s]", scopedIDs(got), misses, err, scopedA)
	}

	// A pool holding a row and its uppercased twin. The byte-exact spelling still
	// names one of them, and that is not a guess; a prefix both share is an error,
	// because nothing in the selector can say which row was meant.
	two := append(scopedPool(), memory.Memory{ID: strings.ToUpper(scopedA), Content: "note a, uppercased id"})
	exact, _, err := (Scope{Only: []string{strings.ToUpper(scopedA)}}).Select(two)
	if err != nil {
		t.Fatalf("the byte-exact id must still resolve beside its case variant: %v", err)
	}
	if !eqIDs(scopedIDs(exact), []string{strings.ToUpper(scopedA)}) {
		t.Errorf("the byte-exact id resolved to %v, want the row it spells", scopedIDs(exact))
	}
	if _, _, err := (Scope{Only: []string{scopedA[:8]}}).Select(two); err == nil {
		t.Error("a prefix shared by a row and its case variant must be an error, not a silent pick")
	}
	// And a spelling that is neither byte-exact nor a unique fold: both rows
	// answer it, so the exact reading declines and the prefix rules carry it to
	// the same refusal.
	if _, _, err := (Scope{Only: []string{recase(scopedA)}}).Select(two); err == nil {
		t.Error("a spelling matching two case variants must be an error, not a silent pick")
	}
}

// exactInPool is the reading that decides whether a selector is a full id, so its
// own rules are worth pinning directly: byte-exact first, then a single
// case-folded match, and two case-variant rows are no match at all.
//
// The pool matters, so each case carries its own: a case-folded match is only
// unique when the pool does not hold the row's twin, and adding the twin is
// exactly what turns that case into the two-variant refusal.
func TestExactInPool(t *testing.T) {
	one := []memory.Memory{{ID: scopedA}, {ID: scopedB}}
	two := append(one, memory.Memory{ID: strings.ToUpper(scopedA)})
	for _, tc := range []struct {
		name  string
		pool  []memory.Memory
		spec  string
		wantI string
		wantO bool
	}{
		{"byte exact", one, scopedA, scopedA, true},
		{"byte exact wins over a case variant", two, strings.ToUpper(scopedA), strings.ToUpper(scopedA), true},
		{"one case-folded match", one, recase(scopedA), scopedA, true},
		{"the same spelling against both twins", two, recase(scopedA), "", false},
		{"a prefix is not an id", one, scopedB[:8], "", false},
		{"no such row", one, strings.Repeat("f", 32), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := exactInPool(tc.pool, tc.spec)
			if ok != tc.wantO || id != tc.wantI {
				t.Errorf("exactInPool(%q) = (%q, %v), want (%q, %v)", tc.spec, id, ok, tc.wantI, tc.wantO)
			}
		})
	}
}
