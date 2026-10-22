package memory

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// The vector corpus cache (#1031).
//
// Every vector search used to re-read and re-copy the whole corpus out of
// SQLite, and that row load was most of a hybrid search. This keeps ONE
// immutable copy of the corpus per store and re-uses it for as long as the
// database says it is still the corpus.
//
// "The database says" is the vector_epoch counter: a one-row table that
// triggers bump, in the writing transaction, on exactly the writes that change
// what the vector query returns (see the schema). Being in the database it is
// right across processes — several Ghost processes share one file, and SQLite
// is the only thing they share — and being bumped in the writer's transaction it
// is read consistently with the rows: a reader that sees epoch E sees the state
// E describes. PRAGMA data_version was rejected for the reasons in #1031: it
// does not report this connection's own writes, and it moves on every commit
// by any other process, including the retrieval record every search writes.
//
// The rules that make a hit safe:
//
//   - The epoch is read, and the hit is decided, under the store's read lock
//     and on the same handle the rows would come from. Inside Candidates that
//     is its pinned read transaction, so the rows scored are the rows the
//     snapshot sees.
//   - A copy is PUBLISHED only when it is exactly the state its epoch
//     describes: read inside one transaction (Candidates), or bracketed by two
//     epoch reads that agree (anything else). A load a concurrent writer
//     overlapped is still used, once, by the search that made it — which is what
//     an uncached search always did — and never kept.
//   - A copy older than the one held is never published over it.
//   - Scoring runs on the immutable copy with the store lock released, exactly
//     as the uncached path does (#556).
//
// What is cached is the corpus without the query's questions: every row whose
// stored identity is the configured one, whatever its width, in the order the
// database yielded it; plus the rows of another identity as (project, model)
// pairs, so the foreign-row facts and warnings are still computed per search
// and per project. The project filter, the width filter and the scope probe run
// per search in view(). Scores are the same cosineFromBytes over the same bytes
// as the uncached path, so they are bit-identical.
//
// Bounded: a corpus whose embedding column would exceed vectorCacheMaxBytes is
// never loaded for the cache, and the search takes the uncached path exactly as
// before. A held copy is dropped after vectorCacheIdle without use.

// vectorCacheMaxBytes is the most embedding bytes the cache will hold. It is the
// bound the scratch pool already uses for what an idle store retains. A variable
// only so a test can lower it.
var vectorCacheMaxBytes int64 = maxRetainedVectorBytes

// beforeVectorCorpusLoadFn is a test seam between a miss's epoch read and its
// corpus query: the one window in which a write by another process makes the
// rows newer than the epoch they were read under, which is what the bracket
// below exists to catch. Production leaves it a no-op.
var beforeVectorCorpusLoadFn atomic.Value // func()

func init() { beforeVectorCorpusLoadFn.Store(func() {}) }

func beforeVectorCorpusLoad() {
	fn, _ := beforeVectorCorpusLoadFn.Load().(func())
	fn()
}

// vectorCacheIdle is how long an unused copy is kept. Each cache takes it at
// construction and carries its own, because the release runs on a timer
// goroutine and a package variable a test writes would be read there unordered.
const vectorCacheIdle = 10 * time.Minute

// vectorEpochQuery reads the counter: one point read, which is all a hit costs.
const vectorEpochQuery = `SELECT COALESCE((SELECT epoch FROM vector_epoch WHERE id = 1), 0)`

// vectorRowCountQuery is the corpus size, read only on a miss: it is the bound's
// pre-check, and a hit has no use for it.
//
// It names the rows the cache would keep, the way vectorCountQuery names the rows
// an uncached copy appends: the same join the scan reads, the configured identity
// (an empty identity counts every stored one, because the loader keeps every model
// when none is configured), and the query's width. A row of another identity or
// width is not what the estimate below is for — the estimate multiplies this count
// by the query's width, so it sizes the configured rows the search may score.
// Counting the whole table instead made a corpus that fits the bound look over it
// as soon as another vector space had rows in the table (#1257).
const vectorRowCountQuery = `
	SELECT COUNT(*)
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id
	WHERE (? = '' OR e.model = ?) AND length(e.embedding)/4 = ?`

// foreignVector is a row of another vector space: no bytes, only what the
// per-search facts and warnings need.
type foreignVector struct {
	project vecSpan // into vectorCorpus.projects
	model   uint32  // into vectorCorpus.models
}

// vectorCorpus is one immutable copy of the corpus at one epoch.
type vectorCorpus struct {
	epoch    int64
	identity string

	ids, scopes, projects, embeds []byte
	// validFroms and validUntils are the validity bounds, in row order, so a
	// view can judge a row's window before the cut exactly as the uncached scan
	// does (#1053). They are the reason the epoch has a validity arm: a cached
	// copy that held them would otherwise go stale the moment a row's window was
	// edited, and a search would keep offering a row that has just expired.
	validFroms, validUntils []byte
	// rows are the rows whose identity is the configured one, in scan order.
	rows []vectorRowSpan
	// rowModel[i] is rows[i]'s stored identity, an index into models.
	rowModel []uint32
	models   []string
	foreign  []foreignVector
}

// vectorCache holds at most one corpus per store.
type vectorCache struct {
	mu    sync.Mutex
	cur   *vectorCorpus
	last  time.Time
	timer *time.Timer
	idle  time.Duration

	// loads counts corpus loads and hits counts served copies. They exist for
	// tests, which assert "this search did not read the corpus" by counting
	// rather than by timing.
	loads, hits atomic.Int64
}

func newVectorCache() *vectorCache { return &vectorCache{idle: vectorCacheIdle} }

// setIdle changes the idle period, for a test that cannot wait ten minutes.
func (c *vectorCache) setIdle(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idle = d
}

// get returns the held copy if it is the corpus at (epoch, identity).
func (c *vectorCache) get(epoch int64, identity string) *vectorCorpus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || c.cur.epoch != epoch || c.cur.identity != identity {
		return nil
	}
	c.touchLocked()
	c.hits.Add(1)
	return c.cur
}

// put publishes cp unless the held copy is newer. Equal epochs under a
// different identity replace, because the identity is part of the key.
func (c *vectorCache) put(cp *vectorCorpus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && c.cur.identity == cp.identity && c.cur.epoch >= cp.epoch {
		return
	}
	c.cur = cp
	c.touchLocked()
}

// held reports the epoch of the held copy, for tests.
func (c *vectorCache) held() (epoch int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return 0, false
	}
	return c.cur.epoch, true
}

// touchLocked records a use and arms the idle release.
func (c *vectorCache) touchLocked() {
	c.last = time.Now()
	if c.timer == nil {
		c.timer = time.AfterFunc(c.idle, c.expire)
	}
}

// expire drops the copy when it has been idle for the whole period and
// otherwise re-arms for what is left of it.
func (c *vectorCache) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if idle := time.Since(c.last); idle < c.idle {
		c.timer.Reset(c.idle - idle)
		return
	}
	c.cur = nil
	c.timer = nil
}

// readVectorEpoch reads the counter from q. ok is false when the database has no usable counter (a schema older than v24
// opened read-only, or a failed read), which is "do not cache", never an error:
// the uncached path that follows reports any real failure itself.
func readVectorEpoch(ctx context.Context, q Queryer) (epoch int64, ok bool) {
	var e int64
	if err := q.QueryRowContext(ctx, vectorEpochQuery).Scan(&e); err != nil {
		return 0, false
	}
	return e, true
}

// vectorCorpusFor returns the corpus a search may score, or nil when the search
// must take the uncached path. It holds the store's read lock for the epoch
// read and, on a miss, the load; the caller scores without it.
func (s *Store) vectorCorpusFor(ctx context.Context, queryVec []float32, identity string) (*vectorCorpus, error) {
	cache := s.vcache
	if cache == nil || vectorCacheMaxBytes <= 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := s.queryDB()
	epoch, ok := readVectorEpoch(ctx, q)
	if !ok {
		return nil, nil
	}
	if cp := cache.get(epoch, identity); cp != nil {
		return cp, nil
	}
	// A corpus that cannot be kept is not loaded for the cache: the estimate is
	// the configured rows' count at the query's width (vectorRowCountQuery). A
	// failed count is "do not cache".
	var n int64
	if err := q.QueryRowContext(ctx, vectorRowCountQuery, identity, identity, len(queryVec)).Scan(&n); err != nil {
		return nil, nil
	}
	if n*int64(len(queryVec))*4 > vectorCacheMaxBytes {
		return nil, nil
	}

	beforeVectorCorpusLoad()
	rows, err := queryVectorCorpus(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	cp, err := loadVectorCorpus(rows, identity)
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}
	cp.epoch, cp.identity = epoch, identity
	cache.loads.Add(1)

	exact := s.snapshot != nil // one transaction: the rows ARE the epoch's state
	if !exact {
		after, ok := readVectorEpoch(ctx, q)
		exact = ok && after == epoch
	}
	if exact && int64(len(cp.embeds)) <= vectorCacheMaxBytes {
		cache.put(cp)
	}
	return cp, nil
}

// loadVectorCorpus reads the corpus query into an immutable copy.
func loadVectorCorpus(rows *sql.Rows, identity string) (*vectorCorpus, error) {
	cp := &vectorCorpus{}
	var identityBytes []byte
	if identity != "" {
		identityBytes = []byte(identity)
	}
	modelIdx := map[string]uint32{}
	intern := func(m []byte) uint32 {
		if i, ok := modelIdx[string(m)]; ok {
			return i
		}
		i := uint32(len(cp.models))
		cp.models = append(cp.models, string(m))
		modelIdx[cp.models[i]] = i
		return i
	}
	var d vectorScanDest
	first := true
	for rows.Next() {
		if err := rows.Scan(&d.id, &d.embed, &d.model, &d.scope, &d.project, &d.resolvedAt, &d.seq, &d.validFrom, &d.validUntil); err != nil {
			return nil, err
		}
		if identity != "" && !bytes.Equal(d.model, identityBytes) {
			cp.foreign = append(cp.foreign, foreignVector{
				project: vecSpan{len(cp.projects), len(d.project)},
				model:   intern(d.model),
			})
			cp.projects = append(cp.projects, d.project...)
			continue
		}
		cp.rows = append(cp.rows, vectorRowSpan{
			id:         vecSpan{len(cp.ids), len(d.id)},
			scope:      vecSpan{len(cp.scopes), len(d.scope)},
			project:    vecSpan{len(cp.projects), len(d.project)},
			embed:      vecSpan{len(cp.embeds), len(d.embed)},
			validFrom:  vecSpan{len(cp.validFroms), len(d.validFrom)},
			validUntil: vecSpan{len(cp.validUntils), len(d.validUntil)},
			resolved:   len(d.resolvedAt) > 0,
			seq:        d.seq,
		})
		cp.rowModel = append(cp.rowModel, intern(d.model))
		cp.ids = append(cp.ids, d.id...)
		cp.scopes = append(cp.scopes, d.scope...)
		cp.projects = append(cp.projects, d.project...)
		cp.embeds = append(cp.embeds, d.embed...)
		cp.validFroms = append(cp.validFroms, d.validFrom...)
		cp.validUntils = append(cp.validUntils, d.validUntil...)
		if first {
			first = false
			duringVectorCopy()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cp, nil
}

// globalProject is the project whose memories every project's search includes.
const globalProject = "_global"

// view builds one search's candidate snapshot from the shared copy: the rows of
// projectID and of _global (every row when allProjects), at the query's width.
// Foreign and mismatched rows are counted and warned about exactly as
// loadVectorRows does for an uncached scan.
//
// The returned vectorRows shares the copy's column buffers read-only and takes
// its rows and heap storage from scratch, which is handed back by release. It is
// never returned to the pool itself: a pooled snapshot appends into its column
// buffers, and these are the cache's.
func (s *Store) viewVectorCorpus(cp *vectorCorpus, scratch *vectorRows, projectID string, allProjects bool, queryVec []float32) *vectorRows {
	v := &vectorRows{
		ids: cp.ids, scopes: cp.scopes, projects: cp.projects, embeds: cp.embeds,
		validFroms: cp.validFroms, validUntils: cp.validUntils,
		rows:  scratch.rows[:0],
		cands: scratch.cands[:0],
	}
	inScope := func(sp vecSpan) bool {
		if allProjects {
			return true
		}
		p := cp.projects[sp.off : sp.off+sp.n]
		return string(p) == projectID || string(p) == globalProject
	}

	var foreign map[string]int
	for _, f := range cp.foreign {
		if !inScope(f.project) {
			continue
		}
		if foreign == nil {
			foreign = make(map[string]int)
		}
		foreign[cp.models[f.model]]++
	}
	mismatched, mismatchedModel := 0, ""
	for i := range cp.rows {
		row := cp.rows[i]
		if !inScope(row.project) {
			continue
		}
		if row.embed.n/4 != len(queryVec) {
			mismatched++
			if mismatchedModel == "" {
				mismatchedModel = cp.models[cp.rowModel[i]]
			}
			continue
		}
		v.rows = append(v.rows, row)
	}
	v.facts.mismatched = mismatched
	v.facts.usable = len(v.rows)
	for _, n := range foreign {
		v.facts.foreign += n
	}
	s.reportSkippedVectors(foreign, mismatched, mismatchedModel, len(v.rows), cp.identity, len(queryVec))
	return v
}

// releaseViewScratch hands the rows and heap storage a view grew back to the
// scratch it borrowed them from.
func releaseViewScratch(scratch, v *vectorRows) {
	scratch.rows, scratch.cands = v.rows[:0], v.cands[:0]
}
