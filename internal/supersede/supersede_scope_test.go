package supersede

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// addScoped creates an embedded memory carrying an explicit scope with a
// controlled age (orient keys off updated_at). add() has no scope knob, and
// scope is the whole subject here, so the two helpers cannot be merged.
func addScoped(t *testing.T, store *memory.Store, db *sql.DB, content string, vec []float32, createdAt string, scope map[string]string) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, "p", memory.Memory{
		Category: "fact", Content: content, Importance: 0.7, Source: "mcp", Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, createdAt, createdAt, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func envScope(env string) map[string]string { return map[string]string{"environment": env} }

// TestSelectCandidatesSkipsScopeConflictingPair: two claims about two
// environments, worded near-identically and near-parallel in vector space. The
// classifier cannot see the difference — scope is a column, not the note text it
// is handed — so the pass must not propose the pair at all.
func TestSelectCandidatesSkipsScopeConflictingPair(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
	addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))

	sel, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 0 {
		t.Errorf("scope-conflicting pair proposed for classification: %+v", sel.Candidates)
	}
}

// TestSelectCandidatesFindsCompatiblePairBelowConflictingOnes covers the
// interaction between the scope guard and the neighbour budget: the candidate
// cut runs inside the store, so a guard applied after it spends the whole budget
// on rows that can never be linked. Here eight conflicting neighbours outrank
// the one compatible row, so under maxNeighbors+1 the compatible row is never
// examined and a genuine supersession is missed.
func TestSelectCandidatesFindsCompatiblePairBelowConflictingOnes(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := addScoped(t, store, db, "production pool timeout is 30s", []float32{1, 0}, "2026-07-01 00:00:00", envScope("production"))
	older := addScoped(t, store, db, "production pool timeout is 5s", []float32{0.85, 0.53}, "2026-01-01 00:00:00", envScope("production"))
	for i := 0; i < maxNeighbors; i++ {
		addScoped(t, store, db, fmt.Sprintf("development candidate %d", i),
			[]float32{1, 0.02 * float32(i+1)}, "2026-01-01 00:00:00", envScope("development"))
	}

	sel, err := SelectCandidates(ctx, store, "p", 0.70)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	found := false
	for _, c := range sel.Candidates {
		if c.NewerID == newer && c.OlderID == older {
			found = true
		}
	}
	if !found {
		t.Errorf("the compatible same-scope pair was not proposed: the scope filter ran after the "+
			"candidate cut, so the budget of %d was spent on conflicting rows ranked above it (candidates=%+v)",
			maxNeighbors, sel.Candidates)
	}
}

// TestRunSpendsNothingOnScopeConflictingPair: the pass's expensive step is a
// billable classify call, so refusing the pair must happen before the call, not
// as a post-hoc refusal of its verdict. A classifier that would have said
// SUPERSEDES must never be reached, and no edge may be written.
func TestRunSpendsNothingOnScopeConflictingPair(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	dev := addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
	prod := addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.calls) != 0 {
		t.Errorf("scope-conflicting pair was sent to the classifier (%d billed call(s)): %+v", len(cls.calls), cls.calls)
	}
	if len(classified) != 0 || res.Candidates != 0 {
		t.Errorf("scope-conflicting pair was classified: candidates=%d classified=%+v", res.Candidates, classified)
	}
	if res.Created != 0 || res.Confirmed != 0 {
		t.Errorf("want no link written; got created=%d confirmed=%d", res.Created, res.Confirmed)
	}
	// GetLinks rather than SupersedesWithin: the latter now withholds a
	// scope-conflicting pair, so it would report "no link" for this pair whether
	// or not one had been written, and the assertion would pass on a broken
	// writer. GetLinks reads the table and applies no scope rule.
	for _, id := range []string{dev, prod} {
		links, err := store.GetLinks(ctx, id)
		if err != nil {
			t.Fatalf("GetLinks(%s): %v", id, err)
		}
		if len(links) != 0 {
			t.Errorf("scope-conflicting pair was linked (%s has %+v)", id, links)
		}
	}
}

// TestRunSkipsReclassifyingScopeConflictingSupersedesLink: a 'supersedes' link
// written before this guard exists is re-proposed on every pass, because
// reclassify candidates are never cache-skipped and the classifier cannot see
// the conflict. Left alone it costs a billed call each pass and re-affirms the
// edge forever. The edge is left in place — scope is a ranking exemption, not a
// delete pass, matching internal/memory's DemotionPenalties — and the consumer
// guard in SupersedePenalties is what makes it inert.
func TestRunSkipsReclassifyingScopeConflictingSupersedesLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Orthogonal vectors, so SelectCandidates never rediscovers the pair on its
	// own: the existing-link reclassify path is the only thing that can classify
	// it, which is exactly the path under test.
	newer := addScoped(t, store, db, "reversed the NATS decision", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
	older := addScoped(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-01-01 00:00:00", envScope("production"))

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// Backdate the link so skip-if-unchanged does not already suppress it.
	if _, err := db.ExecContext(ctx, `UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`, newer, older); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.calls) != 0 {
		t.Errorf("scope-conflicting existing link was re-classified (%d billed call(s)): %+v", len(cls.calls), cls.calls)
	}
	if res.Candidates != 0 {
		t.Errorf("want 0 candidates, got %d", res.Candidates)
	}
	// GetLinks, not SupersedesWithin: the edge must still be in the graph, and
	// SupersedesWithin now withholds a scope-conflicting pair from its result,
	// so it can no longer answer "is the row there?" for this pair. GetLinks
	// reads the table and applies no scope rule.
	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 || links[0].Relation != "supersedes" {
		t.Errorf("the pre-existing edge must be left in place (scope exempts it from ranking, it does not delete it), got %+v", links)
	}
}

// unscopedNeighborStore answers every SearchVectorScoped call with the
// project's full candidate list, ignoring the scope argument. The shipped store
// narrows inside the query, so the guard in SelectCandidates is unreachable
// against it; this stands in for an implementation of vectorStore that does not,
// which is the contract the guard exists to hold.
type unscopedNeighborStore struct {
	*memory.Store
	forced []memory.ScoredMemory
}

func (u *unscopedNeighborStore) SearchVectorScoped(_ context.Context, _ string, _ []float32, _ int, _ map[string]string) ([]memory.ScoredMemory, error) {
	return u.forced, nil
}

// TestSelectCandidatesRefusesConflictingNeighbourFromUnscopedStore: vectorStore
// is an interface, so a conflicting pair must be refused where the candidate is
// chosen, not merely assumed absent. The shipped store narrows inside the query
// and the ScopesConflict test is unreachable against it; this stands in for an
// implementation that does not, which is the contract the test exists to hold.
// Asserted on SelectCandidates rather than through Run, because Run re-checks
// the same rule and would pass either way — a test that cannot tell the two
// guards apart proves nothing about either.
func TestSelectCandidatesRefusesConflictingNeighbourFromUnscopedStore(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	dev := addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
	prod := addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))

	store2 := &unscopedNeighborStore{Store: store, forced: []memory.ScoredMemory{
		{MemoryID: dev, Score: 1.0, Scope: envScope("development")},
		{MemoryID: prod, Score: 0.99, Scope: envScope("production")},
	}}

	sel, err := SelectCandidates(ctx, store2, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 0 {
		t.Errorf("a store that ignores the scope argument must not get its conflicting row proposed: %+v", sel.Candidates)
	}
}
