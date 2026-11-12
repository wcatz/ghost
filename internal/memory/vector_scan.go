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

const vectorScanColumns = `
	SELECT e.memory_id, e.embedding, e.model, COALESCE(m.scope, ''), m.project_id, m.resolved_at, m.rowid, m.valid_from, m.valid_until
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id`

const vectorProjectPredicate = `(m.project_id = ? OR m.project_id = '_global')`

const vectorProjectScanQuery = vectorScanColumns + `
	WHERE ` + vectorProjectPredicate

const vectorCountFrom = `
	SELECT COUNT(*)
	FROM memory_embeddings e
	JOIN memories m ON m.id = e.memory_id`

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

func queryVectorCorpus(ctx context.Context, q Queryer) (*sql.Rows, error) {
	return q.QueryContext(ctx, vectorScanColumns)
}

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

type vecSpan struct{ off, n int }

type vectorRowSpan struct {
	id, scope, project, embed vecSpan
	validFrom, validUntil     vecSpan
	resolved                  bool
	seq                       int64
}

type vecCandidate struct {
	score float32
	row   int
	seq   int64
}

func (c vecCandidate) betterThan(o vecCandidate) bool {
	return compareCandidates(c, o) < 0
}

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

type vectorRows struct {
	ids, scopes, projects, embeds []byte
	validFroms, validUntils       []byte
	rows                          []vectorRowSpan
	cands                         []vecCandidate
	globalCands                   []vecCandidate
	splitGlobal                   bool
	now                           time.Time
	dest                          vectorScanDest
	facts                         vectorLegFacts
}

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
	v.now = time.Time{}
	v.splitGlobal = false
	v.facts = vectorLegFacts{}
}

func (v *vectorRows) column(buf []byte, sp vecSpan) []byte { return buf[sp.off : sp.off+sp.n] }

func (v *vectorRows) embed(sp vecSpan) []byte { return v.embeds[sp.off : sp.off+sp.n] }

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

func (s *Store) loadVectorRows(v *vectorRows, rows *sql.Rows, queryVec []float32, identity string) error {
	var foreign map[string]int
	var identityBytes []byte
	if identity != "" {
		identityBytes = []byte(identity)
	}
	mismatched, mismatchedModel := 0, ""
	first := true

	for rows.Next() {
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
		if len(d.embed)/4 != len(queryVec) {
			mismatched++
			if mismatchedModel == "" {
				mismatchedModel = string(d.model)
			}
			continue
		}
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

		if first {
			first = false
			duringVectorCopy()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	v.facts.mismatched = mismatched
	v.facts.usable = len(v.rows)
	for m := range foreign {
		v.facts.foreign += foreign[m]
	}
	s.reportSkippedVectors(foreign, mismatched, mismatchedModel, len(v.rows), identity, len(queryVec))
	return nil
}

func (s *Store) reportSkippedVectors(foreign map[string]int, mismatched int, mismatchedModel string, usable int, identity string, queryDims int) {
	if s.logger != nil && len(foreign) > 0 {
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

func (v *vectorRows) search(queryVec []float32, limit int, scope map[string]string) []ScoredMemory {
	if limit <= 0 || len(v.rows) == 0 {
		return nil
	}

	probe := newScopeProbe(scope)
	v.cands = v.cands[:0]
	v.globalCands = v.globalCands[:0]
	for i := range v.rows {
		sp := &v.rows[i]
		sim := cosineFromBytes(queryVec, v.embed(sp.embed))
		if i == 0 {
			duringVectorScore()
		}
		if !(sim > minVectorSimilarity) {
			continue
		}
		if v.validityWithheld(sp) {
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

const maxRetainedVectorBytes = 32 << 20

func (s *Store) borrowVectorRows() *vectorRows {
	if s.scratch == nil {
		return &vectorRows{}
	}
	return s.scratch.borrow()
}

func (s *Store) returnVectorRows(v *vectorRows) {
	if s.scratch == nil {
		return
	}
	s.scratch.put(v)
}

type scopeProbe struct {
	want     map[string]string
	keys     [][]byte
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

func (s *vectorScratch) allocationCount() int64 {
	if s == nil {
		return 0
	}
	return s.allocs.Load()
}

func (s *vectorScratch) borrow() *vectorRows {
	v, _ := s.pool.Get().(*vectorRows)
	if v == nil {
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
