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

var vectorCacheMaxBytes int64 = maxRetainedVectorBytes

var beforeVectorCorpusLoadFn atomic.Value // func()

func init() { beforeVectorCorpusLoadFn.Store(func() {}) }

func beforeVectorCorpusLoad() {
	fn, _ := beforeVectorCorpusLoadFn.Load().(func())
	fn()
}

const vectorCacheIdle = 10 * time.Minute

const vectorEpochQuery = `SELECT COALESCE((SELECT epoch FROM vector_epoch WHERE id = 1), 0)`

const vectorRowCountQuery = `
	SELECT COUNT(*)
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id
	WHERE (? = '' OR e.model = ?) AND length(e.embedding)/4 = ?`

type foreignVector struct {
	project vecSpan // into vectorCorpus.projects
	model   uint32  // into vectorCorpus.models
}

type vectorCorpus struct {
	epoch    int64
	identity string

	ids, scopes, projects, embeds []byte
	validFroms, validUntils       []byte
	rows                          []vectorRowSpan
	rowModel                      []uint32
	models                        []string
	foreign                       []foreignVector
}

type vectorCache struct {
	mu    sync.Mutex
	cur   *vectorCorpus
	last  time.Time
	timer *time.Timer
	idle  time.Duration

	loads, hits atomic.Int64
}

func newVectorCache() *vectorCache { return &vectorCache{idle: vectorCacheIdle} }

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

func (c *vectorCache) put(cp *vectorCorpus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && c.cur.identity == cp.identity && c.cur.epoch >= cp.epoch {
		return
	}
	c.cur = cp
	c.touchLocked()
}

func (c *vectorCache) held() (epoch int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return 0, false
	}
	return c.cur.epoch, true
}

func (c *vectorCache) touchLocked() {
	c.last = time.Now()
	if c.timer == nil {
		c.timer = time.AfterFunc(c.idle, c.expire)
	}
}

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

func readVectorEpoch(ctx context.Context, q Queryer) (epoch int64, ok bool) {
	var e int64
	if err := q.QueryRowContext(ctx, vectorEpochQuery).Scan(&e); err != nil {
		return 0, false
	}
	return e, true
}

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

const globalProject = "_global"

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

func releaseViewScratch(scratch, v *vectorRows) {
	scratch.rows, scratch.cands = v.rows[:0], v.cands[:0]
}
