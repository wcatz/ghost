package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// vectorScanColumns is the projection both vector searches read: a candidate's
// id, its embedding, the vector space that embedding was written in, the three
// status fields fusion applies to the returned row, the memory's validity
// window, plus the memory's rowid (an 8-byte integer, not a copied column) that
// breaks exact ties. Nothing else — a search that read a column it does not
// return would be paying the single connection for bytes it throws away.
//
// valid_from/valid_until are read so the vector leg can withhold a row outside
// its window BEFORE the top-k cut, the mirror of the keyword leg's predicate
// (#1053): a query whose limit is small must not spend its vector depth on rows
// stage 2 will drop. They decide membership during the scan and are not copied
// into the result.
//
// m.scope is coalesced to an empty string so the column is never NULL.
// database/sql assigns nil to a []byte destination it is handed for a NULL
// column, which would drop the snapshot's scope buffer (and its reuse with it)
// on every unscoped memory; an empty string means no scope exactly as NULL does,
// so the coalesce changes no answer — see parseScopeJSON.
const vectorScanColumns = `
	SELECT e.memory_id, e.embedding, e.model, COALESCE(m.scope, ''), m.project_id, m.resolved_at, m.rowid, m.valid_from, m.valid_until
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id`

// vectorProjectPredicate is the project leg's narrowing: the project and the
// global project (a project's own search always includes _global). It is one
// constant because the scan below and the uncached path's row-count hint (see
// vectorCountQuery) have to name the same rows — a change to one has to be a
// change to the other. It is parenthesized because vectorCountQuery ANDs the
// loader's filters onto it: without the parentheses `a = ? OR b = ? AND c` binds
// as `a = ? OR (b = ? AND c)`, and the filter would apply only to the second
// branch of the OR.
const vectorProjectPredicate = `(m.project_id = ? OR m.project_id = '_global')`

// vectorProjectScanQuery is the project leg's corpus query: the shared
// projection narrowed by vectorProjectPredicate.
const vectorProjectScanQuery = vectorScanColumns + `
	WHERE ` + vectorProjectPredicate

// vectorCountFrom is the row set the uncached path's count sizes the snapshot
// buffer from (see vectorCountQuery): the same join vectorScanColumns reads.
const vectorCountFrom = `
	SELECT COUNT(*)
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id`

// vectorCountQuery builds the count that sizes an uncached snapshot's embedding
// buffer. It names the rows loadVectorRows can actually copy: the join the scan
// reads, the leg's own narrowing (projectWhere/projectArgs — empty for the
// cross-project leg), and the loader's two filters, the configured identity when
// there is one and the query's width. Without the last two the count is every
// row of the leg, so a corpus mid-re-embed — most rows foreign or of another
// width — would reserve a corpus-sized buffer for rows the copy skips, which is
// the allocation the identity filter exists to avoid. The hint is then an upper
// bound on the appended bytes rather than a guess at them; reserve is correct
// either way, only tighter with the filters.
func vectorCountQuery(projectWhere string, projectArgs []any, identity string, dims int) (string, []any) {
	where := make([]string, 0, 3)
	args := append([]any(nil), projectArgs...)
	if projectWhere != "" {
		where = append(where, projectWhere)
	}
	if identity != "" {
		where = append(where, "e.model = ?")
		args = append(args, identity)
	}
	where = append(where, "length(e.embedding)/4 = ?")
	args = append(args, dims)
	return vectorCountFrom + " WHERE " + strings.Join(where, " AND "), args
}

// queryVectorCorpus runs the corpus cache's query: the shared projection with no
// predicate, so one copy serves every project. It lives beside the constant it
// runs for the reason the structural write scan reads statements — a name bound
// to a name in another file cannot be resolved there, and an unresolved
// statement is reported as a write.
func queryVectorCorpus(ctx context.Context, q Queryer) (*sql.Rows, error) {
	return q.QueryContext(ctx, vectorScanColumns)
}

// duringVectorCopyFn and duringVectorScoreFn are test seams inside a vector
// search, one in each of its two phases (the scoring seam fires once, after the first
// row is scored). Production leaves both no-ops. A test
// parks in one of them to find out what the search is and is not holding at that
// point, which is the property #556 is about and the only way to assert it
// without measuring a duration.
//
// They have to be two seams rather than one. A search reads the store's
// configured embedding identity under a read lock before either phase begins, so
// with a single seam there is no way to tell "the copy holds the read lock" from
// "the search has not got past the identity read yet" — a test that parked in
// the one seam and found a writer blocked could not say which of the two it had
// caught.
var (
	duringVectorCopyFn  atomic.Value // func()
	duringVectorScoreFn atomic.Value // func()
)

func init() {
	duringVectorCopyFn.Store(func() {})
	duringVectorScoreFn.Store(func() {})
}

func duringVectorCopy() {
	fn, _ := duringVectorCopyFn.Load().(func())
	fn()
}

func duringVectorScore() {
	fn, _ := duringVectorScoreFn.Load().(func())
	fn()
}

// vecSpan locates one row's bytes inside one of vectorRows' column buffers.
// Offsets rather than slices, so a row costs a fixed amount of bookkeeping and
// a buffer can be reallocated without any row reference going stale.
type vecSpan struct{ off, n int }

// vectorRowSpan is one usable row of a vector search's snapshot.
type vectorRowSpan struct {
	id, scope, project, embed vecSpan
	// validFrom and validUntil locate the row's stored validity bounds, when it
	// states them. They are read so the scan can withhold an out-of-window row
	// before the cut (#1053); a span of length 0 is a NULL or empty bound, which
	// is no claim, exactly as ValidityState reads it.
	validFrom, validUntil vecSpan
	// resolved is the row's own resolved_at, read here so fusion can demote it
	// without a second lookup per candidate.
	resolved bool
	// seq is the memory's rowid, its creation order, which breaks exact ties.
	seq int64
}

// vecCandidate is one row competing for the result window, held as a score and a
// row index rather than as a ScoredMemory. The strings and the scope map a
// result carries are materialized only for the rows that actually win a slot,
// which is what keeps a search from allocating for the whole corpus.
type vecCandidate struct {
	score float32
	row   int
	// seq is the memory's creation order (its rowid), the tiebreak below.
	seq int64
}

// betterThan reports whether a outranks b: the higher cosine wins, and on an
// exact tie the older memory (lower rowid) wins.
//
// The tiebreak is what makes the bounded top-k a total order. Without one, two
// rows with equal float32 cosines have no order between them, and a bounded
// heap hands back whichever of them happened to reach the cut last. Ordering by
// (score, creation order) makes the returned rows a function of the stored data
// alone, whatever order the database yielded them in (the project index, the
// embeddings scan the corpus cache uses, a VACUUM), so the cached and uncached
// paths agree on ties too.
func (c vecCandidate) betterThan(o vecCandidate) bool {
	return compareCandidates(c, o) < 0
}

// worseThan is betterThan reversed, and is the heap's own ordering: the root of
// a top-k heap is the worst row it holds, so a candidate that cannot beat the
// root is out.
func (c vecCandidate) worseThan(o vecCandidate) bool {
	return compareCandidates(c, o) > 0
}

func compareCandidates(a, b vecCandidate) int {
	switch {
	case a.score > b.score:
		return -1
	case a.score < b.score:
		return 1
	}
	// Exact ties order by creation order (the memory's rowid: older first), so the
	// answer does not depend on the order the database yielded the rows in: the
	// project index and the embeddings scan agree. Rowids are unique; the row is a
	// last resort.
	if a.seq != b.seq {
		if a.seq < b.seq {
			return -1
		}
		return 1
	}
	if a.row < b.row {
		return -1
	}
	return 1
}

// vectorRows is one vector search's private copy of the rows it may score.
//
// The rows are copied out of SQLite while the store's read lock and its single
// connection are held, and scored only after both are released
// (Store.snapshotVectors). Scoring is the O(corpus × dims) half of a
// brute-force search, and holding a read lock across it blocks every writer on
// the store for the length of the whole corpus — which matters most for
// `ghost supersede`, that runs this search once per memory.
//
// The bytes are kept exactly as they came out of the database rather than
// decoded. A decoded vector is a fresh []float32 per row, and at 768
// dimensions that is 3 KiB of throwaway allocation for every memory in the
// corpus on every query; the stored form is already what the score loop needs,
// because float32sToBytes wrote little-endian float32s, so the dot product can
// read them back out of the blob in place (cosineFromBytes). The scope column
// is kept as text for the same reason: parsing it produces a map per row, and
// only a row that wins a result slot needs one.
//
// A vectorRows is recycled (see Store.borrowVectorRows), so the snapshot itself
// is not re-allocated per query: a column buffer with room left from the last
// search is appended to in place, rather than being sized to the corpus and
// thrown away every time. The driver still hands over one value per column per
// row, and that is the allocation a query cannot avoid — see the note on
// maxRetainedVectorBytes.
type vectorRows struct {
	// Column buffers, each holding one column of every usable row concatenated
	// in scan order. Only the embedding column is large; ids, scopes, projects
	// and validity bounds are tens of bytes per row.
	ids, scopes, projects, embeds []byte
	// validFroms and validUntils hold the validity bounds, in the same order, so
	// the scoring pass can withhold an out-of-window row before the cut. They are
	// kept as stored text rather than parsed: the common row states no window, so
	// parsing every row would be pure waste for a decision only a dated row makes.
	validFroms, validUntils []byte
	// rows locates each row's bytes inside those buffers.
	rows []vectorRowSpan
	// cands is the bounded top-k for the project bucket, reused so a search
	// does not allocate a heap per query.
	cands []vecCandidate
	// globalCands is the bounded top-k for the shared _global bucket, used when
	// splitGlobal is set so a project's own rows cannot be crowded out of the
	// candidate set by shared rows before the status demotion runs (#1771).
	globalCands []vecCandidate
	// splitGlobal, when set, keeps the shared _global bucket under its own top-k
	// so the project's own rows stay candidates (#1771). Cleared by reset().
	splitGlobal bool
	// now is the clock the validity window is judged against. The zero time is
	// "do not filter", which is every read that does not ask for it (the exported
	// vector wrappers); the query path sets it to its request's Now.
	now time.Time
	// dest is the scan destination, reused for every row. It lives here rather
	// than in the read loop because database/sql takes each destination as an
	// any, which moves a loop-local to the heap — six allocations for every row
	// of the corpus, which is a sixth of the whole per-query cost.
	dest vectorScanDest
	// facts is what this scan could not use: rows it skipped for a recorded
	// identity from another vector space, and rows whose width does not match the
	// query. It lives on the snapshot rather than in a return value because the
	// snapshot is already the thing every vector leg borrows, and a caller that
	// wants the facts (a leg deciding whether its silence means "nothing matched"
	// or "rows were skipped") reads them from the one scan rather than paying
	// for a second pass over the embeddings.
	facts vectorLegFacts
}

// vectorScanDest holds one row as the driver delivered it. The values are only
// valid until the next Next or Scan, which is why the loader copies them into
// the column buffers before it moves on.
type vectorScanDest struct {
	id, embed, model, scope, project, resolvedAt sql.RawBytes
	validFrom, validUntil                        sql.RawBytes
	seq                                          int64
}

func (v *vectorRows) reset() {
	v.ids = v.ids[:0]
	v.scopes = v.scopes[:0]
	v.projects = v.projects[:0]
	v.embeds = v.embeds[:0]
	v.validFroms = v.validFroms[:0]
	v.validUntils = v.validUntils[:0]
	v.rows = v.rows[:0]
	v.cands = v.cands[:0]
	v.globalCands = v.globalCands[:0]
	// The clock belongs to the search that set it, and the snapshot is pooled
	// across queries, so a stale one would filter this query against another's
	// Now — or, worse, keep filtering a browse read that asked for no filter.
	v.now = time.Time{}
	v.splitGlobal = false
	// The counts belong to the scan that set them, and the snapshot is pooled
	// across queries, so a stale one would be read as this query's.
	v.facts = vectorLegFacts{}
}

func (v *vectorRows) column(buf []byte, sp vecSpan) []byte { return buf[sp.off : sp.off+sp.n] }

func (v *vectorRows) embed(sp vecSpan) []byte { return v.embeds[sp.off : sp.off+sp.n] }

// reserve sizes the snapshot's embedding buffer for rows rows of dims float32s.
//
// It is the grow-in-place half of an uncached scan. A corpus above the pool's
// retention cap is dropped on return (vectorScratch.put), so the next search
// starts from an empty buffer; growing it by repeated append reallocates and
// copies the corpus several times over before it reaches full size. Sizing it
// once from the row count the database already knows replaces that with a single
// allocation of the exact size.
//
// The hint is an upper bound on what the loader will append, not the amount: it
// is the count of rows in the same table the copy reads, and the loader appends
// only rows of the query's width, so it can never under-size and a too-small
// hint stays correct — merely back to append growth. It only ever grows: a
// recycled buffer that already has room is left alone, and a smaller hint does
// not shrink it.
func (v *vectorRows) reserve(rows int64, dims int) {
	if rows <= 0 || dims <= 0 {
		return
	}
	need := rows * int64(dims) * 4
	if need <= 0 || int64(cap(v.embeds)) >= need {
		return
	}
	v.embeds = make([]byte, 0, need)
}

// snapshotVectors copies the candidate rows out of SQLite and returns with the
// store's read lock and its single connection released, so the caller can score
// them without either.
//
// Both are held for the copy only, because the copy is the only part that needs
// them: the cosine pass over the rows it produced does not read the database
// again. The connection is the binding constraint either way — OpenDB pins the
// pool at one, so a write cannot proceed while a query is streaming regardless
// of the mutex — but the lock still says what the statement is, and every other
// reader in this package takes it. query is the caller's own SQL (the
// project-scoped leg and the cross-project leg differ only in their WHERE
// clause) and is run against queryDB, so the snapshot store Candidates builds still
// reads its transaction.
//
// countWhere and countArgs, when non-empty, are the leg's own narrowing of the
// corpus (the project predicate, or nothing for the cross-project leg). The
// count that sizes the embedding buffer is built from them plus the loader's own
// filters — the configured identity and the query's width — so it names exactly
// the rows the copy below can append (vectorCountQuery). It is run first, under
// the same lock and on the same handle as the copy, so it describes the rows
// about to be read, and sizes the buffer before the copy (vectorRows.reserve):
// a corpus above the pool's retention cap is dropped on return and would
// otherwise be regrown from zero by append growth every search. A failed count
// is not an error: it only means the copy falls back to that growth.
func (s *Store) snapshotVectors(ctx context.Context, query string, args []any, countWhere string, countArgs []any, queryVec []float32, identity string, v *vectorRows) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	countQuery, countArgs := vectorCountQuery(countWhere, countArgs, identity, len(queryVec))
	var n int64
	if err := s.queryDB().QueryRowContext(ctx, countQuery, countArgs...).Scan(&n); err == nil {
		v.reserve(n, len(queryVec))
	}

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("load embeddings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return s.loadVectorRows(v, rows, queryVec, identity)
}

// loadVectorRows reads an open embeddings query into the snapshot, keeping only
// the rows this search may compare against queryVec: a row whose recorded
// identity is not the configured one belongs to another vector space (a model,
// dimension or task-prefix change — see embedding.VectorIdentity), and one
// whose width differs was written by a model that does not produce queryVec.
// Both are skipped rather than scored, and neither is dropped silently: silently
// dropping them turns a reconfiguration into "vector search found nothing" with
// no explanation, and a reconfiguration is exactly when the operator is
// watching the log.
//
// identity is passed in rather than read here: the caller holds the store's read
// lock, and taking it again would risk the RWMutex recursive-read deadlock
// documented at searchVector. An empty identity disables the identity check (a
// store with no embedding model configured).
//
// The foreign-identity warning is logged once per retired identity
// (warnForeignOnce) rather than once per search: during a re-embed every search
// skips the same rows, and a line per query buries the state it reports. The
// dimension warning keeps its per-search reporting — it cannot repeat during a
// re-embed, because the identity check above it takes those rows first.
func (s *Store) loadVectorRows(v *vectorRows, rows *sql.Rows, queryVec []float32, identity string) error {
	// foreign counts skipped rows per stored identity rather than as one
	// total: retirements OVERLAP (the model changes again before the first
	// re-embed finishes), and a single total reported against whichever
	// identity the planner yielded first would absorb the newer retirement —
	// both would be folded into one line naming only the older one, which is
	// exactly the diagnosis the operator needs at that moment.
	var foreign map[string]int
	var identityBytes []byte
	if identity != "" {
		identityBytes = []byte(identity)
	}
	mismatched, mismatchedModel := 0, ""
	first := true

	for rows.Next() {
		// RawBytes, not string/[]byte destinations. database/sql clones a
		// string or []byte column into a fresh allocation for every row, which
		// is one throwaway per column of every memory in the corpus; a
		// RawBytes destination is the one it can hand over without copying,
		// and the copy into the snapshot below is made while the row is still
		// valid — which is the contract RawBytes carries. (It is also why m.scope
		// is coalesced: a NULL column assigns nil to the destination, and this
		// is the one place the scan would otherwise have to notice.)
		d := &v.dest
		if err := rows.Scan(&d.id, &d.embed, &d.model, &d.scope, &d.project, &d.resolvedAt, &d.seq, &d.validFrom, &d.validUntil); err != nil {
			return err
		}
		if identity != "" && !bytes.Equal(d.model, identityBytes) {
			if foreign == nil {
				foreign = make(map[string]int)
			}
			foreign[string(d.model)]++
			continue
		}
		// Dimensions are compared as a whole-vector width, exactly as the
		// decode-then-compare it replaces did: a blob is len/4 float32s, and a
		// blob that is not a multiple of four is compared at its floor rather
		// than rounded up to a dimension the model never produced.
		if len(d.embed)/4 != len(queryVec) {
			mismatched++
			if mismatchedModel == "" {
				mismatchedModel = string(d.model)
			}
			continue
		}
		// A skipped row is never copied, so a store mid-re-embed — where most
		// of the corpus is foreign or the wrong width — does not grow a
		// snapshot for vectors it will not score.
		row := vectorRowSpan{
			id:         vecSpan{len(v.ids), len(d.id)},
			scope:      vecSpan{len(v.scopes), len(d.scope)},
			project:    vecSpan{len(v.projects), len(d.project)},
			embed:      vecSpan{len(v.embeds), len(d.embed)},
			validFrom:  vecSpan{len(v.validFroms), len(d.validFrom)},
			validUntil: vecSpan{len(v.validUntils), len(d.validUntil)},
			resolved:   len(d.resolvedAt) > 0,
			seq:        d.seq,
		}
		v.ids = append(v.ids, d.id...)
		v.scopes = append(v.scopes, d.scope...)
		v.projects = append(v.projects, d.project...)
		v.embeds = append(v.embeds, d.embed...)
		v.validFroms = append(v.validFroms, d.validFrom...)
		v.validUntils = append(v.validUntils, d.validUntil...)
		v.rows = append(v.rows, row)

		// Once per scan, after the first row is in the snapshot, so the seam is
		// reached with the copy demonstrably in progress rather than about to
		// start.
		if first {
			first = false
			duringVectorCopy()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The facts are counted beside the warning rather than inside it, so a count
	// cannot depend on a diagnostic channel being present. Every store NewStore
	// builds has a logger — a nil one becomes a discard handler — so no caller
	// reaches the difference today; it is stated because the coupling is wrong on
	// its face, and a count gated on a log call is one refactor away from being
	// gated on it for real.
	v.facts.mismatched = mismatched
	for m := range foreign {
		v.facts.foreign += foreign[m]
	}
	s.reportSkippedVectors(foreign, mismatched, mismatchedModel, len(v.rows), identity, len(queryVec))
	return nil
}

// reportSkippedVectors logs what a scan could not use: rows from another vector
// space (once per retired identity) and rows of the wrong width (every scan).
// It is shared by the uncached load and the cached corpus's per-search view, so
// the two cannot drift apart on what an operator is told.
func (s *Store) reportSkippedVectors(foreign map[string]int, mismatched int, mismatchedModel string, usable int, identity string, queryDims int) {
	if s.logger != nil && len(foreign) > 0 {
		// Sorted so a log is deterministic whichever row order the planner
		// happened to yield.
		models := make([]string, 0, len(foreign))
		for m := range foreign {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			if s.warnForeignOnce(m) {
				s.logger.Warn("vector search skipped embeddings from another vector space — the configured embedding model changed and those rows are waiting to be re-embedded (reported once per retired identity)",
					"skipped", foreign[m], "usable", usable, "configured_identity", identity, "stored_identity", m)
			}
		}
	}
	if s.logger != nil && mismatched > 0 {
		s.logger.Warn("vector search skipped embeddings whose dimension does not match the query — the embedding model likely changed; re-embed to restore vector recall",
			"skipped", mismatched, "usable", usable, "query_dims", queryDims, "stored_model", mismatchedModel)
	}
}

// search scores the snapshot against queryVec and returns at most limit
// candidates in descending similarity.
//
// The result window is bounded while the corpus is scanned: candidates are
// offered to a top-k heap whose root is the worst row held, so a row that
// cannot win a slot costs one comparison and the corpus is never collected and
// never fully sorted. The order is imposed once at the end, over at most limit
// rows. The old search did the opposite — every positive cosine into one slice
// sized to the corpus, then sort.Slice over all of it — which is why a query
// cost time and memory proportional to the store rather than to the answer.
//
// scope narrows before the cut, not after it, so limit counts candidates the
// caller may actually use. That is why the scope filter runs per row here
// rather than on the k survivors: a compatible row ranked below the cut is
// exactly the one a post-filter would never see.
//
// The validity window, when v.now names one, narrows before the cut for the same
// reason and is the mirror of the keyword leg's SQL predicate (#1053): a row
// stage 2 will withhold must not spend a slot of the depth, or a small window
// fills with expired rows and hides a valid match ranked below them. The rows it
// withholds are counted into v.facts.validityExcluded as they are scored, so an
// empty leg can still report that it found matches and withheld them rather than
// that it found none (see vectorLegFacts).
func (v *vectorRows) search(queryVec []float32, limit int, scope map[string]string) []ScoredMemory {
	// A non-positive limit cannot be satisfied by any window. (The cut it
	// replaces sliced to `limit` without checking, which panicked on a negative
	// one.)
	if limit <= 0 || len(v.rows) == 0 {
		return nil
	}

	probe := newScopeProbe(scope)
	v.cands = v.cands[:0]
	v.globalCands = v.globalCands[:0]
	for i := range v.rows {
		sp := &v.rows[i]
		// Drop non-positive scores: a cosine of 0 or below is not a match, and
		// RRF awards weight by rank alone, so a meaningless rank-1 candidate
		// would otherwise claim the full vector weight and enter the fused
		// window.
		//
		// `!(sim > floor)` rather than `sim <= floor`, which is the form the old
		// search used and which also drops a NaN: every comparison against NaN
		// is false, so the negated form would let a NaN cosine into a window
		// whose comparator cannot rank it, and from there into RRF, which weighs
		// a candidate by its rank alone. A stored embedding cannot be NaN today
		// (they arrive as JSON numbers, and float64 accumulation over 768
		// float32s cannot overflow), but the old shape was right and this one
		// would quietly stop being right the day either stops holding.
		sim := cosineFromBytes(queryVec, v.embed(sp.embed))
		if i == 0 {
			// Once per search, after the first row has been scored, so a test
			// that parks here is inside the scoring loop over a real row and
			// not merely at the top of a search that may have nothing to score.
			duringVectorScore()
		}
		if !(sim > minVectorSimilarity) {
			continue
		}
		if v.validityWithheld(sp) {
			// Count what the window withheld so an empty leg can say why it is
			// empty (#1053). The row matched above the floor — the check above
			// passed — so it is a match the leg is reporting as withheld rather
			// than missed; the query path reads the count when the leg returns
			// nothing and keeps the empty reason `all_invalid`. Free: the
			// similarity is already in hand and no second pass is run.
			v.facts.validityExcluded++
			continue
		}
		if len(probe.want) > 0 && !probe.eligible(v.column(v.scopes, sp.scope)) {
			continue
		}
		c := vecCandidate{score: sim, row: i, seq: sp.seq}
		if v.splitGlobal && string(v.column(v.projects, sp.project)) == globalProject {
			offerCandidate(&v.globalCands, limit, c)
		} else {
			offerCandidate(&v.cands, limit, c)
		}
	}
	if len(v.cands) == 0 && len(v.globalCands) == 0 {
		return nil
	}
	slices.SortFunc(v.cands, compareCandidates)
	slices.SortFunc(v.globalCands, compareCandidates)
	// Merge the two buckets by score, so the leg's rank stays the store-wide
	// order and each bucket keeps its own depth (#1771).
	merged := make([]vecCandidate, 0, len(v.cands)+len(v.globalCands))
	i, j := 0, 0
	for i < len(v.cands) && j < len(v.globalCands) {
		if compareCandidates(v.cands[i], v.globalCands[j]) <= 0 {
			merged = append(merged, v.cands[i])
			i++
		} else {
			merged = append(merged, v.globalCands[j])
			j++
		}
	}
	merged = append(merged, v.cands[i:]...)
	merged = append(merged, v.globalCands[j:]...)

	out := make([]ScoredMemory, len(merged))
	for i, c := range merged {
		sp := &v.rows[c.row]
		out[i] = ScoredMemory{
			MemoryID:  string(v.column(v.ids, sp.id)),
			Score:     c.score,
			Scope:     parseScopeJSON(v.column(v.scopes, sp.scope)),
			ProjectID: string(v.column(v.projects, sp.project)),
			Resolved:  sp.resolved,
		}
	}
	return out
}

// validityWithheld reports whether a row's stored validity window excludes it at
// v.now: expired when a readable valid_until is before it, future when a
// readable valid_from is after it. This is ValidityWithheld over the same rule
// (see ValidityState), evaluated on the raw stored span so the common row — the
// one that states no window — allocates nothing and pays one length check.
//
// A zero v.now means no filter was asked for (every exported vector search), so
// nothing is withheld: those callers return expired rows marked, not withheld,
// exactly as SearchFTS does. An absent or unreadable bound is no claim, so a row
// that names only one end of a window is judged on that end alone.
func (v *vectorRows) validityWithheld(sp *vectorRowSpan) bool {
	if v.now.IsZero() {
		return false
	}
	if sp.validUntil.n > 0 {
		if t, ok := ParseStamp(string(v.validUntils[sp.validUntil.off : sp.validUntil.off+sp.validUntil.n])); ok && t.Before(v.now) {
			return true
		}
	}
	if sp.validFrom.n > 0 {
		if t, ok := ParseStamp(string(v.validFroms[sp.validFrom.off : sp.validFrom.off+sp.validFrom.n])); ok && t.After(v.now) {
			return true
		}
	}
	return false
}

// offerCandidate puts c into the heap cands if it belongs there. Once the window
// is full a candidate has to beat the worst row held, which is a single comparison
// for every row of the corpus that does not make it. It is the free-function form
// so a search can keep one heap per bucket (#1771).
func offerCandidate(cands *[]vecCandidate, k int, c vecCandidate) {
	if len(*cands) < k {
		*cands = append(*cands, c)
		siftUpCandidate(cands, len(*cands)-1)
		return
	}
	if !c.betterThan((*cands)[0]) {
		return
	}
	(*cands)[0] = c
	siftDownCandidate(cands, 0)
}

// siftUpCandidate restores the heap after a new candidate was appended. The root
// of this heap is the WORST candidate held, so a parent is never better than its
// child; a freshly appended candidate that is worse than its parent belongs nearer
// the root, and bubbles up past exactly the parents it is worse than.
func siftUpCandidate(cands *[]vecCandidate, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !(*cands)[i].worseThan((*cands)[parent]) {
			return
		}
		(*cands)[i], (*cands)[parent] = (*cands)[parent], (*cands)[i]
		i = parent
	}
}

// siftDownCandidate restores the heap after the root was replaced: the worst child
// of a node rises until the node is worse than both of them.
func siftDownCandidate(cands *[]vecCandidate, i int) {
	n := len(*cands)
	for {
		worst := i
		if l := 2*i + 1; l < n && (*cands)[l].worseThan((*cands)[worst]) {
			worst = l
		}
		if r := 2*i + 2; r < n && (*cands)[r].worseThan((*cands)[worst]) {
			worst = r
		}
		if worst == i {
			return
		}
		(*cands)[i], (*cands)[worst] = (*cands)[worst], (*cands)[i]
		i = worst
	}
}

// cosineFromBytes is cosineSimilarity over an embedding that was never decoded:
// it reads the little-endian float32s straight out of the stored blob. The
// accumulation is the same loop, in the same order, over the same values, so
// the score is bit-identical to decoding the blob first — and it allocates
// nothing, which decoding does once per row of the corpus.
//
// The width guard counts float32s the way the loader does (len/4, floored) and
// not bytes, so a blob that is not a whole number of vectors is scored from the
// same prefix `bytesToFloat32s` would have decoded rather than being rejected
// here. Deciding a malformed blob is not a vector belongs to the loader's
// dimension filter, which is where it was before.
func cosineFromBytes(query []float32, blob []byte) float32 {
	if len(blob)/4 != len(query) || len(query) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range query {
		ai := float64(query[i])
		bi := float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4])))
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

// maxRetainedVectorBytes caps what an idle search keeps alive in the pool. A
// snapshot is as large as the corpus it copied — about 3 KiB per memory at 768
// dimensions — so a store that once searched 100k memories would otherwise hold
// hundreds of megabytes for a query that may not come back for weeks. Only the
// embedding column is bounded: the other buffers together are a few tens of
// bytes per row, so they are a small fraction of what this releases, and a
// dropped buffer costs one regrow on the next search, which is nothing next to
// what the pre-snapshot search allocated on every query.
const maxRetainedVectorBytes = 32 << 20

// borrowVectorRows takes a scratch snapshot from the store's pool, or makes one.
// A store built as a literal (the snapshot store Candidates builds) has no pool
// function, so the nil case is a real one rather than a bug to assert away.
func (s *Store) borrowVectorRows() *vectorRows {
	// A store literal built without scratch gets a fresh snapshot rather than a nil
	// dereference. That is the pre-#556 behaviour and the expensive one, so the
	// literals that stand in for a store share the real store's scratch instead
	// (see the field comment); this branch is what a store nobody wired up gets,
	// and it is correct rather than fast.
	if s.scratch == nil {
		return &vectorRows{}
	}
	return s.scratch.borrow()
}

// returnVectorRows hands the scratch back for the next search to reuse.
func (s *Store) returnVectorRows(v *vectorRows) {
	if s.scratch == nil {
		return
	}
	s.scratch.put(v)
}

// scopeProbe answers "may this row be compared against a scope request" without
// parsing the scope of every row in the corpus.
//
// ScopeMatches only ever EXCLUDES a row that explicitly mentions a requested
// key and disagrees with it, so a row whose stored text does not contain a
// requested key at all cannot be excluded — the substring test is a sound
// pre-filter. It can only save a parse, never decide membership: a row the test
// cannot rule out still goes through the full ScopeMatches.
type scopeProbe struct {
	want map[string]string
	// keys are the requested keys already quoted the way they appear inside a
	// stored scope object. Empty when parseAll is set.
	keys [][]byte
	// parseAll drops the shortcut for a request whose keys encoding/json could
	// have escaped, so the text cannot be searched for them.
	parseAll bool
}

func newScopeProbe(scope map[string]string) scopeProbe {
	p := scopeProbe{want: scope}
	if len(scope) == 0 {
		return p
	}
	p.keys = make([][]byte, 0, len(scope))
	for key := range scope {
		quoted := quoteScopeKey(key)
		if quoted == nil {
			p.parseAll = true
			return p
		}
		p.keys = append(p.keys, quoted)
	}
	return p
}

// eligible reports whether a row whose stored scope column is raw satisfies the
// request. An empty request matches everything, as ScopeMatches itself does.
func (p scopeProbe) eligible(raw []byte) bool {
	if len(p.want) == 0 {
		return true
	}
	if !p.parseAll {
		for _, key := range p.keys {
			if !bytes.Contains(raw, key) {
				continue
			}
			return ScopeMatches(parseScopeJSON(raw), p.want)
		}
		return true
	}
	return ScopeMatches(parseScopeJSON(raw), p.want)
}

// quoteScopeKey returns a key as it appears inside a stored scope object —
// `"key"` — or nil when encoding/json could have written it as something else.
//
// scopeJSON marshals with json.Marshal, whose default encoder escapes the HTML
// characters <, > and &, the quote, the backslash and every control character,
// so a key containing one of those is not in the stored text in the form the
// request spells it. Non-ASCII is declined for the same reason: that encoder
// also escapes U+2028 and U+2029, and restricting the shortcut to plain ASCII is
// cheaper than telling those two code points from any other UTF-8 sequence.
// Declining costs the shortcut, not correctness — the caller parses every row,
// which is what it did before.
func quoteScopeKey(key string) []byte {
	for i := range len(key) {
		if c := key[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return nil
		}
	}
	quoted := make([]byte, 0, len(key)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, key...)
	return append(quoted, '"')
}

// vectorScratch is the pool of per-search corpus snapshots, plus the one number
// worth watching: how many snapshots had to be allocated rather than recycled.
//
// The counter is the point. A sync.Pool's contents are per-processor and are
// dropped at every collection, so "is the pool empty" is not a property a test can
// assert on — it passes on one P and fails under parallel execution. Counting the
// allocations instead is deterministic, and it is the actual cost: a snapshot
// standing in for a store with a pool of its own allocates one per query and
// returns it to a pool that is immediately garbage.
type vectorScratch struct {
	pool   sync.Pool
	allocs atomic.Int64
}

func newVectorScratch() *vectorScratch {
	s := &vectorScratch{}
	s.pool.New = func() any {
		s.allocs.Add(1)
		return &vectorRows{}
	}
	return s
}

// allocs reports how many corpus snapshots have been allocated. A steady-state
// search on an unchanged corpus stops moving it.
func (s *vectorScratch) allocationCount() int64 {
	if s == nil {
		return 0
	}
	return s.allocs.Load()
}

func (s *vectorScratch) borrow() *vectorRows {
	v, _ := s.pool.Get().(*vectorRows)
	if v == nil {
		// Reached only when the pool is emptied between the New and the Get,
		// which a collection between them can do. Counted here too, so the number
		// is every allocation and not only the ones that came from New.
		v = s.newSnapshot()
	}
	v.reset()
	return v
}

func (s *vectorScratch) newSnapshot() *vectorRows {
	s.allocs.Add(1)
	return &vectorRows{}
}

func (s *vectorScratch) put(v *vectorRows) {
	if cap(v.embeds) > maxRetainedVectorBytes {
		v.embeds = nil
	}
	s.pool.Put(v)
}
