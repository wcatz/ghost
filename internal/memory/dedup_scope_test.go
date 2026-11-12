package memory

import (
	"context"
	"path/filepath"
	"testing"
)

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

// conflictCutText is stored byte-identically by every row of the corpus below,
// so each row ties on FTS rank and only importance decides the window.
const conflictCutText = "the api listen port is 8443"

// seedConflictCutCorpus stores one production memory and fifteen development
// memories whose text is byte-identical to it, and returns the production id
// and the development ids.
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
