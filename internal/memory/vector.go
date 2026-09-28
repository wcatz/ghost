package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// beforeHybridHydrateFn is a test seam between candidate selection and
// hydration. Production leaves it as a no-op; tests use it to model a delete
// landing after the leg queries release their read locks.
var beforeHybridHydrateFn atomic.Value // func([]string)

func init() {
	beforeHybridHydrateFn.Store(func([]string) {})
}

func beforeHybridHydrate(ids []string) {
	fn, _ := beforeHybridHydrateFn.Load().(func([]string))
	fn(ids)
}

// StoreEmbedding saves an embedding vector for a memory.
// The vector is stored as raw little-endian float32 bytes.
// model records the identity of the space that produced vec — the
// "<model>[:<dimensions>][+prefix]" string from embedding.VectorIdentity — and
// is what vector search matches against the configured identity and the
// embedding worker matches to decide a row needs rewriting.
//
// Rewriting a row under a different identity also retires the link scan the
// memory earned in the old space: link_scans records that its neighbours were
// compared in ONE vector space, and a scan slot that survives the change means
// the memory is never scanned again — its links would stay exactly the ones
// the retired space produced, feeding `ghost supersede` and the Obsidian graph
// forever. Clearing the slot re-queues the memory so the linker compares it
// again in the new space; the re-scan adds current-space edges alongside the
// old ones rather than replacing them (only supersede invalidates edges, and
// CreateLink keeps MAX(strength)). The delete runs
// before the upsert only because it has to read the old model first; the
// ordering fails safe in both directions, so no transaction is needed. If the
// upsert then fails, the memory is merely re-queued for a scan it did not need,
// which the linker repeats idempotently. If the delete fails, this returns
// before touching the vector, so the state is exactly what it was.
func (s *Store) StoreEmbedding(ctx context.Context, memoryID string, vec []float32, model string) error {
	blob := float32sToBytes(vec)

	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM link_scans
		WHERE memory_id = ? AND EXISTS (
			SELECT 1 FROM memory_embeddings WHERE memory_id = ? AND model <> ?
		)
	`, memoryID, memoryID, model); err != nil {
		return fmt.Errorf("invalidate link scan: %w", err)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_embeddings (memory_id, embedding, model)
		VALUES (?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET embedding = excluded.embedding, model = excluded.model, created_at = datetime('now')
	`, memoryID, blob, model)
	if err != nil {
		return fmt.Errorf("store embedding: %w", err)
	}
	return nil
}

// DeleteEmbedding removes the embedding for a memory.
func (s *Store) DeleteEmbedding(ctx context.Context, memoryID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id = ?`, memoryID)
	return err
}

// EmbeddingCosines returns the cosine similarity between queryVec and the stored
// embedding of each requested memory, keyed by memory ID. An ID is simply absent
// from the result when there is no score to report for it: no stored vector, a
// width that does not match queryVec, or a row whose recorded identity belongs to
// another vector space (a model, dimension or task-prefix change — see
// embedding.VectorIdentity). Inventing a zero for any of those would read as a
// measured non-match rather than a missing one, and a cross-space cosine is not
// even that: it is an arbitrary number, which is why usableVectorEntries skips
// those rows for the vector legs and GetEmbedding returns nil for them. Foreign
// rows are counted and logged here for the same reason — a reconfiguration is
// exactly when the operator is watching the log — but only on the first lookup
// to meet a given retired identity, through the shared warnForeignOnce gate, so
// this once-per-query call cannot print the same pending re-embed once per
// query.
//
// It exists because a search result's score is not always in the vector leg's own
// output. Fusion admits the top keyword hits on a reserved slot whatever their
// cosine, so a result can be in the window while sitting below every fetched
// vector list — and a caller that wants to know how strongly a result matched
// (a score-gated abstention rule, the bench harness's false-positive report) has
// to read the row's own vector rather than infer a score from a list the row was
// never in. This is the same cosine the vector leg computes, over exactly the
// rows asked for, so it costs a window's worth of reads instead of a second scan
// of the project.
func (s *Store) EmbeddingCosines(ctx context.Context, ids []string, queryVec []float32) (map[string]float32, error) {
	if len(ids) == 0 || len(queryVec) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	// Read the identity before taking the lock, for the reason searchVector does:
	// the reads below run with the read lock held, and a second RLock on a
	// RWMutex with a writer waiting blocks that writer's readers — including this
	// one — forever.
	identity := s.configuredEmbeddingIdentity()

	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(`
		SELECT memory_id, embedding, model FROM memory_embeddings
		WHERE memory_id IN (%s)
	`, strings.Join(ph, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("embedding cosines: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	cosines := make(map[string]float32, len(ids))
	// foreign counts skipped rows per stored identity rather than as one total,
	// for the reason usableVectorEntries gives: retirements overlap (the model
	// changes again before the first re-embed finishes), and a single total
	// reported against whichever identity the rows happened to yield first would
	// absorb the newer retirement into the older one's line.
	foreign := make(map[string]int)
	mismatched, mismatchedModel := 0, ""
	for rows.Next() {
		var id, model string
		var blob []byte
		if err := rows.Scan(&id, &blob, &model); err != nil {
			return nil, fmt.Errorf("embedding cosines: %w", err)
		}
		if identity != "" && model != identity {
			foreign[model]++
			continue
		}
		vec := bytesToFloat32s(blob)
		if len(vec) != len(queryVec) {
			mismatched++
			if mismatchedModel == "" {
				mismatchedModel = model
			}
			continue
		}
		cosines[id] = cosineSimilarity(queryVec, vec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding cosines: %w", err)
	}
	if s.logger != nil && len(foreign) > 0 {
		// Sorted, so a log is deterministic whichever order the rows came back
		// in. The gate makes this the one line the process ever writes about a
		// retired identity: this lookup runs once per query (the bench harness
		// calls it 244 times in a single `ghost bench`), so a warning per call
		// reports the same pending re-embed a line at a time for as long as it
		// runs — and a search that already reported this identity has said
		// everything the operator needs to hear. warnForeignOnce shares one gate
		// with usableVectorEntries for exactly that reason.
		models := make([]string, 0, len(foreign))
		for m := range foreign {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			if s.warnForeignOnce(m) {
				s.logger.Warn("embedding cosine lookup skipped rows from another vector space — the configured embedding model changed and those rows are waiting to be re-embedded (reported once per retired identity)",
					"skipped", foreign[m], "scored", len(cosines), "configured_identity", identity, "stored_identity", m)
			}
		}
	}
	// The width warning stays per-call, as in usableVectorEntries: it cannot
	// repeat during a re-embed, because the identity check above takes those
	// rows first.
	if s.logger != nil && mismatched > 0 {
		s.logger.Warn("embedding cosine lookup skipped vectors whose width differs from the query — the embedding model likely changed; re-embed to restore comparable scores",
			"skipped", mismatched, "scored", len(cosines), "query_dims", len(queryVec), "stored_identity", mismatchedModel)
	}
	return cosines, nil
}

// UnembeddedMemoryIDs returns memory IDs that still need an embedding produced
// by identity: rows with no vector at all, and rows whose vector was stamped
// with a different identity — a model, dimension or task-prefix change (see
// embedding.VectorIdentity for what an identity is). Both are re-embedded by
// the worker, so a model change retires the old vectors instead of leaving
// them to be compared with queries embedded in the new space.
//
// An empty identity means "any recorded vector counts", which is the
// pre-identity existence check and the state of a store nobody has configured
// an embedding model for (the bench harness, tests).
func (s *Store) UnembeddedMemoryIDs(ctx context.Context, projectID, identity string, limit int) ([]string, error) {
	// Two statements rather than one with a conditional predicate: the
	// identity-less form has to be exactly the existence check it was before,
	// and a single statement expressing "…OR (identity <> '' AND e.model <> ?)"
	// would restate that rule in a way a later edit can easily turn inside out.
	query := `
		SELECT m.id
		FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.project_id = ? AND e.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`
	args := []any{projectID, limit}
	if identity != "" {
		query = `
		SELECT m.id
		FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id
		WHERE m.project_id = ? AND (e.memory_id IS NULL OR e.model <> ?)
		ORDER BY m.created_at DESC
		LIMIT ?
		`
		args = []any{projectID, identity, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("unembedded memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetMemoryContent returns the content of a memory by ID.
func (s *Store) GetMemoryContent(ctx context.Context, id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM memories WHERE id = ?`, id).Scan(&content)
	return content, err
}

// SearchVector performs brute-force cosine similarity search against stored embeddings.
// Returns memory IDs sorted by descending similarity.
func (s *Store) SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorLeg(ctx, projectID, queryVec, limit, nil)
	return rows, err
}

// SearchVectorScoped is SearchVector with a scope constraint applied *before*
// the limit. SearchVector truncates to its top `limit` candidates, so a caller
// that filters scope afterwards can be handed only rows it may not use, and a
// compatible row ranked just below the cut is never seen at all. Filtering first
// is the whole point: the limit then counts eligible candidates.
//
// Scope is not folded into the SQL. The predicate is ScopeMatches — a row that
// does not mention a requested key stays eligible — which is a per-key
// comparison over decoded JSON, and re-expressing it in SQL would duplicate the
// rule the linker and fusion already share. The scan is brute force over the
// project's embeddings either way, so deciding per row during the scan costs
// nothing that deciding on the results would not have cost either.
func (s *Store) SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorLeg(ctx, projectID, queryVec, limit, scope)
	return rows, err
}

// vectorLegFacts is what a vector leg learned about the rows it could not use.
// It is reported rather than only logged because it decides whether the leg's
// silence means "nothing matched" or "rows were skipped": a vector written by
// another model, or one whose dimensions differ, is invisible to the scan, and
// treating those rows as absent is how a changed-model setup looks like an empty
// store.
//
// It carries the two counts and nothing else. The stored identities behind the
// foreign count stay local to the scan, where the warning log names them one per
// identity; a model name cannot reach a caller anyway, because LegStatus has no
// field to carry it, and a field no consumer can read is a claim nothing checks.
type vectorLegFacts struct {
	mismatched, foreign int
}

// searchVectorLeg is the project's vector leg, with its facts. Every exported
// wrapper delegates here, so the facts are computed once on the one scan rather
// than re-derived by a second pass over the embeddings.
func (s *Store) searchVectorLeg(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]ScoredMemory, vectorLegFacts, error) {
	// Read the identity before taking the lock: the helper below runs with the
	// read lock held, and a second RLock on a RWMutex that has a writer waiting
	// blocks that writer's readers — including this one — forever.
	identity := s.configuredEmbeddingIdentity()

	rows := s.borrowVectorRows()
	defer s.returnVectorRows(rows)

	// Everything the cosine pass needs is copied out of SQLite here, under the
	// store's read lock and its single connection, and the copy is the only part
	// that needs either of them. Scoring then runs with both released: the
	// cosine pass is O(corpus × dims) of float arithmetic, and holding a read
	// lock across it blocked every writer on the store for the length of the
	// whole corpus. The copy itself still holds the lock, because it is a read
	// and every other reader in this package takes it — and because the
	// connection is taken for its duration either way (OpenDB pins the pool at
	// one), so releasing the mutex without releasing the connection would not
	// have let a writer through.
	if err := s.snapshotVectors(ctx, vectorScanColumns+`
		WHERE m.project_id = ? OR m.project_id = '_global'
	`, []any{projectID}, queryVec, identity, rows); err != nil {
		return nil, vectorLegFacts{}, err
	}
	// rows.facts is what the copy pass could not use; the scoring pass below is
	// the same bounded window as every other vector search, so the facts and the
	// results come off one scan.
	return rows.search(queryVec, limit, scope), rows.facts, nil
}

// warnForeignOnce claims the store's foreign-vector warning for storedIdentity
// and reports whether it may be logged. The gate is keyed on the retired
// identity: the first search to skip rows from a given identity wins it, every
// later search during the same re-embed stays quiet (one line per query buries
// the state it reports), but a second reconfiguration in the same process —
// a different retired identity — warns again, because that is a new diagnosis
// with its own stored_identity. A store with no gate (a literal built without
// one) shares nothing with an explain trace store and falls back to warning
// every time — the pre-gate behavior, which is no worse than staying silent
// about a reconfiguration.
func (s *Store) warnForeignOnce(storedIdentity string) bool {
	if s.foreignWarned == nil {
		return true
	}
	s.foreignWarned.mu.Lock()
	defer s.foreignWarned.mu.Unlock()
	if s.foreignWarned.warned[storedIdentity] {
		return false
	}
	s.foreignWarned.warned[storedIdentity] = true
	return true
}

// minVectorSimilarity is the cosine floor below which a vector candidate is
// discarded before fusion. Only non-positive scores are dropped today — the
// conservative choice, since RRF already ranks by position and a higher floor
// risks evicting genuinely weak semantic matches.
const minVectorSimilarity float32 = 0

// ScoredMemory pairs a memory ID with a relevance score.
type ScoredMemory struct {
	MemoryID string
	Score    float32
	// Scope lets fusion and window selection apply the memory's own scope
	// without a second lookup.
	Scope map[string]string
	// ProjectID and Resolved carry the row's search status for the same
	// reason: the vector scan already joins memories, so status demotion can
	// run inside fusion without a second read per candidate. Both legs must
	// supply them, or a semantic-only match would escape the demotion.
	ProjectID string
	Resolved  bool
}

// SearchParams parameterizes hybrid-search fusion. The zero value disables
// every optional signal — use DefaultSearchParams for production behavior. The
// bench harness (ghost bench --sweep) grid-searches these knobs against the graded
// dataset; defaults should only change on the strength of those numbers.
//
// Status demotion is deliberately not one of those knobs: a resolved row's
// factor and a project-scoped _global row's factor are properties of the rows
// being ranked, not tunables (see statusDemotionFactor), so they apply under
// any params.
type SearchParams struct {
	FTSWeight float64 // RRF weight of the full-text leg
	VecWeight float64 // RRF weight of the vector leg
	RRFK      int     // RRF smoothing constant (Cormack & Clarke use 60)
	// DecayEnabled applies the category-aware time-decay factor
	// (decayFactor — the Go mirror of DecayRankingSQL) to reorder the result
	// window after truncation: decay reorders but never changes membership, so
	// a more-relevant memory is never dropped in favor of an unrelated younger
	// one. True by default (production behavior). The bench harness toggles it
	// to measure decay-on vs decay-off impact.
	DecayEnabled bool
	// SupersedeDemote, when true, demotes a memory below its superseder within
	// the result window when a valid 'supersedes' link between them exists AND
	// both are present. Unlike the recency prior it is targeted — it only ever
	// touches genuine replacement pairs, so it flips the staleness suite
	// without the collateral damage the recency-trap frontier showed. On by
	// default in production (DefaultSearchParams). See docs/benchmarks.md
	// Phase 3.
	SupersedeDemote bool
	// DecayReselect, when true, changes decayRank membership: keep the top
	// limit*2 by base score, then select the top limit by base×decay — so a
	// fresh memory ranked just below the pure-base cut can be rescued.
	// false (default) is the historical behavior: relevance owns membership
	// and decay only reorders the surviving window. Ship-gated on the
	// staleness and recency-trap suites — see docs/benchmarks.md Phase 3.
	DecayReselect bool
	// MinSimilarity is the cosine floor applied to vector-leg candidates
	// AFTER SearchVector's non-positive drop and BEFORE fusion. RRF awards by
	// rank alone, so without a floor every weak positive cosine still claims
	// a vector rank and can enter the fused window — padding results with
	// near-misses when the corpus has no true match. FTS candidates are
	// exempt (they have no cosine). 0 preserves the historical behavior of
	// only dropping non-positives. Production overrides this from config via
	// Store.SetVectorMinSimilarity (search.min_similarity).
	MinSimilarity float32
	// Scope is applied before the fused window cut so eligible candidates can
	// backfill rows excluded by scope. It follows ScopeMatches semantics: a
	// row that does not mention a requested key remains eligible.
	Scope map[string]string
	// ProjectID is the project the search is scoped to; it decides whether a
	// _global row is status-demoted (see statusDemotionFactor). It is not a
	// caller-supplied knob: searchHybridLegs stamps it from the projectID it
	// is searching, so the factor can never disagree with the legs' own
	// scoping. Empty means a cross-project search, where nothing is demoted
	// for being global.
	ProjectID string
	// trace, when non-nil, receives a per-candidate record of what this ranking
	// decided. Only ExplainSearchScoped sets it, and only explain reads it, so
	// the production path pays a nil check per stage and allocates nothing.
	//
	// It is here, and not an extra argument on every stage, because SearchParams
	// is already the one value that reaches fusion, scope narrowing, window
	// selection, decay and both demotions — a trace that had to be threaded
	// separately would be one a future stage could forget to accept, and a stage
	// that forgets is a stage whose decisions explain cannot report. See
	// ranktrace.go for why explain must not rebuild these numbers.
	trace *searchTrace
}

// DefaultSearchParams returns the production fusion parameters.
//
// Time-awareness comes from the always-on category-aware decay (DecayEnabled,
// see decayFactor) — not a blanket age-only freshness prior. The recency-trap
// suite in docs/benchmarks.md showed an untargeted prior damages correct-wins,
// which is why a flat RecencyWeight/RecencyTau prior was removed in favor of
// the category-aware factor (preference/convention/fact never decay, pattern/
// architecture tau 45, decision/gotcha/dependency tau 30).
//
// A link-graph expansion bonus was evaluated and removed. It was structurally
// dominated by simply retrieving a deeper vector-k: links are built from cosine
// similarity and the vector leg is also cosine, so the bonus only re-surfaced
// cosine-neighbors a larger k already reaches — a public LongMemEval-S kill
// experiment confirmed its recoveries were a strict subset of deeper-k's, with
// no headroom at production depth. The link graph is retained for Obsidian
// export and supersedes ranking. See docs/architecture.md.
//
// SupersedeDemote graduates to on here — the last step of the supersedes
// feature, which docs/benchmarks.md Phase 3 left deliberately separate because
// it changes live ranking. It is safe as a default for three reasons the
// benchmarks establish: it is a hard no-op unless a 'supersedes' edge joins
// two memories inside one result window; those edges only exist if the user
// ran `ghost supersede --apply`, which is itself opt-in and LLM-confirmed; and
// on the graded suites it flips staleness fresh-wins 0.083 -> 1.0 while leaving
// recency-trap correct-wins untouched at 0.929
// (TestSupersedeDemoteClearsFrontier). Leaving it off meant a memory the user
// had explicitly marked as replaced could still outrank its replacement in
// ghost_memory_search.
func DefaultSearchParams() SearchParams {
	return SearchParams{
		FTSWeight:       0.3,
		VecWeight:       0.7,
		RRFK:            60,
		DecayEnabled:    true,
		SupersedeDemote: true,
		MinSimilarity:   0, // historical: only non-positive cosines dropped
	}
}

// filterVectorFloor drops vector candidates at or below floor (cosine).
// Membership-preserving for the FTS leg by construction — only ScoredMemory
// values are filtered. floor <= 0 is a no-op beyond SearchVector's own
// non-positive drop.
func filterVectorFloor(vec []ScoredMemory, floor float32) []ScoredMemory {
	if floor <= 0 || len(vec) == 0 {
		return vec
	}
	kept := vec[:0:0]
	for _, sm := range vec {
		if sm.Score > floor {
			kept = append(kept, sm)
		}
	}
	return kept
}

// demoteSuperseded reorders results so a superseded memory falls below every
// present memory that supersedes it. It penalizes each result by the number of
// its present superseders and stable-sorts by that penalty ascending — so a
// memory with no present superseder keeps its place, and an update chain
// (v3 supersedes v2 and v1; v2 supersedes v1 — star links) orders v3, v2, v1.
// A no-op when SupersedeDemote is off or no supersedes edge joins two results,
// so it never fires on unrelated memories (the recency-trap case). Errors are
// non-fatal: the unreordered results are returned.
func (s *Store) demoteSuperseded(ctx context.Context, results []Memory, p SearchParams) []Memory {
	if !p.SupersedeDemote || len(results) < 2 {
		return results
	}
	ids := make([]string, len(results))
	for i, m := range results {
		ids[i] = m.ID
	}
	s.mu.RLock()
	penalty, err := supersedeVerdicts(ctx, s.queryDB(), ids, p.trace)
	s.mu.RUnlock()
	if err != nil {
		s.logger.Debug("supersede demote: lookup failed", "error", err)
		return results
	}
	if len(penalty) == 0 {
		return results
	}
	return StableDemote(results, func(m Memory) string { return m.ID }, penalty)
}

// parseCreatedAt parses the SQLite datetime('now') format stored in
// memories.created_at. On failure it returns the zero time (ancient), so the
// caller applies no freshness boost.
func parseCreatedAt(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// decayFactor returns the category-aware time-decay multiplier for a memory
// given its age in days. It mirrors DecayRankingSQL (store.go) exactly — a
// pinned memory or a preference/convention/fact never decays (factor 1.0);
// pattern/architecture decay with tau 45 and a 0.3 floor; all other categories
// (decision, gotcha, dependency, ...) decay with tau 30 and a 0.15 floor. The
// SQL-vs-Go parity test (store_test.go) guards against drift between this and
// the SQL constant.
func DecayFactor(category string, pinned bool, ageDays float64) float64 {
	if pinned {
		return 1.0
	}
	switch category {
	case "preference", "convention", "fact":
		return 1.0
	case "pattern", "architecture":
		return math.Max(0.3, 1.0/(1.0+ageDays/45.0))
	default:
		return math.Max(0.15, 1.0/(1.0+ageDays/30.0))
	}
}

// decayRank truncates results to limit, then (when enabled) reorders by base
// score × decayFactor. base is the fused score when scores is non-nil;
// otherwise it is synthesized from position (the FTS-only paths),
// base = 1/(RRFK+rank+1).
//
// Membership rules:
//   - DecayReselect=false (default, historical): truncate to limit by base
//     alone — relevance owns membership; decay only reorders the surviving
//     window. A fresh memory ranked below the cut by base is NOT rescued.
//   - DecayReselect=true: keep the top limit*2 by base, then select the top
//     limit by base×decay — a fresh memory just below the pure-base cut can
//     enter the final window. Ship-gated on staleness + recency-trap suites.
//
// Age reads created_at — never updated_at, which Upsert's strengthen path
// bumps. An unparseable created_at is treated as ancient so a malformed
// timestamp can never spuriously win. The sort by base happens in both modes
// — the fused path hydrates via GetByIDs, which does NOT preserve order.
func decayRank(results []Memory, scores map[string]float64, p SearchParams, limit int, now time.Time) []Memory {
	scored := make([]struct {
		m    Memory
		base float64
	}, len(results))
	for i, m := range results {
		base := scores[m.ID]
		if scores == nil {
			// No production caller reaches this: every path into decayRank comes
			// from fuseAndRank, which always supplies a score map (the keyword-only
			// path's is the unweighted 1/(K+rank+1) base, not a missing map). Kept
			// so a caller that has no fused scores at all still ranks on rank
			// order rather than on a map miss reading as 0.
			base = 1.0 / float64(p.RRFK+i+1)
		}
		scored[i] = struct {
			m    Memory
			base float64
		}{m, base}
		// The factor is computed HERE, where the order is taken, rather than by
		// explain afterwards: this is the multiplier that decided the sequence,
		// so recording it is the only way a reader can verify the sequence. The
		// clock travels with it for the same reason — an explanation measured
		// against its own wall clock reports a different factor than the ranking
		// used, and the difference shows up only on old rows.
		if t := p.trace.row(m.ID); t != nil {
			t.AgeDays = ageDays(m.CreatedAt, now)
			t.Decay = DecayFactor(m.Category, m.Pinned, t.AgeDays)
			if p.trace.now.IsZero() {
				p.trace.now = now
			}
		}
	}

	// Sort by base first: relevance is the primary key in both modes.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].base != scored[j].base {
			return scored[i].base > scored[j].base
		}
		return scored[i].m.ID < scored[j].m.ID
	})

	baseCut := limit
	if p.DecayReselect && p.DecayEnabled {
		// Keep a wider base window so decay can choose among more candidates.
		baseCut = limit * 2
	}
	if len(scored) > baseCut {
		scored = scored[:baseCut]
	}

	if p.DecayEnabled {
		// Reorder by base × decay (ordering; with DecayReselect this also
		// owns final membership via the subsequent truncate).
		sort.SliceStable(scored, func(i, j int) bool {
			fi := scored[i].base * DecayFactor(scored[i].m.Category, scored[i].m.Pinned, ageDays(scored[i].m.CreatedAt, now))
			fj := scored[j].base * DecayFactor(scored[j].m.Category, scored[j].m.Pinned, ageDays(scored[j].m.CreatedAt, now))
			if fi != fj {
				return fi > fj
			}
			return scored[i].m.ID < scored[j].m.ID
		})
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]Memory, len(scored))
	for i, s := range scored {
		out[i] = s.m
	}
	return out
}

// ageDays returns a memory's age in days from its created_at, clamped at 0
// (a future timestamp is treated as brand-new).
func ageDays(createdAt string, now time.Time) float64 {
	ageDays := now.Sub(parseCreatedAt(createdAt)).Hours() / 24.0
	if ageDays < 0 {
		return 0
	}
	return ageDays
}

// fuseAndRank runs the shared selection pipeline for hybrid and FTS-only
// searches. Window selection — fusion, scope narrowing and membership — lives in
// FuseAndSelectWindow and the unexported selectWindow it delegates to; this
// function materializes that window, hydrates it, ranks it, and applies final
// demotion.
func (s *Store) fuseAndRank(ctx context.Context, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
	// Fuse once, narrow scope, then cut the window. The same narrowed pool feeds
	// the hydration backfill below, which is why narrowing happens here rather
	// than inside the selection seam alone.
	pool := scopeEligiblePool(fuseCandidatePool(ftsResults, vecResults, p), p)
	window := selectWindow(pool, limit, p)

	// Hydrate the full candidate pool before ranking — decayRank needs
	// category/pinned/created_at to reorder the window, and hydration via
	// GetByIDs does not preserve order, so the final sort lives there too.
	poolIDs := make([]string, len(pool))
	if window.Scores == nil {
		window.Scores = make(map[string]float64, len(pool))
	}
	for i, candidate := range pool {
		poolIDs[i] = candidate.id
		if _, ok := window.Scores[candidate.id]; !ok {
			window.Scores[candidate.id] = candidate.score
		}
	}
	if len(poolIDs) == 0 {
		poolIDs = window.IDs
	}

	// Hydrate before ranking — decayRank needs category/pinned/created_at to
	// reorder the window, and hydration via GetByIDs does not preserve order, so
	// the final sort lives there too.
	//
	// The leg results are NOT a substitute for this read, on either path. They
	// are the snapshot an earlier statement returned, and a concurrent writer can
	// delete or update a row before this function returns. Reading by id is what
	// makes a vanished window ID disappear so selectHydratedWindow can backfill
	// the next candidate; taking the leg's copy instead would emit a row that no
	// longer exists, with stale category, pinned and created_at feeding decay.
	beforeHybridHydrate(window.IDs)

	// Re-read the window first, which is all the common case needs. The wider pool
	// is only consulted when a window ID is genuinely missing, so the deleted-row
	// backfill still has somewhere to draw from without every keyword-only search
	// paying for a limit*2-row read it will not use.
	hydrated, err := s.GetByIDs(ctx, window.IDs)
	if err != nil {
		return nil, err
	}
	if len(window.IDs) > 0 && len(hydrated) < len(window.IDs) {
		pool, err := s.GetByIDs(ctx, poolIDs)
		if err != nil {
			return nil, err
		}
		hydrated = pool
	}
	memories := selectHydratedWindow(hydrated, window, poolIDs)
	memories = decayRank(memories, window.Scores, p, limit, time.Now().UTC())
	return s.demoteResults(ctx, memories, p), nil
}

func selectHydratedWindow(hydrated []Memory, window HybridWindow, poolIDs []string) []Memory {
	byID := make(map[string]Memory, len(hydrated))
	for _, memory := range hydrated {
		byID[memory.ID] = memory
	}
	selected := make([]Memory, 0, len(window.IDs))
	seen := make(map[string]bool, len(window.IDs))
	for _, id := range window.IDs {
		if memory, ok := byID[id]; ok {
			selected = append(selected, memory)
			seen[id] = true
		}
	}
	if len(selected) == len(window.IDs) {
		return selected
	}
	for _, id := range poolIDs {
		if len(selected) == len(window.IDs) {
			break
		}
		if seen[id] {
			continue
		}
		if memory, ok := byID[id]; ok {
			selected = append(selected, memory)
			seen[id] = true
		}
	}
	return selected
}

// HybridWindow is the result of hybrid fusion and window selection: the memory
// ids eligible to be returned, in ranked order, with the fused score each was
// selected on.
type HybridWindow struct {
	IDs    []string
	Scores map[string]float64
}

func fuseCandidatePool(ftsResults []Memory, vecResults []ScoredMemory, p SearchParams) []*hybridCandidate {
	byID := make(map[string]*hybridCandidate, len(ftsResults)+len(vecResults))
	get := func(id string) *hybridCandidate {
		c, ok := byID[id]
		if !ok {
			c = &hybridCandidate{id: id}
			byID[id] = c
		}
		return c
	}
	for rank, memory := range ftsResults {
		candidate := get(memory.ID)
		candidate.fts = rank + 1
		candidate.scope = memory.Scope
		candidate.projectID = memory.ProjectID
		candidate.resolved = memory.ResolvedAt != nil
		candidate.score += p.FTSWeight / float64(p.RRFK+rank+1)
		if t := p.trace.row(memory.ID); t != nil {
			// The 0-based rank, recorded where fusion read the leg's order —
			// the same order RRF awarded from, so the two cannot disagree.
			t.FTSRank = rank
		}
	}
	for rank, scored := range vecResults {
		candidate := get(scored.MemoryID)
		candidate.vec = rank + 1
		if t := p.trace.row(scored.MemoryID); t != nil {
			t.VectorRank = rank
			t.VectorScore = float64(scored.Score)
		}
		// Only fill in scope from the vector leg when the keyword leg did not
		// supply it: both describe the same row, so they agree, and a nil map
		// from either leg is a row that genuinely has no scope.
		if candidate.scope == nil {
			candidate.scope = scored.Scope
		}
		// Status follows the same rule — except that resolved only ever turns
		// on, so a row both legs saw is demoted even if one leg's snapshot
		// predates the stamp. A vector-only candidate gets its status here,
		// which is the whole reason the vector leg carries it.
		if candidate.projectID == "" {
			candidate.projectID = scored.ProjectID
		}
		candidate.resolved = candidate.resolved || scored.Resolved
		candidate.score += p.VecWeight / float64(p.RRFK+rank+1)
	}

	pool := make([]*hybridCandidate, 0, len(byID))
	for _, candidate := range byID {
		pool = append(pool, candidate)
	}
	// Status demotion runs here — on the fused score, before the sort and
	// therefore before the cut — because it has to own membership too: a live
	// project memory that only a demoted row was keeping out of the window
	// must be able to take that slot. Applied later it would reorder rows
	// that were already selected and change nothing about who got in.
	demoteStatus(pool, p)

	// Deterministic order: map iteration is randomized, and an unstable sort
	// over tied scores made result order — and the demotion penalties that
	// depend on it — vary between runs of the same query.
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].id < pool[j].id
	})
	return pool
}

// scopeEligiblePool drops the candidates the requested scope excludes. It runs
// before the cut so an eligible candidate can backfill a row scope removed, and
// both the window and fuseAndRank's hydration backfill read its result, so an
// out-of-scope row can neither take a result slot nor stand in for a selected
// row that vanished. ScopeMatches keeps rows that do not mention a requested
// key eligible; silence is not disagreement.
func scopeEligiblePool(pool []*hybridCandidate, p SearchParams) []*hybridCandidate {
	if len(p.Scope) == 0 {
		return pool
	}
	eligible := pool[:0]
	for _, c := range pool {
		matched := ScopeMatches(c.scope, p.Scope)
		if matched {
			eligible = append(eligible, c)
		}
		// Recorded for the DROPPED candidates too, not just the survivors. A
		// verdict only for the rows that passed would leave explain with no way
		// to say why one was excluded — the #571 failure — and it would have to
		// call ScopeMatches itself to recover it, which is the parallel
		// re-derivation this trace exists to remove.
		if t := p.trace.row(c.id); t != nil {
			t.ScopeMatched = matched
		}
	}
	return eligible
}

// FuseAndSelectWindow owns both halves of hybrid retrieval — fusing the
// keyword and vector legs into one ranking, and deciding which memories form
// the result window — in one place, because window selection is not separable
// from fusion. Scope constraints are narrowed from the combined candidate pool
// before the cut, so eligible rows can backfill candidates excluded by scope.
//
// Selecting by fused score alone is what made the window unable to admit a
// keyword-only hit. RRF weights the vector leg 0.7 and the keyword leg 0.3, so
// with k=60 the keyword leg's rank-1 row scores 0.3/61 ≈ 0.0049, while the
// vector leg's 20th row — still comfortably inside the fetched window — scores
// 0.7/80 ≈ 0.0088. A full vector leg therefore outranked the best keyword
// match, every time, and no amount of exact identifier matching could put that
// memory in the results. That is the case FTS exists for.
//
// So the window reserves slots for the top keyword hits. Reserving is the
// narrowest rule that repairs it: a reserved hit already in the window keeps
// its place, and one the score cut would have dropped is promoted in its
// stead. It does not invert the ranking — a memory matching both legs
// accumulates both weighted contributions and still outranks a keyword-only
// row, because the reservation only guarantees admission, never a position.
//
// The scores being cut here are already status-demoted: fusion multiplies a
// resolved row's and a project-scoped _global row's contribution by
// statusDemotionFactor before this seam runs, so admission is decided on the
// demoted score too.
//
// The returned width is `limit`, except under DecayReselect, where decay
// narrows the set afterwards and therefore still needs the wider pool it has
// always been given. Either way this is the eligible set, and decayRank orders
// and trims within it.
func FuseAndSelectWindow(ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) HybridWindow {
	return selectWindow(scopeEligiblePool(fuseCandidatePool(ftsResults, vecResults, p), p), limit, p)
}

// selectWindow cuts an already-fused, already-narrowed candidate pool down to
// the result window. It is the same seam fuseAndRank uses once it holds that
// pool, so a candidate excluded by scope cannot re-enter through the backfill.
//
// It reorders the slice it is handed — the reservation promotes keyword hits in
// place — so it cuts a copy, leaving the caller's pool in fused-score order for
// use as a hydration backfill source.
func selectWindow(pool []*hybridCandidate, limit int, p SearchParams) HybridWindow {
	width := limit
	if p.DecayReselect && p.DecayEnabled {
		width = limit * 2
	}
	if width <= 0 {
		return HybridWindow{}
	}
	pool = append([]*hybridCandidate(nil), pool...)
	// Reserve slots for the top keyword hits: the best `limit/5` of them are
	// guaranteed a place in the window even when the vector leg would have
	// filled every row. A fifth of the window, and only when that is at least
	// one — below a window of five, a reservation would be a majority of the
	// answer rather than a correction to it.
	//
	// Admission is the whole of it, and the position stays the fused score's
	// to decide. Reserving is also gated on status: the reservation reads raw
	// FTS rank while demotion writes the fused score, so without the gate a
	// _global or resolved row that led the keyword leg took a window slot
	// back from the live project row the demoted score had given it — the
	// factor owns membership, and the reservation would have overridden that
	// membership decision while leaving the halved scores untouched (the
	// slice is re-sorted by score right after the loop, so a reserved demoted
	// row sat at the bottom of the window, not in front of anything). A row
	// whose status factor is below 1 is therefore never reserved: it competes
	// for the window on its demoted score alone. The consequence is the
	// deliberate trade of the fix, documented in docs/architecture.md: where
	// the #543 repair rescues an undemoted keyword-only hit from the score
	// cut the weights cause, a demoted one is not rescued — it comes back
	// when it makes that cut or the window has room, and drops out of a full
	// window two ways: enough rows outscore it, or the reservation's
	// score-blind eviction hands its slot to a top-limit/5 keyword hit
	// scoring below it (eviction targets the weakest admitted row that is
	// not reserved, and a demoted row never is).
	//
	// Two stronger interventions were built and measured against
	// the built-in dataset before settling here:
	//
	//   - Reordering the returned slice so reserved hits lead the window.
	//     decayRank re-sorts by fused score on the way out, so this changes
	//     nothing beyond admission: the hit reappeared at the bottom.
	//   - Flooring a reserved hit's score. The top is far too strong — hybrid
	//     R@1 0.507 -> 0.366, NDCG@10 0.812 -> 0.738. The median improves
	//     R@10 and NDCG but is a score, and decay multiplies scores by a
	//     category- and age-dependent factor, so a reserved hit parked a hair
	//     above the median gets reordered by decay and the invariant that
	//     uniform timestamps leave the graded ranking untouched
	//     (TestDecayDoesNotPerturbGradedBench) stops holding. Every margin big
	//     enough to survive that spread exceeds the catastrophic top floor.
	//
	// What the issue describes is admission, and admission is what this does.
	if slots := limit / 5; p.FTSWeight > 0 && slots > 0 && len(pool) > width {
		isReserved := func(c *hybridCandidate) bool {
			if statusDemotionFactor(c.resolved, c.projectID, p.ProjectID) < 1 {
				return false
			}
			return c.vec == 0 && c.fts > 0 && c.fts <= slots
		}
		admitted := make(map[string]bool, width)
		for _, c := range pool[:width] {
			admitted[c.id] = true
		}
		for _, c := range pool {
			if !isReserved(c) || admitted[c.id] {
				continue
			}
			// Evict the weakest admitted row that is not itself reserved.
			for i := width - 1; i >= 0; i-- {
				if !isReserved(pool[i]) {
					// The reservation is the one admission decision a reader
					// cannot reconstruct from a score, so both sides of it are
					// recorded: the row that was promoted, and the row whose
					// slot it took. Without the displaced id, "why is this
					// keyword hit in the answer" has no answer, because its score
					// is by construction below the cut.
					if t := p.trace.row(c.id); t != nil {
						t.KeywordReserved = true
						t.TookSlotFrom = pool[i].id
					}
					if t := p.trace.row(pool[i].id); t != nil {
						t.DisplacedBy = c.id
					}
					pool[i] = c
					break
				}
			}
			admitted[c.id] = true
		}
		// The evicted row may have been stronger than its replacement.
		sort.Slice(pool[:width], func(i, j int) bool {
			if pool[i].score != pool[j].score {
				return pool[i].score > pool[j].score
			}
			return pool[i].id < pool[j].id
		})
	}

	// The cut, once the reservation has had its say. It belongs out here
	// rather than inside the reservation: a limit too small to reserve
	// anything (below five) still has to return a window, not the pool.
	if len(pool) > width {
		pool = pool[:width]
	}

	return hybridWindowOf(pool)
}

// hybridCandidate is one memory's fused standing: the rank each leg gave it
// and the score those ranks produced. fts or vec is 0 when that leg did not
// retrieve it, which is what distinguishes a two-leg hit from a keyword-only
// one. scope is the row's own scope, carried by whichever leg retrieved it, so
// selection can narrow the pool without a second lookup; projectID and
// resolved are carried the same way for status demotion.
type hybridCandidate struct {
	id        string
	fts       int
	vec       int
	score     float64
	scope     map[string]string
	projectID string
	resolved  bool
}

// hybridWindowOf materialises the selected candidates, keeping the fused score
// alongside each id for decayRank.
func hybridWindowOf(cands []*hybridCandidate) HybridWindow {
	w := HybridWindow{IDs: make([]string, 0, len(cands)), Scores: make(map[string]float64, len(cands))}
	for _, c := range cands {
		w.IDs = append(w.IDs, c.id)
		w.Scores[c.id] = c.score
	}
	return w
}

// SearchHybrid combines FTS5 keyword search with vector similarity using
// Reciprocal Rank Fusion (RRF). Falls back to FTS-only if queryVec is nil.
func (s *Store) SearchHybrid(ctx context.Context, projectID, query string, queryVec []float32, limit int) ([]Memory, error) {
	return s.SearchHybridScoped(ctx, projectID, query, queryVec, limit, nil)
}

// SearchHybridScoped is the production search entry point with a scope
// constraint. It deliberately builds the parameters here rather than accepting
// them from MCP so the configured vector similarity floor cannot be bypassed.
func (s *Store) SearchHybridScoped(ctx context.Context, projectID, query string, queryVec []float32, limit int, scope map[string]string) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	p.Scope = scope
	return s.SearchHybridParams(ctx, projectID, query, queryVec, limit, p)
}

// SearchHybridParams is SearchHybrid with explicit fusion parameters. It is
// used by the benchmark harness and by SearchHybridScoped after the store has
// assembled production parameters.
func (s *Store) SearchHybridParams(ctx context.Context, projectID, query string, queryVec []float32, limit int, p SearchParams) ([]Memory, error) {
	final, _, err := s.searchHybridLegs(ctx, projectID, query, queryVec, limit, p)
	return final, err
}

// hybridLegs is the raw output of each retrieval leg. vec is the leg before the
// similarity floor, because a candidate the floor dropped is a distinct,
// diagnosable outcome that is invisible once the floor has been applied.
type hybridLegs struct {
	fts []Memory
	vec []ScoredMemory
}

// searchHybridLegs is SearchHybridParams plus the legs it already fetched.
//
// Explain mode needs those exact rows — its whole job is to say why the search
// ranked what it ranked — and re-running each leg inside explain's snapshot
// transaction would hold the store's single connection across a second FTS
// scan and a second full embedding scan. The store runs with one connection, so
// every statement inside that transaction is time a concurrent save, touch or
// background write cannot have the connection at all.
func (s *Store) searchHybridLegs(ctx context.Context, projectID, query string, queryVec []float32, limit int, p SearchParams) ([]Memory, hybridLegs, error) {
	// The search's own project is what makes a _global row a candidate for
	// status demotion, so it is stamped here, once, for every exit below —
	// including the FTS-only fallbacks and explain, which reaches this
	// function too.
	p.ProjectID = projectID

	// FTS results.
	ftsResults, err := s.SearchFTS(ctx, projectID, query, limit*2)
	if err != nil {
		ftsResults = nil // non-fatal, proceed with vector only
	}
	legs := hybridLegs{fts: ftsResults}

	// FTS-only is the same selection seam with an empty vector leg. Use an
	// unweighted keyword score to preserve the historical FTS-only ordering
	// and explain-mode score contract.
	if queryVec == nil {
		final, err := s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
		return final, legs, err
	}

	// Vector results.
	vecResults, err := s.SearchVector(ctx, projectID, queryVec, limit*2)
	if err != nil {
		vecResults = nil // non-fatal, proceed with FTS only
	}
	legs.vec = vecResults
	filtered := filterVectorFloor(vecResults, p.MinSimilarity)
	// A candidate the floor removed never reaches fusion, so nothing downstream
	// can record why it is absent. It is a distinct, diagnosable outcome — "your
	// query matched nothing strongly enough" — and it is invisible if only the
	// surviving leg is kept, so the floor's own verdict is stamped here, at the
	// only place that applies it. Explained, not asserted: explain reports
	// whether the row was floor-dropped rather than inferring it from a missing
	// vector rank, which is indistinguishable from a leg that never matched.
	if p.trace != nil && len(filtered) < len(vecResults) {
		kept := make(map[string]bool, len(filtered))
		for _, v := range filtered {
			kept[v.MemoryID] = true
		}
		for _, v := range vecResults {
			if kept[v.MemoryID] {
				continue
			}
			if t := p.trace.row(v.MemoryID); t != nil {
				t.FloorDropped = true
				t.FloorScore = float64(v.Score)
				// The project travels with it: the row belongs to one whether or
				// not it was eligible, and a floor-dropped row reporting no
				// project at all would be indistinguishable from a row the legs
				// never attributed to one.
				t.RowProject = v.ProjectID
				t.ProjectMatch = p.ProjectID == "" || v.ProjectID == p.ProjectID
				// StatusFactor comes from the same function demoteStatus uses
				// rather than a hardcoded 1.0, because 1.0 beside
				// project_match=false is the one combination this row cannot be: a
				// shared row in a project search IS status-demoted, it simply never
				// got far enough for the demotion to run. Reporting the factor that
				// applies to the row is what a reader needs; "nothing was done" is
				// true of the scoring and misleading about the row.
				t.StatusFactor = statusDemotionFactor(v.Resolved, v.ProjectID, p.ProjectID)
			}
		}
	}

	// If only FTS worked, return that through the same selection seam.
	if len(filtered) == 0 {
		final, err := s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
		return final, legs, err
	}

	final, err := s.fuseAndRank(ctx, ftsResults, filtered, limit, p)
	return final, legs, err
}

func keywordOnlyParams(p SearchParams) SearchParams {
	p.FTSWeight = 1
	p.VecWeight = 0
	return p
}

// getByIDsChunk bounds the placeholder list in one GetByIDs statement. SQLite
// caps a statement's variables, and the cap is a property of the build rather
// than of anything Ghost controls: 32766 on the version this tree links, 999 on
// older ones. 500 sits inside every cap there is, so one number serves all of
// them, and a window that needs 32766 ids costs 66 statements on a handle that
// already serves the whole corpus from memory.
const getByIDsChunk = 500

// GetByIDs fetches memories by a list of IDs.
//
// The ids are asked for in chunks of getByIDsChunk, because one statement's
// placeholder list is bounded by SQLite's variable limit and a caller that
// passed more ids than that got an error rather than rows. The callers are the
// candidate hydration (a window and its backfill pool) and the supersedes-link
// read, so the width is the caller's fetch limit rather than anything chosen
// here — and a hydration that errors is a retrieval that returns nothing, which
// reads to a caller as "this project has no memories" rather than as a failure.
//
// One RLock spans the whole set and the rows are appended in chunk order, so
// the result is a superset of any single chunk and never a subset: a row
// written between two chunks appears or does not, exactly as it would have
// between two rows of one unchunked statement, since neither form reads a
// snapshot. The order is not the order of ids and never was — the query has no
// ORDER BY — so chunking does not change it into something callers can rely on.
func (s *Store) GetByIDs(ctx context.Context, ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	const columns = `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope, valid_from, valid_until, verified_at
		FROM memories
		WHERE id IN (%s)`

	var out []Memory
	for start := 0; start < len(ids); start += getByIDsChunk {
		end := min(start+getByIDsChunk, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.queryDB().QueryContext(ctx, fmt.Sprintf(columns, strings.Join(placeholders, ",")), args...)
		if err != nil {
			return nil, fmt.Errorf("get by ids: %w", err)
		}
		found, err := scanMemories(rows)
		rows.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("get by ids: %w", err)
		}
		out = append(out, found...)
	}
	return out, nil
}

// searchVectorAllLeg is the cross-project vector leg, with its facts, mirroring
// searchVectorLeg without the project predicate.
func (s *Store) searchVectorAllLeg(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, vectorLegFacts, error) {
	// Read the identity before taking the lock, as searchVectorLeg does.
	identity := s.configuredEmbeddingIdentity()

	rows := s.borrowVectorRows()
	defer s.returnVectorRows(rows)

	// The same snapshot, the same scoring pass and the same bounded window as
	// the project leg — the only difference is the query, which here has no
	// WHERE clause at all.
	if err := s.snapshotVectors(ctx, vectorScanColumns, nil, queryVec, identity, rows); err != nil {
		return nil, vectorLegFacts{}, err
	}
	return rows.search(queryVec, limit, nil), rows.facts, nil
}

// SearchVectorAll performs brute-force cosine similarity search across ALL projects.
func (s *Store) SearchVectorAll(ctx context.Context, queryVec []float32, limit int) ([]ScoredMemory, error) {
	rows, _, err := s.searchVectorAllLeg(ctx, queryVec, limit)
	return rows, err
}

// SearchHybridAll combines FTS5 and vector search across ALL projects using RRF.
// Falls back to FTS-only when queryVec is nil.
//
// p.ProjectID stays empty by design: a cross-project search has no project
// whose own memories a _global row could be padding, so status demotion
// applies to resolved rows here but never to global ones.
func (s *Store) SearchHybridAll(ctx context.Context, query string, queryVec []float32, limit int) ([]Memory, error) {
	p := DefaultSearchParams()
	p.MinSimilarity = s.vectorMinSimilarityFloor()
	ftsResults, err := s.SearchFTSAll(ctx, query, limit*2)
	if err != nil {
		ftsResults = nil
	}

	// The FTS-only fallbacks below must use the same selection and supersede
	// path as hybrid search — otherwise cross-project search silently ranks
	// superseded memories above their replacements whenever Ollama is down.
	if queryVec == nil {
		return s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
	}

	vecResults, err := s.SearchVectorAll(ctx, queryVec, limit*2)
	if err != nil {
		vecResults = nil
	}
	vecResults = filterVectorFloor(vecResults, p.MinSimilarity)

	if len(vecResults) == 0 {
		return s.fuseAndRank(ctx, ftsResults, nil, limit, keywordOnlyParams(p))
	}

	return s.fuseAndRank(ctx, ftsResults, vecResults, limit, p)
}

func float32sToBytes(fs []float32) []byte {
	buf := make([]byte, len(fs)*4)
	for i, f := range fs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func bytesToFloat32s(b []byte) []float32 {
	n := len(b) / 4
	fs := make([]float32, n)
	for i := range n {
		fs[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return fs
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}
