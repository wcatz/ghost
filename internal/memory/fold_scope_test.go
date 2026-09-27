package memory

import (
	"context"
	"testing"
)

// seedConflictingSupersede stores a production claim and a development claim
// that a 'supersedes' edge joins, newer → older, so the development row is
// nominally the one that replaced the production row.
//
// Nothing writes that edge any more: internal/supersede refuses a pair whose
// scopes conflict. It is the state an existing store is actually in, and every
// rule that reads a 'supersedes' edge has to cope with it rather than assume
// the edge is sound.
func seedConflictingSupersede(t *testing.T, s *Store, ctx context.Context) (newerID, olderID string) {
	t.Helper()
	olderID = makeScopedMemory(t, s, "the api listen port is 8443", "production")
	newerID = makeScopedMemory(t, s, "the api listen port is 8443", "development")
	if err := s.CreateLink(ctx, newerID, olderID, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	return newerID, olderID
}

// TestFoldIgnoresScopeConflictingSupersedesEdge is the harm the review found:
// a 'supersedes' edge that names two different places made its target look dead
// to every fold path, so a re-save of that exact text inserted a second row
// instead of folding. A production fact duplicated by every re-save is how a
// store fills with restatements dedup exists to absorb, and the duplicate rows
// it creates are themselves never marked resolved, so they rank beside the
// original rather than in place of it.
//
// The edge must not be deleted — scope exempts a pair from ranking, it does not
// rewrite graph history — it must simply stop being read as a reason to refuse
// the fold. A same-scope edge keeps refusing it, which is the direction that
// would be wrong to over-correct.
func TestFoldIgnoresScopeConflictingSupersedesEdge(t *testing.T) {
	s, ctx := newDedupStore(t)
	prod := map[string]string{"environment": "production"}
	_, prodID := seedConflictingSupersede(t, s, ctx)

	// FoldOnly because that is the path that re-verifies the target: it
	// strengthens the row and returns WITHOUT storing the incoming wording, so
	// a target it wrongly calls dead loses the text entirely and inserts a
	// second copy instead.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the api listen port is 8443", "manual", 0.7, nil,
		UpsertOptions{Scope: prod, FoldOnly: true})
	if err != nil {
		t.Fatalf("FoldOnly re-save: %v", err)
	}
	if dup != prodID {
		t.Errorf("re-saving the production text folded into %q, want the production row %q: a 'supersedes' edge whose endpoints name different environments says the development row replaced nothing, so it cannot make the production row dead",
			dup, prodID)
	}

	// The over-correction guard: a same-scope edge still makes its target dead.
	same := makeScopedMemory(t, s, "the worker pool size is 12", "production")
	sameNewer := makeScopedMemory(t, s, "the worker pool size is 12", "production")
	if err := s.CreateLink(ctx, sameNewer, same, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink same scope: %v", err)
	}
	_, dup, _, err = s.UpsertWithOptions(ctx, testProject, "fact",
		"the worker pool size is 12", "manual", 0.7, nil,
		UpsertOptions{Scope: prod, FoldOnly: true})
	if err != nil {
		t.Fatalf("FoldOnly re-save (same scope): %v", err)
	}
	if dup == same {
		t.Error("a same-scope 'supersedes' edge stopped protecting the fold: the edge is what makes the target dead, and only a scope conflict disclaims it")
	}
}

// TestCrossCategoryProbeFoldsAcrossScopeConflictingSupersedesEdge: the
// cross-category probe carries the same exclusion in its own SQL, so the same
// edge blocks a cross-category fold too. The default fold (no FoldOnly) writes
// a 'duplicate' edge and a linked copy rather than nothing, but the outcome
// this asserts is the one that matters either way: one row for the claim, not
// two.
func TestCrossCategoryProbeFoldsAcrossScopeConflictingSupersedesEdge(t *testing.T) {
	s, ctx := newDedupStore(t)
	prod := map[string]string{"environment": "production"}
	_, prodID := seedConflictingSupersede(t, s, ctx)

	// A different category, so the same-category probe cannot find the
	// production row and only the cross-category probe can.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the api listen port is 8443", "manual", 0.7, nil,
		UpsertOptions{Scope: prod})
	if err != nil {
		t.Fatalf("cross-category re-save: %v", err)
	}
	if dup != prodID {
		t.Errorf("cross-category fold went to %q, want the production row %q: the probe's SQL excluded a row as superseded without asking whether the superseder names a different place",
			dup, prodID)
	}
}
