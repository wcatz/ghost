package resolve

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestReassessOnlyJudgesTheNamedMemories: the reason the flag exists (#698).
// An unscoped repair over a real store proposed un-hiding 143 memories and an
// independent judge found about 35% of them stale — completed changelogs, host
// snapshots, notes a newer memory had already superseded. A repair that knows
// which rows it is about must judge exactly those and leave the rest resolved,
// however loudly the rest of the project would come back KEEP.
func TestReassessOnlyJudgesTheNamedMemories(t *testing.T) {
	target := memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	// Vetoed, so it comes back KEEP for free — and would have been cleared by an
	// unscoped pass. The flag has to keep it out of the run.
	rule := memory.Memory{ID: "b2b2b2b2c3c3d3d3e4e4f4f4a4a4b2b2", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
	other := memory.Memory{ID: "c3c3c3c3d4d4e4e4f5f5a5a5b5b5c3c3", Category: "changelog",
		Content: "The May rollout plan was abandoned; the relay change shipped instead."}
	store := &fakeStore{alreadyResolved: []memory.Memory{target, rule, other}}
	cls := &fakeClassifier{drop: map[string]bool{other.Content: true}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{Only: []string{"a1a1a1a1"}}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Loaded != 1 || res.Pool != 3 {
		t.Errorf("Loaded = %d, Pool = %d, want 1 judged of 3 already resolved", res.Loaded, res.Pool)
	}
	if !eqStrings(cls.askedFor, []string{target.Content}) {
		t.Errorf("the classifier was asked about %v, want only the named note", cls.askedFor)
	}
	if len(reKept) != 1 || reKept[0].ID != target.ID {
		t.Fatalf("reKept = %v, want [target]", reKept)
	}
	if len(store.cleared) != 1 || store.cleared[0] != target.ID {
		t.Errorf("cleared = %v, want [target] — the vetoed rule and the unlisted note stay resolved", store.cleared)
	}
}

// TestReassessScopedKeepsEveryFloor: scoping narrows which rows are judged, not
// what a judged row has to pass. A named row that a live 'supersedes' edge
// still asserts is still asserted: clearing it would print a repair the next
// ordinary pass undoes (issue #640's review finding), and the operator would
// believe it.
func TestReassessScopedKeepsEveryFloor(t *testing.T) {
	asserted := memory.Memory{ID: "d4d4d4d4e5e5f5f5a6a6b6b6c6c6d4d4", Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	rule := memory.Memory{ID: "b2b2b2b2c3c3d3d3e4e4f4f4a4a4b2b2", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
	newer := memory.Memory{ID: "e5e5e5e5f6f6a6a6b7b7c7c7d7d7e5e5", Category: "changelog", UpdatedAt: "2026-09-02 00:00:00",
		Content: "the actuals document has replaced the May cost estimate"}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{asserted, rule},
		candidates:      []memory.Memory{newer},
		links: []memory.Link{{
			SourceID: newer.ID, TargetID: asserted.ID, Relation: "supersedes", Source: "llm",
		}},
	}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true,
		Scope{Only: []string{"d4d4d4d4", "b2b2b2b2"}}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Demoted != 1 {
		t.Errorf("res.Demoted = %d, want 1 — the live edge still asserts the named row", res.Demoted)
	}
	if len(reKept) != 1 || reKept[0].ID != rule.ID {
		t.Errorf("reKept = %v, want [rule]: the asserted row is not repairable", reKept)
	}
	if len(store.cleared) != 1 || store.cleared[0] != rule.ID {
		t.Errorf("cleared = %v, want [rule]", store.cleared)
	}
}

// TestReassessScopedReportsMissesAndStillRuns: a selector the operator mistyped
// must not cost them the rest of the repair. The miss is reported, the rows that
// do exist are judged, and the exit is a success.
func TestReassessScopedReportsMissesAndStillRuns(t *testing.T) {
	target := memory.Memory{ID: "a1a1a1a1b2b2c2c2d3d3e3e3f3f3a1a1", Category: "changelog",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	store := &fakeStore{alreadyResolved: []memory.Memory{target}}
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true,
		Scope{Only: []string{"a1a1a1a1", "ffffffffffffffff"}}, nil)
	if err != nil {
		t.Fatalf("a miss must not fail the run: %v", err)
	}
	if len(res.Misses) != 1 || res.Misses[0].Spec != "ffffffffffffffff" {
		t.Errorf("res.Misses = %v, want the one selector that matched nothing", res.Misses)
	}
	if len(reKept) != 1 || len(store.cleared) != 1 || store.cleared[0] != target.ID {
		t.Errorf("reKept = %v cleared = %v, want the existing row still repaired", reKept, store.cleared)
	}
}

// TestReassessAmbiguousSelectorFailsTheRun: unlike a miss, an ambiguous prefix
// is the one selector error that stops everything — the pass cannot say which
// rows were meant, and judging the wrong ones is the failure this flag was added
// to stop.
func TestReassessAmbiguousSelectorFailsTheRun(t *testing.T) {
	store := &fakeStore{alreadyResolved: []memory.Memory{
		{ID: "abcdef00111111111111111111111111", Content: "one"},
		{ID: "abcdef00222222222222222222222222", Content: "two"},
	}}
	cls := &fakeClassifier{}

	if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{Only: []string{"abcdef00"}}, nil); err == nil {
		t.Fatal("Reassess: an ambiguous selector must be an error, not a silent pick")
	}
	if cls.calls != 0 || len(store.cleared) != 0 {
		t.Errorf("calls = %d cleared = %v, want nothing asked and nothing written", cls.calls, store.cleared)
	}
}

// TestReassessScopeMatchingNothingIsANoOp: every selector missing is a run that
// judged nothing, and it must say so through its counts rather than by looking
// like a pass that found nothing to repair.
func TestReassessScopeMatchingNothingIsANoOp(t *testing.T) {
	rule := memory.Memory{ID: "b2b2b2b2c3c3d3d3e4e4f4f4a4a4b2b2", Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
	store := &fakeStore{alreadyResolved: []memory.Memory{rule}}
	cls := &fakeClassifier{}

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true,
		Scope{Only: []string{"ffffffffffffffff"}}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Loaded != 0 || res.Pool != 1 || len(res.Misses) != 1 {
		t.Errorf("Loaded = %d Pool = %d misses = %v, want 0 judged of 1 with one miss", res.Loaded, res.Pool, res.Misses)
	}
	if len(reKept) != 0 || len(store.cleared) != 0 || cls.calls != 0 {
		t.Errorf("reKept = %v cleared = %v calls = %d, want a no-op", reKept, store.cleared, cls.calls)
	}
}

// eqStrings compares two string slices element-wise, so a test can assert WHICH
// notes were asked about rather than how many.
func eqStrings(got, want []string) bool {
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
