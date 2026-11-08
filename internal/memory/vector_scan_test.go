package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"
)

// --- ranking equivalence with the pre-#556 implementation ---

// collectAndSortBaseline is the brute-force vector search exactly as it was
// before #556: decode every embedding into a fresh []float32, score every row,
// drop the non-positive cosines, fully sort, narrow by scope, then cut.
//
// It is transcribed from the implementation it replaces rather than shared with
// it. A shared helper would make the comparison a function against itself, and
// the whole claim under test is that the corpus scan no longer decides the
// ranking — so the baseline has to be the old decision procedure, not the new
// one wearing the old name.
func collectAndSortBaseline(t *testing.T, s *Store, projectID string, queryVec []float32, limit int, scope map[string]string, allProjects bool) []ScoredMemory {
	t.Helper()
	ctx := context.Background()

	// The projection is the one the old search ran, COALESCE and all: a NULL
	// scope and an empty one both parse to no scope, and the corpus below has no
	// exact score ties, so the answer cannot depend on the order the planner
	// happened to yield the rows in.
	query := `
		SELECT e.memory_id, e.embedding, e.model, COALESCE(m.scope, ''), m.project_id, m.resolved_at
		FROM memory_embeddings e
		JOIN memories m ON m.id = e.memory_id`
	var args []any
	if !allProjects {
		query += ` WHERE m.project_id = ? OR m.project_id = '_global'`
		args = append(args, projectID)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("baseline query: %v", err)
	}
	defer rows.Close() //nolint:errcheck

	var scored []ScoredMemory
	for rows.Next() {
		var id, model, rowProject string
		var blob []byte
		var scopeCol, resolvedCol sql.NullString
		if err := rows.Scan(&id, &blob, &model, &scopeCol, &rowProject, &resolvedCol); err != nil {
			t.Fatalf("baseline scan: %v", err)
		}
		if identity := s.embeddingIdentityValue(); identity != "" && model != identity {
			continue
		}
		vec := bytesToFloat32s(blob)
		if len(vec) != len(queryVec) {
			continue
		}
		if sim := cosineSimilarity(queryVec, vec); sim > minVectorSimilarity {
			scored = append(scored, ScoredMemory{
				MemoryID: id, Score: sim, Scope: parseScope(scopeCol),
				ProjectID: rowProject, Resolved: resolvedCol.Valid && resolvedCol.String != "",
			})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("baseline rows: %v", err)
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scope) > 0 {
		eligible := scored[:0]
		for _, sm := range scored {
			if ScopeMatches(sm.Scope, scope) {
				eligible = append(eligible, sm)
			}
		}
		scored = eligible
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// rankingCorpus builds the store the equivalence tests compare on. Every branch
// a vector search can take is represented: rows the cosine floor drops, rows
// another vector space excludes, rows whose width is not the query's, rows in
// another project, a resolved row, and scoped rows on both sides of a request.
// The vectors are generated with distinct offsets so no two scores are equal —
// the old search broke such a tie with sort.Slice, which is unstable, so a tie
// in the fixture would make the comparison depend on the implementation it is
// meant to check.
func rankingCorpus(t *testing.T) (*Store, context.Context, []float32) {
	t.Helper()
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "other-proj", "/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// The seeded global project is where a cross-project memory lands, and the
	// project leg searches it alongside the project's own rows.
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	store.SetEmbeddingIdentity(identityCurrent)

	add := func(project, content string, scope map[string]string, identity string, vec []float32) string {
		t.Helper()
		id, err := store.Create(ctx, project, Memory{
			Category: "fact", Content: content, Source: "manual", Scope: scope,
		})
		if err != nil {
			t.Fatalf("Create %q: %v", content, err)
		}
		if vec == nil {
			return id // a memory nobody embedded
		}
		if err := store.StoreEmbedding(ctx, id, vec, identity); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", content, err)
		}
		return id
	}

	// The query leans on the first two coordinates; a per-row offset on the
	// third keeps every neighbour's cosine distinct from every other's.
	query := []float32{1, 0.4, 0, 0}
	for i := range 24 {
		vec := []float32{1, 0.4, 0.001 * float32(i+1), 0}
		add("test-proj", fmt.Sprintf("project row %d", i), nil, identityCurrent, vec)
	}
	add("test-proj", "production row", map[string]string{"environment": "production"}, identityCurrent,
		[]float32{1, 0.4, 0.05, 0})
	add("test-proj", "development row", map[string]string{"environment": "development"}, identityCurrent,
		[]float32{1, 0.4, 0.06, 0})
	add("test-proj", "unscoped but adjacent row", nil, identityCurrent, []float32{1, 0.4, 0.07, 0})
	add("test-proj", "row in a retired vector space", nil, identityStale, []float32{1, 0.4, 0.08, 0})
	add("test-proj", "row from a different width model", nil, identityCurrent, []float32{1, 0.4, 0, 0.09})
	add("test-proj", "opposite row", nil, identityCurrent, []float32{-1, -0.4, 0, 0})
	add("test-proj", "never embedded row", nil, identityCurrent, nil)
	add("other-proj", "other project's row", nil, identityCurrent, []float32{1, 0.4, 0.1, 0})
	add("_global", "global row", map[string]string{"environment": "production"}, identityCurrent,
		[]float32{1, 0.4, 0.11, 0})

	resolved := add("test-proj", "resolved row", nil, identityCurrent, []float32{1, 0.4, 0.12, 0})
	if _, err := store.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	// Keep the generator honest: the fixture is only a valid comparison if the
	// baseline's own scores are all distinct.
	baseline := collectAndSortBaseline(t, store, "test-proj", query, 1000, nil, false)
	if len(baseline) < 20 {
		t.Fatalf("fixture produced %d candidates, want a corpus that exercises the cut", len(baseline))
	}
	for i := 1; i < len(baseline); i++ {
		if baseline[i].Score == baseline[i-1].Score {
			t.Fatalf("fixture has an exact score tie at rank %d (%s and %s); the comparison below cannot be exact", i-1, baseline[i-1].MemoryID, baseline[i].MemoryID)
		}
	}
	return store, ctx, query
}

// assertSameRanking compares two rankings row for row, on the score bit pattern
// as well as the id: the new search has to reach the same float32 the old one
// did, not merely an equally good one.
func assertSameRanking(t *testing.T, label string, want, got []ScoredMemory) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d candidates, want %d\ngot  %+v\nwant %+v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].MemoryID != want[i].MemoryID {
			t.Errorf("%s rank %d = %s, want %s", label, i, got[i].MemoryID, want[i].MemoryID)
			continue
		}
		if math.Float32bits(got[i].Score) != math.Float32bits(want[i].Score) {
			t.Errorf("%s rank %d (%s) scored %v, want %v", label, i, got[i].MemoryID, got[i].Score, want[i].Score)
		}
		if got[i].ProjectID != want[i].ProjectID {
			t.Errorf("%s rank %d (%s) carried project %q, want %q", label, i, got[i].MemoryID, got[i].ProjectID, want[i].ProjectID)
		}
		if got[i].Resolved != want[i].Resolved {
			t.Errorf("%s rank %d (%s) carried resolved=%v, want %v", label, i, got[i].MemoryID, got[i].Resolved, want[i].Resolved)
		}
		if len(got[i].Scope) != len(want[i].Scope) {
			t.Errorf("%s rank %d (%s) carried scope %v, want %v", label, i, got[i].MemoryID, got[i].Scope, want[i].Scope)
			continue
		}
		for k, v := range want[i].Scope {
			if got[i].Scope[k] != v {
				t.Errorf("%s rank %d (%s) carried scope %v, want %v", label, i, got[i].MemoryID, got[i].Scope, want[i].Scope)
			}
		}
	}
}

// TestSearchVectorRankingMatchesTheCollectAndSortBaseline is the property the
// snapshot-and-top-k rewrite of the vector scan has to keep: the corpus is no
// longer collected and fully sorted, so nothing about the ranking is allowed to
// move. Every limit is checked, because the bounded heap only diverges from a
// full sort once the window is smaller than the corpus.
func TestSearchVectorRankingMatchesTheCollectAndSortBaseline(t *testing.T) {
	store, ctx, query := rankingCorpus(t)

	for _, limit := range []int{1, 2, 5, 17, 100, 1000} {
		want := collectAndSortBaseline(t, store, "test-proj", query, limit, nil, false)
		got, err := store.SearchVector(ctx, "test-proj", query, limit)
		if err != nil {
			t.Fatalf("limit %d: SearchVector: %v", limit, err)
		}
		assertSameRanking(t, fmt.Sprintf("SearchVector(limit=%d)", limit), want, got)
	}
}

// TestSearchVectorScopedRankingMatchesTheCollectAndSortBaseline is the same
// comparison for the scoped leg, where the limit counts candidates the caller
// may use rather than rows a post-filter would discard — the reason the scope
// filter runs per row during the scan instead of on the window.
func TestSearchVectorScopedRankingMatchesTheCollectAndSortBaseline(t *testing.T) {
	store, ctx, query := rankingCorpus(t)

	for _, want := range []map[string]string{
		{"environment": "production"},
		{"environment": "development"},
		{"environment": "staging"}, // named by nobody: every row stays eligible
		{"environment": "production", "component": "worker"},
	} {
		for _, limit := range []int{1, 3, 8, 50} {
			label := fmt.Sprintf("SearchVectorScoped(%v, limit=%d)", want, limit)
			expected := collectAndSortBaseline(t, store, "test-proj", query, limit, want, false)
			got, err := store.SearchVectorScoped(ctx, "test-proj", query, limit, want)
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			assertSameRanking(t, label, expected, got)
		}
	}
}

// TestSearchVectorAllRankingMatchesTheCollectAndSortBaseline: the cross-project
// leg (ghost_search_all) runs the same scan through its own entry point, and it
// has to agree with the old one for the same reason.
func TestSearchVectorAllRankingMatchesTheCollectAndSortBaseline(t *testing.T) {
	store, ctx, query := rankingCorpus(t)

	for _, limit := range []int{1, 4, 12, 100} {
		want := collectAndSortBaseline(t, store, "", query, limit, nil, true)
		got, err := store.SearchVectorAll(ctx, query, limit)
		if err != nil {
			t.Fatalf("limit %d: SearchVectorAll: %v", limit, err)
		}
		assertSameRanking(t, fmt.Sprintf("SearchVectorAll(limit=%d)", limit), want, got)
	}
}

// --- the store lock ---

// setDuringVectorSeams installs the copy and scoring-pass seams for one test.
// The production values are no-ops; nothing outside a test sets them.
func setDuringVectorSeams(copyFn, scoreFn func()) {
	if copyFn == nil {
		copyFn = func() {}
	}
	if scoreFn == nil {
		scoreFn = func() {}
	}
	duringVectorCopyFn.Store(copyFn)
	duringVectorScoreFn.Store(scoreFn)
}

// TestVectorSearchScoresWithTheStoreLockReleased is the property #556 is about.
// A vector search copies its candidate rows out of SQLite under the store's read
// lock, and that read lock is what a writer on the store waits for. The cosine
// pass that follows is the O(corpus × dims) half of the search and needs neither
// the lock nor the connection, so it must not be holding them.
//
// The test parks the search inside its scoring pass, after its first row has
// been scored, and asserts a writer
// completes. A search that still held the read lock across the pass would park
// the writer until the pass finished, and the pass is waiting for the test —
// which is exactly the stall the timeout below reports.
//
// The other half of the property is TestVectorSearchCopiesTheCorpusUnderTheStoreReadLock.
func TestVectorSearchScoresWithTheStoreLockReleased(t *testing.T) {
	store, ctx := setupTestStore(t)
	// The scoring seam fires only after a row has been scored, so a corpus with
	// no rows never reaches it and this test fails instead of passing vacuously.
	//
	// The rows and their vectors are written in one transaction rather than as
	// four thousand Create and StoreEmbedding calls: what is asserted is who can
	// take the store lock while the scoring pass runs, not how the corpus got
	// there, and each of those calls is its own committed write whose per-call
	// cost under the race detector adds up to tens of seconds. The corpus is the
	// same rows and the same vectors, and it carries the same model identity.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for i := range 500 {
		var id string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source)
			VALUES ('test-proj', 'fact', ?, 'manual') RETURNING id`,
			fmt.Sprintf("row %d", i)).Scan(&id); err != nil {
			t.Fatalf("seed memory %d: %v", i, err)
		}
		vec := make([]float32, 64)
		for d := range vec {
			vec[d] = float32(i%17) + float32(d)/100
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_embeddings (memory_id, embedding, model) VALUES (?, ?, ?)`,
			id, float32sToBytes(vec), identityCurrent); err != nil {
			t.Fatalf("seed embedding %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	store.SetEmbeddingIdentity(identityCurrent)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once bool
	unpark := func() {
		if !once {
			once = true
			close(release)
		}
	}
	setDuringVectorSeams(nil, func() {
		close(entered)
		<-release
	})
	t.Cleanup(func() { setDuringVectorSeams(nil, nil) })

	searched := make(chan error, 1)
	go func() {
		_, err := store.SearchVector(ctx, "test-proj", make([]float32, 64), 10)
		searched <- err
	}()

	select {
	case <-entered:
	case err := <-searched:
		t.Fatalf("SearchVector returned before reaching its scoring pass (%v), "+
			"so the seam this test observes is not where the search scores", err)
	case <-time.After(30 * time.Second):
		t.Fatal("a vector search never reached its scoring pass within 30s")
	}

	wrote := make(chan struct{})
	go func() {
		// SetEmbeddingIdentity takes the store's write lock, which cannot be
		// granted while a read lock is outstanding.
		store.SetEmbeddingIdentity(identityCurrent)
		close(wrote)
	}()

	select {
	case <-wrote:
	case <-time.After(30 * time.Second):
		unpark() // let the parked search finish so this test exits rather than leaks it
		t.Fatal("a writer could not take the store lock while a vector search was scoring: " +
			"the search is holding its read lock across the corpus pass")
	}
	unpark()
	if err := <-searched; err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
}

// TestVectorSearchReadsIdentityBeforeTakingTheLock: a search reads the store's
// configured embedding identity before it takes the read lock, because the copy
// that follows runs with the read lock held and a second RLock on an RWMutex
// that already has a writer waiting blocks that writer's readers — including
// the second one — forever. The identity itself is process configuration and
// is read without the lock at all (#1738), so this pins the property that
// survives that change: the search takes the store's read lock ONCE. The
// deadlock needs a waiting writer to be visible, which a test cannot arrange,
// so what this pins is the half that is observable: with a read lock already
// outstanding, the search still completes instead of taking a write lock or
// waiting on itself.
func TestVectorSearchReadsIdentityBeforeTakingTheLock(t *testing.T) {
	store, ctx := setupTestStore(t)
	for i := range 200 {
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact", Content: fmt.Sprintf("row %d", i), Source: "manual",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.StoreEmbedding(ctx, id, []float32{float32(i), 1}, identityCurrent); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}
	store.SetEmbeddingIdentity(identityCurrent)

	// Hold the store's read lock, then start a search. The search must be able
	// to take its own read lock and must complete.
	store.mu.RLock()
	defer store.mu.RUnlock()

	done := make(chan error, 1)
	go func() {
		_, err := store.SearchVector(ctx, "test-proj", []float32{1, 1}, 5)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a vector search did not complete while a read lock was already held on the store")
	}
}

// --- the uncached copy is sized for the corpus ---

// TestVectorRowsReserveSizesTheEmbeddingBufferOnce pins what grow-in-place is
// buying: a hint has to produce a buffer of exactly the hinted size in one
// allocation (so appending the hinted bytes never grows it), and it may only
// ever grow a recycled buffer, never shrink it. A shrink would defeat the pool:
// a snapshot that came back with room to spare gets smaller with every search
// that needs less than it has.
func TestVectorRowsReserveSizesTheEmbeddingBufferOnce(t *testing.T) {
	v := &vectorRows{}
	v.reserve(10, 4)
	if cap(v.embeds) != 10*4*4 {
		t.Fatalf("reserve(10, 4) made a buffer of capacity %d, want exactly %d", cap(v.embeds), 10*4*4)
	}
	if len(v.embeds) != 0 {
		t.Fatalf("reserve left %d bytes in the buffer, want an empty buffer to append into", len(v.embeds))
	}
	// Appending exactly the hinted bytes must not grow the buffer: that is the
	// whole point — one allocation instead of append growth.
	v.embeds = append(v.embeds, make([]byte, 10*4*4)...)
	if cap(v.embeds) != 10*4*4 {
		t.Errorf("appending the hinted bytes grew the buffer to capacity %d; reserve did not size it exactly", cap(v.embeds))
	}

	// A smaller hint leaves a buffer that already has room alone.
	v.embeds = v.embeds[:0]
	v.reserve(5, 4)
	if cap(v.embeds) != 10*4*4 {
		t.Errorf("reserve(5, 4) changed the capacity to %d, want the existing %d kept", cap(v.embeds), 10*4*4)
	}
	// A larger hint replaces it, growing to the new hint in one allocation.
	v.reserve(100, 4)
	if cap(v.embeds) != 100*4*4 {
		t.Errorf("reserve(100, 4) made a buffer of capacity %d, want %d", cap(v.embeds), 100*4*4)
	}
	// A non-positive hint is a no-op rather than a panic or an empty allocation.
	for _, rows := range []int64{0, -1} {
		v.reserve(rows, 4)
	}
	v.reserve(10, 0)
	v.reserve(10, -1)
	if cap(v.embeds) != 100*4*4 {
		t.Errorf("a non-positive hint changed the capacity to %d, want the existing %d kept", cap(v.embeds), 100*4*4)
	}
}

// TestSnapshotVectorsSizesTheEmbeddingBufferFromTheRowCount is the wiring half
// of reserve: the count query a caller passes has to reach the buffer before the
// copy, so the buffer the copy fills is exactly count×dims×4 bytes with no
// append growth. Without the wiring the loader still returns the same rows —
// which is why only a capacity assertion can tell the two apart.
func TestSnapshotVectorsSizesTheEmbeddingBufferFromTheRowCount(t *testing.T) {
	store, ctx := setupTestStore(t)
	const dims = 8
	const rows = 40
	for i := range rows {
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact", Content: fmt.Sprintf("row %d", i), Source: "manual",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		vec := make([]float32, dims)
		for d := range vec {
			vec[d] = float32(i+1) + float32(d)/7
		}
		if err := store.StoreEmbedding(ctx, id, vec, identityCurrent); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}
	store.SetEmbeddingIdentity(identityCurrent)

	v := &vectorRows{}
	queryVec := make([]float32, dims)
	for d := range queryVec {
		queryVec[d] = 1 + float32(d)/3
	}
	err := store.snapshotVectors(ctx, vectorProjectScanQuery, []any{"test-proj"},
		vectorProjectPredicate, []any{"test-proj"}, queryVec, identityCurrent, v)
	if err != nil {
		t.Fatalf("snapshotVectors: %v", err)
	}
	if len(v.rows) != rows {
		t.Fatalf("the snapshot loaded %d rows, want %d", len(v.rows), rows)
	}
	want := rows * dims * 4
	if cap(v.embeds) != want {
		t.Fatalf("the embedding buffer has capacity %d, want exactly %d: the copy was not sized from the row count", cap(v.embeds), want)
	}
	if len(v.embeds) != want {
		t.Errorf("the embedding buffer holds %d bytes, want %d (one row per appended vector)", len(v.embeds), want)
	}
}

// TestSnapshotVectorsDoesNotReserveForRowsItWillSkip is the other half of the
// sizing: the count that sizes the buffer has to name only the rows the loader
// can copy. A corpus mid-re-embed — its rows carrying a retired identity, or a
// width the query does not match — would otherwise reserve a corpus-sized buffer
// for rows the copy skips, which is the allocation the identity and width
// filters exist to avoid (a search that appends nothing must allocate nothing).
// The corpus below has no row the loader will copy, half of them foreign and
// half of the configured identity at the wrong width, so dropping either filter
// from the count makes it reserve for rows that never reach the buffer.
func TestSnapshotVectorsDoesNotReserveForRowsItWillSkip(t *testing.T) {
	store, ctx := setupTestStore(t)
	const dims = 8
	for i := range 40 {
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact", Content: fmt.Sprintf("row %d", i), Source: "manual",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		vec := make([]float32, dims)
		if i%2 == 0 {
			// A retired vector space: the identity filter skips it.
			if err := store.StoreEmbedding(ctx, id, vec, identityStale); err != nil {
				t.Fatalf("StoreEmbedding: %v", err)
			}
			continue
		}
		// The configured identity at a width the query does not match: the
		// width filter skips it.
		if err := store.StoreEmbedding(ctx, id, make([]float32, dims+1), identityCurrent); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}
	store.SetEmbeddingIdentity(identityCurrent)

	v := &vectorRows{}
	queryVec := make([]float32, dims)
	err := store.snapshotVectors(ctx, vectorProjectScanQuery, []any{"test-proj"},
		vectorProjectPredicate, []any{"test-proj"}, queryVec, identityCurrent, v)
	if err != nil {
		t.Fatalf("snapshotVectors: %v", err)
	}
	if len(v.rows) != 0 {
		t.Fatalf("the snapshot loaded %d rows, want none: the fixture must hold only rows the copy skips", len(v.rows))
	}
	if cap(v.embeds) != 0 {
		t.Errorf("reserved %d bytes for a corpus of rows the copy skips, want none", cap(v.embeds))
	}
}

// --- the scoring pass ---

// snapshotFixture builds a vectorRows holding n usable rows of dims dimensions,
// so the scoring pass can be measured without a database in the way.
func snapshotFixture(t testing.TB, n, dims int) *vectorRows {
	t.Helper()
	v := &vectorRows{}
	for i := range n {
		id := fmt.Sprintf("mem-%06d", i)
		vec := make([]float32, dims)
		for d := range vec {
			vec[d] = float32(i+1) + float32(d)/7
		}
		row := vectorRowSpan{
			id:      vecSpan{len(v.ids), len(id)},
			project: vecSpan{len(v.projects), len("test-proj")},
			embed:   vecSpan{len(v.embeds), len(vec) * 4},
		}
		v.ids = append(v.ids, id...)
		v.projects = append(v.projects, "test-proj"...)
		v.embeds = append(v.embeds, float32sToBytes(vec)...)
		v.rows = append(v.rows, row)
	}
	return v
}

// TestVectorSearchKeepsOnlyTheResultWindow: a search used to collect every
// positive cosine in the corpus into one slice and fully sort it, so what it held
// while answering was as large as the store. It now keeps a bounded window, and
// the window does not move when the corpus does.
func TestVectorSearchKeepsOnlyTheResultWindow(t *testing.T) {
	const limit = 10
	query := make([]float32, 32)
	for d := range query {
		query[d] = 1 + float32(d)/3
	}

	for _, rows := range []int{250, 1000, 4000} {
		v := snapshotFixture(t, rows, len(query))
		got := v.search(query, limit, nil)
		if len(got) != limit {
			t.Fatalf("%d rows: returned %d candidates, want %d", rows, len(got), limit)
		}
		if len(v.cands) > limit {
			t.Errorf("%d rows: the search held on to %d candidates, want at most the %d-row window",
				rows, len(v.cands), limit)
		}
		// Capacity, not just length: a search that collected the whole corpus
		// and cut it afterwards would still hold a %d-long slice, and the
		// memory it keeps between queries is what has to be bounded.
		if cap(v.cands) > 4*limit {
			t.Errorf("%d rows: the search's candidate buffer grew to %d entries, "+
				"want room for the %d-row window and no more", rows, cap(v.cands), limit)
		}
	}
}

// TestVectorSearchScoresWithoutAllocatingTheCorpus: the scoring pass is the
// half of a search that used to allocate the whole corpus — a decoded []float32
// per row, and a ScoredMemory per positive cosine — and what it allocates now is
// the result window. Quadrupling the corpus must not move that number, because
// the window did not move.
func TestVectorSearchScoresWithoutAllocatingTheCorpus(t *testing.T) {
	const limit = 10
	query := make([]float32, 32)
	for d := range query {
		query[d] = 1 + float32(d)/3
	}

	small := snapshotFixture(t, 250, len(query))
	large := snapshotFixture(t, 1000, len(query))

	smallAllocs := testing.AllocsPerRun(50, func() { small.search(query, limit, nil) })
	largeAllocs := testing.AllocsPerRun(50, func() { large.search(query, limit, nil) })

	if smallAllocs > 4*limit+8 {
		t.Errorf("scoring %d rows into a %d-row window allocated %.0f times, "+
			"want the window and not the corpus", 250, limit, smallAllocs)
	}
	if largeAllocs > smallAllocs+8 {
		t.Errorf("scoring 1000 rows allocated %.0f times against %.0f for 250 rows: "+
			"the per-query cost still scales with the corpus", largeAllocs, smallAllocs)
	}
}

// TestVectorSearchTopKTieBreakIsScanOrder: the bounded heap needs a total order,
// and for rows at exactly the same float32 cosine it is the memory's creation
// order (seq, its rowid), which the fixture assigns in scan order. Without a
// tiebreak the answer would depend on which row happened to reach the cut last.
//
// The rows are alternated {1,0} and {2,0} against a {1,0} query, so every
// cosine is exactly 1.0 while no two rows are byte-identical — a tie produced by
// different data, not by a duplicated vector, and one a different magnitude
// could not be accused of producing by accident. The last case gives the
// eviction path a strictly better candidate that arrives late, which is the
// comparison `offer` actually makes.
func TestVectorSearchTopKTieBreakIsScanOrder(t *testing.T) {
	query := []float32{1, 0}
	build := func(vecs ...[]float32) *vectorRows {
		t.Helper()
		v := &vectorRows{}
		for i, vec := range vecs {
			id := fmt.Sprintf("tie-%d", i)
			blob := float32sToBytes(vec)
			row := vectorRowSpan{
				id:      vecSpan{len(v.ids), len(id)},
				project: vecSpan{len(v.projects), len("test-proj")},
				embed:   vecSpan{len(v.embeds), len(blob)},
				seq:     int64(i + 1),
			}
			v.ids = append(v.ids, id...)
			v.projects = append(v.projects, "test-proj"...)
			v.embeds = append(v.embeds, blob...)
			v.rows = append(v.rows, row)
		}
		return v
	}
	// Every score is 1.0: assert it rather than trust the construction, because a
	// test that claims to exercise a tie and quietly has none is worse than no
	// test at all.
	ties := [][]float32{{1, 0}, {2, 0}, {1, 0}, {2, 0}, {1, 0}, {2, 0}}
	for i, vec := range ties {
		if sim := cosineFromBytes(query, float32sToBytes(vec)); sim != 1 {
			t.Fatalf("fixture row %d scores %v, want exactly 1 — the test needs real ties", i, sim)
		}
	}

	for _, limit := range []int{1, 3, 6} {
		got := build(ties...).search(query, limit, nil)
		if len(got) != limit {
			t.Fatalf("limit %d returned %d candidates, want %d", limit, len(got), limit)
		}
		for i, sm := range got {
			if want := fmt.Sprintf("tie-%d", i); sm.MemoryID != want {
				t.Errorf("limit %d rank %d = %s, want %s (scan order breaks the tie)", limit, i, sm.MemoryID, want)
			}
		}
	}

	// A better candidate arriving after the window is full must displace the
	// worst row held, and the row it displaces is the last of the tied scan
	// order rather than a score picked out of a heap. Nothing scores above 1, so
	// the tied rows here sit at 1/sqrt(2) and the late arrival is the query
	// itself.
	late := [][]float32{{1, 1}, {1, 1}, {1, 1}, {1, 1}, {1, 1}, {1, 1}, {1, 0}}
	got := build(late...).search(query, 6, nil)
	if len(got) != 6 {
		t.Fatalf("eviction case returned %d candidates, want 6", len(got))
	}
	if got[0].MemoryID != "tie-6" {
		t.Errorf("a strictly better candidate arriving last ranked %d, want rank 0 (id tie-6); got %v",
			rankOf(got, "tie-6"), idsOf(got))
	}
	if rankOf(got, "tie-5") != -1 {
		t.Errorf("tie-5 survived a better candidate arriving after it; the window kept %v, want tie-5 evicted as the worst of the tied scan order", idsOf(got))
	}
	if last := got[5].MemoryID; last != "tie-4" {
		t.Errorf("rank 5 is %s, want tie-4 (the survivors keep their tied scan order)", last)
	}
}

// TestVectorSearchCopiesTheCorpusUnderTheStoreReadLock is the other half of
// TestVectorSearchScoresWithTheStoreLockReleased, and the two together are what
// "the copy takes the read lock, the scoring pass does not" means. It parks the
// search mid-copy and asserts a writer is still waiting; a copy that dropped the
// lock would let the writer straight through. Parking mid-copy rather than
// merely starting a search is the point: a search reads the store's identity
// under a read lock before either phase, so a writer blocked at the start of a
// search proves nothing about the copy.
func TestVectorSearchCopiesTheCorpusUnderTheStoreReadLock(t *testing.T) {
	store, ctx, _ := rankingCorpus(t)

	// Park the search once its first row is in the snapshot, so it is mid-copy
	// and not merely about to start one. Only then can the test tell which lock
	// the copy is running under.
	copying := make(chan struct{})
	release := make(chan struct{})
	var released bool
	unpark := func() {
		if !released {
			released = true
			close(release)
		}
	}
	setDuringVectorSeams(func() {
		close(copying)
		<-release
	}, nil)
	t.Cleanup(func() { setDuringVectorSeams(nil, nil) })

	searched := make(chan error, 1)
	go func() {
		_, err := store.SearchVector(ctx, "test-proj", []float32{1, 0.4, 0, 0}, 10)
		searched <- err
	}()

	select {
	case <-copying:
	case err := <-searched:
		unpark()
		t.Fatalf("the search finished (%v) without ever copying a row, so the copy seam proved nothing", err)
	case <-time.After(30 * time.Second):
		unpark()
		t.Fatal("a vector search never reached its copy pass within 30s")
	}

	wrote := make(chan struct{})
	go func() {
		store.SetVectorMinSimilarity(0.25) // takes the store's write lock
		close(wrote)
	}()
	select {
	case <-wrote:
		unpark()
		t.Fatal("a writer took the store's write lock while a vector search was copying the corpus: " +
			"the copy has to hold the read lock, like every other reader in the package")
	case <-time.After(2 * time.Second):
		// Expected: the copy holds the read lock, so the writer waits.
	}
	unpark()

	select {
	case err := <-searched:
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the search did not complete after the copy was released")
	}
	select {
	case <-wrote:
	case <-time.After(30 * time.Second):
		t.Fatal("the writer never completed after the search finished copying")
	}
}

// TestVectorSearchDropsNaNScores: a NaN cosine compares false against
// everything, so the window's comparator cannot rank it and it would be handed
// back as a candidate and then weighted by rank alone in RRF. The old search
// dropped it because it filtered with `sim > floor`; the filter has to keep that
// shape, and this is the test that says so. Unreachable from a stored embedding
// today, which is exactly why it needs a test rather than a comment.
func TestVectorSearchDropsNaNScores(t *testing.T) {
	query := []float32{1, 0}
	v := &vectorRows{}
	add := func(id string, vec []float32) {
		t.Helper()
		blob := float32sToBytes(vec)
		row := vectorRowSpan{
			id:      vecSpan{len(v.ids), len(id)},
			project: vecSpan{0, 0},
			embed:   vecSpan{len(v.embeds), len(blob)},
		}
		v.ids = append(v.ids, id...)
		v.embeds = append(v.embeds, blob...)
		v.rows = append(v.rows, row)
	}
	add("nan-row", []float32{float32(math.NaN()), 0})
	add("real-row", []float32{1, 0})

	if sim := cosineFromBytes(query, float32sToBytes([]float32{float32(math.NaN()), 0})); !math.IsNaN(float64(sim)) {
		t.Fatalf("the fixture does not produce a NaN cosine (got %v), so the test proves nothing", sim)
	}

	got := v.search(query, 10, nil)
	if len(got) != 1 || got[0].MemoryID != "real-row" {
		t.Fatalf("got %v, want only real-row: a NaN cosine must not enter the window", idsOf(got))
	}
}

func rankOf(got []ScoredMemory, id string) int {
	for i, sm := range got {
		if sm.MemoryID == id {
			return i
		}
	}
	return -1
}

func idsOf(got []ScoredMemory) []string {
	out := make([]string, len(got))
	for i, sm := range got {
		out[i] = sm.MemoryID
	}
	return out
}

// TestVectorSearchRejectsNonPositiveLimit: the cut it replaces sliced to a
// non-positive limit without checking, which panicked on a negative one. The
// bounded window has no such arithmetic left to do, so a limit nothing can
// satisfy is an empty answer.
func TestVectorSearchRejectsNonPositiveLimit(t *testing.T) {
	store, ctx := setupTestStore(t)
	id, err := store.Create(ctx, "test-proj", Memory{Category: "fact", Content: "row", Source: "manual"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	for _, limit := range []int{0, -1, -10} {
		got, err := store.SearchVector(ctx, "test-proj", []float32{1, 0}, limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) != 0 {
			t.Errorf("limit %d returned %d candidates, want 0", limit, len(got))
		}
	}
}

// --- the scope pre-filter ---

// TestCosineFromBytesMatchesCosineSimilarity: scoring straight out of the stored
// blob has to be the same number as decoding it first, down to the last bit —
// fusion ranks by the float32 it is given, and a one-ULP difference between the
// two would reorder results for reasons no test could explain. The malformed
// blob is here too, because it is the case where the two implementations are
// most likely to disagree: a blob that is not a whole number of float32s is
// scored from the prefix a decode would have produced, not rejected.
func TestCosineFromBytesMatchesCosineSimilarity(t *testing.T) {
	blobs := [][]float32{
		{},
		{0},
		{1, 0, 0},
		{-1, 0.5, 2},
		{0.1, -0.2, 0.30000001, 1e-8, -12345.678},
		{math.MaxFloat32, math.SmallestNonzeroFloat32, 1},
		{float32(math.Inf(1)), 1, 0},
		{float32(math.NaN()), 1, 0},
	}
	for _, vec := range blobs {
		blob := float32sToBytes(vec)
		want := cosineSimilarity(vec, bytesToFloat32s(blob))
		got := cosineFromBytes(vec, blob)
		if math.Float32bits(got) != math.Float32bits(want) {
			t.Errorf("cosineFromBytes(%v) = %v (bits %#x), cosineSimilarity = %v (bits %#x)",
				vec, got, math.Float32bits(got), want, math.Float32bits(want))
		}
	}

	// A blob one byte longer than a whole vector: len/4 is still the vector's
	// dimension, so both score the prefix that a decode would have produced.
	short := float32sToBytes([]float32{1, 0, 0})
	ragged := append(append([]byte(nil), short...), 0x7f)
	if got, want := cosineFromBytes([]float32{1, 0, 0}, ragged), cosineSimilarity([]float32{1, 0, 0}, bytesToFloat32s(ragged)); got != want {
		t.Errorf("ragged blob: cosineFromBytes = %v, cosineSimilarity = %v", got, want)
	}
	// A blob two bytes short of a second vector reads the same width.
	truncated := short[:len(short)-2]
	if got, want := cosineFromBytes([]float32{1, 0, 0}, truncated), cosineSimilarity([]float32{1, 0, 0}, bytesToFloat32s(truncated)); got != want {
		t.Errorf("truncated blob: cosineFromBytes = %v, cosineSimilarity = %v", got, want)
	}
}

// TestScopeProbeAgreesWithScopeMatches: a vector search decides scope
// eligibility per row from the stored text, skipping the json.Unmarshal for a
// row whose scope cannot mention a requested key. That shortcut is only allowed
// to save a parse — it may not decide membership on its own — so the probe and
// ScopeMatches have to agree on every case, including the keys the shortcut
// declines and the rows where a requested key appears as a value.
func TestScopeProbeAgreesWithScopeMatches(t *testing.T) {
	stored := []string{
		`{"environment":"production"}`,
		`{"environment":"development"}`,
		`{"component":"worker"}`,
		`{"environment":"production","component":"worker"}`,
		`{"region":"env"}`,                   // "env" as a value, not a key
		`{"note":"the environment matters"}`, // the key inside a value
		`{"weird\"key":"production"}`,        // an escaped key
		`{"env<>ironment":"production"}`,     // HTML-escaped by json.Marshal
		`{"environment":""}`,                 // present but empty
		`{}`,
		`not json at all`,
	}
	requests := []map[string]string{
		nil,
		{},
		{"environment": "production"},
		{"environment": "development"},
		{"environment": "staging"},
		{"component": "worker"},
		{"component": "api"},
		{"environment": "production", "component": "worker"},
		{"env": "region"},
		{"region": "env"},
		{`weird"key`: "production"},
		{"weird\"key": "development"},
		{"env<>ironment": "production"},
		{"env<>ironment": "development"},
		{`quote"and<backslash`: "x"},
		{"note": "the environment matters"},
	}

	for _, raw := range stored {
		for _, request := range requests {
			probe := newScopeProbe(request)
			got := probe.eligible([]byte(raw))
			want := ScopeMatches(parseScopeJSON([]byte(raw)), request)
			if got != want {
				t.Errorf("scope %q against request %v: probe says eligible=%v, ScopeMatches says %v", raw, request, got, want)
			}
		}
	}
}

// TestQuoteScopeKeyDeclinesEscapableKeys: the shortcut searches the stored text
// for the requested key as it appears inside a JSON object, which is only sound
// while encoding/json would have written it that way. These are the keys it has
// to decline rather than guess at — and non-ASCII is among them, because
// encoding/json escapes U+2028 and U+2029 and telling those apart from any
// other UTF-8 sequence is not worth a wrong answer.
func TestQuoteScopeKeyDeclinesEscapableKeys(t *testing.T) {
	for _, key := range []string{"environment", "component", "region", "a b", "x-1"} {
		if quoteScopeKey(key) == nil {
			t.Errorf("quoteScopeKey(%q) declined a key json.Marshal writes verbatim", key)
		}
	}
	for _, key := range []string{`a"b`, `a\b`, "a\nb", "a\tb", "a<b", "a>b", "a&b", "a\x00b", "a\x7fb", "tié", "日本", "a b"} {
		if quoteScopeKey(key) != nil {
			t.Errorf("quoteScopeKey(%q) accepted a key encoding/json may have escaped, "+
				"so the substring shortcut could wrongly rule a row out", key)
		}
	}
}
