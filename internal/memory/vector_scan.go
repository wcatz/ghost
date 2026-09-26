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
	"sync/atomic"
)

// vectorScanColumns is the projection both vector searches read: a candidate's
// id, its embedding, the vector space that embedding was written in, and the
// three status fields fusion applies to the returned row. Nothing else — a
// search that read a column it does not return would be paying the single
// connection for bytes it throws away.
//
// m.scope is coalesced to an empty string so the column is never NULL.
// database/sql assigns nil to a []byte destination it is handed for a NULL
// column, which would drop the snapshot's scope buffer (and its reuse with it)
// on every unscoped memory; an empty string means no scope exactly as NULL does,
// so the coalesce changes no answer — see parseScopeJSON.
const vectorScanColumns = `
	SELECT e.memory_id, e.embedding, e.model, COALESCE(m.scope, ''), m.project_id, m.resolved_at
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id`

// duringVectorCopyFn and duringVectorScoreFn are test seams inside a vector
// search, one in each of its two phases. Production leaves both no-ops. A test
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
	// resolved is the row's own resolved_at, read here so fusion can demote it
	// without a second lookup per candidate.
	resolved bool
}

// vecCandidate is one row competing for the result window, held as a score and a
// row index rather than as a ScoredMemory. The strings and the scope map a
// result carries are materialized only for the rows that actually win a slot,
// which is what keeps a search from allocating for the whole corpus.
type vecCandidate struct {
	score float32
	row   int
}

// betterThan reports whether a outranks b: the higher cosine wins, and on an
// exact tie the row the scan yielded first wins.
//
// The tiebreak is what makes the bounded top-k a total order. Without one, two
// rows with equal float32 cosines have no order between them, and a bounded
// heap hands back whichever of them happened to reach the cut last, so the
// answer would depend on the heap's internals rather than on the scan. Ordering
// by (score, scan position) makes the returned rows a function of the stored
// data AND the order the scan yielded it in. That is a weaker guarantee than
// independence from the scan order, and deliberately so: the query has no ORDER
// BY, so a VACUUM or an index rebuild can reorder a tie. What it does buy is
// that two searches over the same rows in the same order return the same rows,
// which the old unstable sort.Slice did not promise either.
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
	case a.row < b.row:
		return -1
	default:
		return 1
	}
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
	// in scan order. Only the embedding column is large; ids, scopes and
	// projects are tens of bytes per row.
	ids, scopes, projects, embeds []byte
	// rows locates each row's bytes inside those buffers.
	rows []vectorRowSpan
	// cands is the bounded top-k, reused so a search does not allocate a heap
	// per query.
	cands []vecCandidate
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
}

func (v *vectorRows) reset() {
	v.ids = v.ids[:0]
	v.scopes = v.scopes[:0]
	v.projects = v.projects[:0]
	v.embeds = v.embeds[:0]
	v.rows = v.rows[:0]
	v.cands = v.cands[:0]
	// The counts belong to the scan that set them, and the snapshot is pooled
	// across queries, so a stale one would be read as this query's.
	v.facts = vectorLegFacts{}
}

func (v *vectorRows) column(buf []byte, sp vecSpan) []byte { return buf[sp.off : sp.off+sp.n] }

func (v *vectorRows) embed(sp vecSpan) []byte { return v.embeds[sp.off : sp.off+sp.n] }

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
// clause) and is run against queryDB, so ExplainSearch's trace store still
// reads its snapshot transaction.
func (s *Store) snapshotVectors(ctx context.Context, query string, args []any, queryVec []float32, identity string, v *vectorRows) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
		v.facts.scanned++
		// RawBytes, not string/[]byte destinations. database/sql clones a
		// string or []byte column into a fresh allocation for every row, which
		// is one throwaway per column of every memory in the corpus; a
		// RawBytes destination is the one it can hand over without copying,
		// and the copy into the snapshot below is made while the row is still
		// valid — which is the contract RawBytes carries. (It is also why m.scope
		// is coalesced: a NULL column assigns nil to the destination, and this
		// is the one place the scan would otherwise have to notice.)
		d := &v.dest
		if err := rows.Scan(&d.id, &d.embed, &d.model, &d.scope, &d.project, &d.resolvedAt); err != nil {
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
			id:       vecSpan{len(v.ids), len(d.id)},
			scope:    vecSpan{len(v.scopes), len(d.scope)},
			project:  vecSpan{len(v.projects), len(d.project)},
			embed:    vecSpan{len(v.embeds), len(d.embed)},
			resolved: len(d.resolvedAt) > 0,
		}
		v.ids = append(v.ids, d.id...)
		v.scopes = append(v.scopes, d.scope...)
		v.projects = append(v.projects, d.project...)
		v.embeds = append(v.embeds, d.embed...)
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
	v.facts.mismatched, v.facts.mismatchedModel = mismatched, mismatchedModel
	if s.logger != nil && len(foreign) > 0 {
		// Sorted so a log is deterministic whichever row order the planner
		// happened to yield, and so the fact's single foreignModel names a
		// stable identity.
		models := make([]string, 0, len(foreign))
		for m := range foreign {
			models = append(models, m)
			v.facts.foreign += foreign[m]
		}
		sort.Strings(models)
		v.facts.foreignModel = models[0]
		for _, m := range models {
			if s.warnForeignOnce(m) {
				s.logger.Warn("vector search skipped embeddings from another vector space — the configured embedding model changed and those rows are waiting to be re-embedded (reported once per retired identity)",
					"skipped", foreign[m], "usable", len(v.rows), "configured_identity", identity, "stored_identity", m)
			}
		}
	}
	if s.logger != nil && mismatched > 0 {
		s.logger.Warn("vector search skipped embeddings whose dimension does not match the query — the embedding model likely changed; re-embed to restore vector recall",
			"skipped", mismatched, "usable", len(v.rows), "query_dims", len(queryVec), "stored_model", mismatchedModel)
	}
	return nil
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
func (v *vectorRows) search(queryVec []float32, limit int, scope map[string]string) []ScoredMemory {
	duringVectorScore()

	// A non-positive limit cannot be satisfied by any window. (The cut it
	// replaces sliced to `limit` without checking, which panicked on a negative
	// one.)
	if limit <= 0 || len(v.rows) == 0 {
		return nil
	}

	probe := newScopeProbe(scope)
	v.cands = v.cands[:0]
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
		if !(sim > minVectorSimilarity) {
			continue
		}
		if len(probe.want) > 0 && !probe.eligible(v.column(v.scopes, sp.scope)) {
			continue
		}
		v.offer(limit, vecCandidate{score: sim, row: i})
	}
	if len(v.cands) == 0 {
		return nil
	}
	slices.SortFunc(v.cands, compareCandidates)

	out := make([]ScoredMemory, len(v.cands))
	for i, c := range v.cands {
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

// offer puts c in the top-k if it belongs there. Once the window is full a
// candidate has to beat the worst row held, which is a single comparison for
// every row of the corpus that does not make it.
func (v *vectorRows) offer(k int, c vecCandidate) {
	if len(v.cands) < k {
		v.cands = append(v.cands, c)
		v.siftUp(len(v.cands) - 1)
		return
	}
	if !c.betterThan(v.cands[0]) {
		return
	}
	v.cands[0] = c
	v.siftDown(0)
}

// siftUp restores the heap after a new candidate was appended. The root of this
// heap is the WORST candidate held, so a parent is never better than its child;
// a freshly appended candidate that is worse than its parent belongs nearer the
// root, and bubbles up past exactly the parents it is worse than.
func (v *vectorRows) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !v.cands[i].worseThan(v.cands[parent]) {
			return
		}
		v.cands[i], v.cands[parent] = v.cands[parent], v.cands[i]
		i = parent
	}
}

// siftDown restores the heap after the root was replaced: the worst child of a
// node rises until the node is worse than both of them.
func (v *vectorRows) siftDown(i int) {
	n := len(v.cands)
	for {
		worst := i
		if l := 2*i + 1; l < n && v.cands[l].worseThan(v.cands[worst]) {
			worst = l
		}
		if r := 2*i + 2; r < n && v.cands[r].worseThan(v.cands[worst]) {
			worst = r
		}
		if worst == i {
			return
		}
		v.cands[i], v.cands[worst] = v.cands[worst], v.cands[i]
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
// A store built as a literal (ExplainSearch's trace store) has no pool
// function, so the nil case is a real one rather than a bug to assert away.
func (s *Store) borrowVectorRows() *vectorRows {
	v, _ := s.vectorRowsPool.Get().(*vectorRows)
	if v == nil {
		v = &vectorRows{}
	}
	v.reset()
	return v
}

// returnVectorRows hands the scratch back for the next search to reuse.
func (s *Store) returnVectorRows(v *vectorRows) {
	if cap(v.embeds) > maxRetainedVectorBytes {
		v.embeds = nil
	}
	s.vectorRowsPool.Put(v)
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
