package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// TestScopesConflict pins the rule that keeps folding honest about scope.
//
// Two memories are candidates for the same claim only if they could both be
// true in the same place. Scope states that: if each names the same key with
// a different value, they are not two wordings of one fact but two facts
// about two places, and linking them as duplicates would demote one of them
// behind a partner it never belonged to.
//
// The rule is deliberately narrow. If either side is silent on a key, there
// is no disagreement to find, and an unscoped memory stays as foldable as it
// was before scope existed — which matters because every memory written
// before schema v12 has no scope at all, and a rule that made those inert
// would rewrite the behaviour of the whole existing store.
func TestScopesConflict(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{
			"same value on a shared key agrees",
			map[string]string{"environment": "production"},
			map[string]string{"environment": "production"},
			false,
		},
		{
			"different value on a shared key conflicts",
			map[string]string{"environment": "production"},
			map[string]string{"environment": "development"},
			true,
		},
		{
			"conflict is symmetric",
			map[string]string{"environment": "development"},
			map[string]string{"environment": "production"},
			true,
		},
		{
			"unscoped against scoped agrees",
			nil,
			map[string]string{"environment": "development"},
			false,
		},
		{
			"scoped against unscoped agrees",
			map[string]string{"environment": "development"},
			nil,
			false,
		},
		{
			"disjoint keys agree",
			map[string]string{"environment": "production"},
			map[string]string{"component": "api"},
			false,
		},
		{
			"one shared key conflicting outweighs the agreeing ones",
			map[string]string{"environment": "production", "component": "api"},
			map[string]string{"environment": "development", "component": "api"},
			true,
		},
		{
			"both unscoped agree",
			nil,
			nil,
			false,
		},
	}
	for _, c := range cases {
		if got := ScopesConflict(c.a, c.b); got != c.want {
			t.Errorf("%s: ScopesConflict(%v, %v) = %v, want %v",
				c.name, c.a, c.b, got, c.want)
		}
	}
}

// newDedupStore opens a store with one project ready for folding tests.
func newDedupStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "dedup.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/dedup", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s, ctx
}

// TestDedupDoesNotFoldAcrossConflictingScope is the defect this PR exists
// for. Observed in practice: "The production database is PostgreSQL." folded
// as a likely duplicate of "The development database is SQLite." at score
// 0.60 — two claims about two environments, near-identical in wording,
// linked as duplicates of each other.
//
// The fold preserves both rows, so retrieval still worked, but it wrote a
// 'duplicate' edge between facts that are not duplicates, and the duplicate
// penalty then sinks one behind a partner it does not belong to. Scope is
// exactly the signal that says they never were one claim, and text
// similarity can never supply it.
func TestDedupDoesNotFoldAcrossConflictingScope(t *testing.T) {
	s, ctx := newDedupStore(t)

	devScope := map[string]string{"environment": "development"}
	prodScope := map[string]string{"environment": "production"}

	devID, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{Scope: devScope})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if dup != "" {
		t.Fatalf("first save folded with nothing to fold into (duplicateOf=%s)", dup)
	}

	prodID, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for production is PostgreSQL.", "manual", 0.7, nil,
		UpsertOptions{Scope: prodScope})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if dup != "" {
		t.Errorf("a production memory folded into a development one (duplicateOf=%s): two environments are not two wordings of one fact", dup)
	}
	if prodID == devID {
		t.Fatal("both saves returned the same id")
	}

	// The mirror. Re-saving the development memory must not reach for the
	// production row — a conflict that only blocked one direction would be
	// reinstated by the next save from the other side.
	//
	// Folding into its own original row is correct and expected: identical
	// content in identical scope is exactly what dedup exists to absorb, so
	// this asserts the target rather than demanding no fold at all.
	_, dup, _, err = s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{Scope: devScope})
	if err != nil {
		t.Fatalf("third save: %v", err)
	}
	if dup == prodID {
		t.Error("re-saving the development memory folded it into the production one; the conflict must hold on every save, not only the first")
	}
	if dup != "" && dup != devID {
		t.Errorf("re-saving the development memory folded into %s, which is neither its own row (%s) nor nothing", dup, devID)
	}
}

// TestDedupStillFoldsWithinScope and TestDedupStillFoldsUnscoped are the
// guard against over-correction. Exempting every scoped memory would be as
// wrong as folding across scopes: it would make scope a reason to stop
// deduplicating at all, so the store fills with the near-restatements
// dedup exists to absorb.
func TestDedupStillFoldsWithinScope(t *testing.T) {
	s, ctx := newDedupStore(t)

	prod := map[string]string{"environment": "production"}

	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for production is PostgreSQL.", "manual", 0.7, nil,
		UpsertOptions{Scope: prod})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The production project database uses PostgreSQL.", "manual", 0.7, nil,
		UpsertOptions{Scope: prod})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if dup == "" {
		t.Error("two restatements in the SAME scope did not fold — scope should only block a fold when it disagrees")
	}
}

// TestDedupUnscopedBehaviourUnchanged: every memory written before schema
// v12 has no scope, so a rule that made unscoped rows un-foldable would
// silently change dedup for the entire existing store. An unscoped save
// against a scoped row must fold exactly as it did before, because neither
// side makes a claim the other contradicts.
func TestDedupUnscopedBehaviourUnchanged(t *testing.T) {
	s, ctx := newDedupStore(t)

	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "development"}})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Second save states no scope — no disagreement is possible.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if dup == "" {
		t.Error("an unscoped save stopped folding — scope is silent here, so behaviour must match pre-scope dedup")
	}
}

// TestDedupScopeConflictAcrossCategory: the cross-category probe has the
// same blind spot, and its stricter Jaccard gate does not help — near
// identical wording across environments scores high regardless of category.
// A conflicting scope must exempt the candidate there too.
func TestDedupScopeConflictAcrossCategory(t *testing.T) {
	s, ctx := newDedupStore(t)

	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "development"}})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Different category, so this can only be found by the cross probe.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The project database for development is SQLite.", "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if dup != "" {
		t.Errorf("cross-category probe folded across a scope conflict (duplicateOf=%s)", dup)
	}
}

// conflictCutText is stored byte-identically by every row of the corpus below,
// so each row ties on FTS rank and only importance decides the window.
const conflictCutText = "the api listen port is 8443"

// seedConflictCutCorpus stores one production memory and fifteen development
// memories whose text is byte-identical to it, and returns the production id
// and the development ids.
//
// Both probes order candidates `ORDER BY rank, m.importance DESC LIMIT 15`, and
// rank is a function of the indexed text alone — so fifteen identical rows tie
// at the top of the window and, seeded at a higher importance than the
// production row, occupy all fifteen slots. The production row is then the
// sixteenth candidate the unfiltered window returns: precisely the rank a scope
// check applied AFTER that cut could never see. Rows are created directly
// rather than through Upsert, because fifteen restatements of one sentence are
// what the probe is meant to absorb and seeding them through the fold path
// would collapse the corpus this test depends on.
func seedConflictCutCorpus(t *testing.T, s *Store, ctx context.Context) (compatible string, conflicting []string) {
	t.Helper()
	dev := map[string]string{"environment": "development"}
	for i := range 15 {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: conflictCutText, Source: "manual", Importance: 0.9, Scope: dev,
		})
		if err != nil {
			t.Fatalf("seed conflicting memory %d: %v", i, err)
		}
		conflicting = append(conflicting, id)
	}
	compatible = makeScopedMemory(t, s, conflictCutText, "production")
	return compatible, conflicting
}

// accessCounts reads access_count for ids, keyed by id.
//
// The fold path is the only writer that bumps access_count, and it bumps it on
// the row it chose and on no other, so this is how a test sees which candidate
// a probe picked when duplicateOf alone would not say.
func accessCounts(t *testing.T, s *Store, ctx context.Context, ids []string) map[string]int {
	t.Helper()
	ms, err := s.GetByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	out := make(map[string]int, len(ms))
	for _, m := range ms {
		out[m.ID] = m.AccessCount
	}
	return out
}

// TestDedupFoldsCompatibleDuplicateBelowTheConflictCut is the cost of deciding
// scope after the candidate window instead of inside it (issue #665).
//
// A save's scope can disagree with the fifteen best FTS matches it retrieves
// while agreeing with the sixteenth. When the check ran after the cut, those
// fifteen rows consumed the whole budget, every one of them was skipped as
// conflicting, and the probe reported no match — so a production fact that
// _global already knew was written a second time, unlinked, next to the row it
// restates. Scope has to be a condition of the query, not a filter over what
// the query already decided to return.
func TestDedupFoldsCompatibleDuplicateBelowTheConflictCut(t *testing.T) {
	s, ctx := newDedupStore(t)
	compatible, conflicting := seedConflictCutCorpus(t, s, ctx)
	before := accessCounts(t, s, ctx, conflicting)

	id, dup, score, err := s.UpsertWithOptions(ctx, testProject, "fact",
		conflictCutText, "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if dup != compatible {
		t.Fatalf("duplicateOf = %q, want the compatible production row %q: fifteen development rows ranked above it filled the candidate window, and a duplicate that loses the cut is stored a second time instead",
			dup, compatible)
	}
	if id == "" || id == compatible {
		t.Errorf("id = %q, want a new linked copy rather than a rewrite of the fold target", id)
	}
	if score <= 0 {
		t.Errorf("score = %v, want the match score that chose the fold", score)
	}

	// The new row is the caller's wording, which the default fold always
	// stores — so what makes this a fold rather than a second copy is the
	// 'duplicate' edge joining it to the compatible row, the thing the
	// duplicate penalty and the next re-save both read.
	links, err := s.GetLinks(ctx, id)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "duplicate" && (l.TargetID == compatible || l.SourceID == compatible) {
			found = true
		}
	}
	if !found {
		t.Errorf("links on the new row %+v carry no 'duplicate' edge to the compatible row %q: the restatement was stored as an independent memory", links, compatible)
	}

	// The over-correction guard: narrowing the window must not make the fifteen
	// conflicting rows foldable. Nothing may be strengthened except the target.
	for _, conflictingID := range conflicting {
		after := accessCounts(t, s, ctx, []string{conflictingID})[conflictingID]
		if after != before[conflictingID] {
			t.Errorf("conflicting development row %s was strengthened (access_count %d, was %d): scope still has to refuse it, and refusing it is not the same as excluding it from the window",
				conflictingID, after, before[conflictingID])
		}
	}
}

// TestCrossCategoryDedupFoldsCompatibleDuplicateBelowTheConflictCut is the same
// defect in the cross-category probe, which carries its own statement and its
// own window. Its stricter Jaccard gate changes nothing here: identical wording
// scores 1.0 whatever the category, so scope is the only thing that separates
// the candidates, and it has to be asked before the cut there too.
func TestCrossCategoryDedupFoldsCompatibleDuplicateBelowTheConflictCut(t *testing.T) {
	s, ctx := newDedupStore(t)
	compatible, conflicting := seedConflictCutCorpus(t, s, ctx)
	before := accessCounts(t, s, ctx, conflicting)

	// A different category, so the same-category probe cannot reach any of the
	// seeded rows and only the cross-category probe can find the compatible one.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		conflictCutText, "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "production"}})
	if err != nil {
		t.Fatalf("cross-category save: %v", err)
	}
	if dup != compatible {
		t.Errorf("duplicateOf = %q, want the compatible production row %q: the cross-category window spent itself on fifteen development rows ranked above it",
			dup, compatible)
	}
	for _, conflictingID := range conflicting {
		after := accessCounts(t, s, ctx, []string{conflictingID})[conflictingID]
		if after != before[conflictingID] {
			t.Errorf("conflicting development row %s was strengthened (access_count %d, was %d): the cross-category probe must still refuse the fold across environments",
				conflictingID, after, before[conflictingID])
		}
	}
}
