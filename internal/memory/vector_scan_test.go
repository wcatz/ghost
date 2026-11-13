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

// TestSearchVectorAllRankingMatchesTheCollectAndSortBaseline: the cross-project
// leg runs the same scan through its own entry point and must agree too.
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

// --- the uncached copy is sized for the corpus ---

// --- the scoring pass ---

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

func idsOf(got []ScoredMemory) []string {
	out := make([]string, len(got))
	for i, sm := range got {
		out[i] = sm.MemoryID
	}
	return out
}

// --- the scope pre-filter ---

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
