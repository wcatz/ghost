package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// Query is the shape a replay needs from a recorded search call. It is
// the store's record under a local name so the gate does not depend on fields
// the audit adds later.
type Query = memory.SearchRecord

// Replay is deliberately keyword-only and offline: a snapshot's vectors
// describe the stored memories, not the fresh query, and producing a query
// vector would need the embedding endpoint. The gate therefore compares
// lexical result sets, which need no network at all.

// recordedInstant is the audit row's own timestamp. A search is a question
// about the store as it stood when the caller asked it, so decay, validity and
// version selection are judged at that instant — never at the replay's wall
// clock and never at a pinned constant.
func recordedInstant(q Query) (time.Time, error) {
	raw := strings.TrimSpace(q.RecordedAt)
	for _, layout := range []string{memory.StoredStampLayout, time.RFC3339} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unreadable recorded time %q: replay needs the call's own instant", raw)
}

// recordedAsOf returns the historical binding the live call asked about, or
// nil for a present-day search. A recorded as_of is a binding, not a filter:
// without it a call over a since-edited memory ranks today's version.
func recordedAsOf(q Query) (*time.Time, error) {
	raw := strings.TrimSpace(q.AsOf)
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("unreadable recorded as_of %q: %w", raw, err)
	}
	utc := at.UTC()
	return &utc, nil
}

// effectiveLimit keeps a call's own answer window. A caller that asked for one
// row has a one-row baseline, so comparing it against the command-wide budget
// would read the missing positions as ranking changes.
func effectiveLimit(q Query, commandLimit int) int {
	limit := commandLimit
	if q.Limit > 0 && (limit <= 0 || q.Limit < limit) {
		limit = q.Limit
	}
	return limit
}

// Runner ranks one recorded query against a pinned snapshot.
type Runner interface {
	Rank(ctx context.Context, q Query) ([]string, error)
}

// StoreRunner ranks in-process against a snapshot already opened read-only.
type StoreRunner struct {
	Store *memory.Store
	Limit int
}

func (r StoreRunner) Rank(ctx context.Context, q Query) ([]string, error) {
	limit := effectiveLimit(q, r.Limit)
	if q.Source == memory.SearchAllSource {
		// ghost_search_all never goes through the assembler: it is one
		// whole-corpus SearchHybridAll read, and its ProjectID is the "_global"
		// label meaning "no one project". Scoping it to project "_global"
		// would rank a different corpus and report a regression that is only
		// the replay's own mistake. nil keeps it keyword-only like the rest
		// of the gate; the id order is the ranking the tool returned.
		rows, err := r.Store.SearchHybridAll(ctx, q.Query, nil, limit)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, m.ID)
		}
		return ids, nil
	}
	at, err := recordedInstant(q)
	if err != nil {
		return nil, err
	}
	asOf, err := recordedAsOf(q)
	if err != nil {
		return nil, err
	}
	req := assemble.Request{
		ProjectID: q.ProjectID,
		Query:     q.Query,
		Scope:     q.Scope,
		Category:  q.Category,
		Retention: q.Retention,
		Source:    assemble.SourceSearch,
		Budget:    assemble.Budget{MaxItems: limit},
		Condition: assemble.CondFTSOnly,
		Now:       at,
		AsOf:      asOf,
	}
	res, err := assemble.Run(ctx, r.Store, req)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, item := range res.Items {
		ids = append(ids, item.ID)
	}
	return ids, nil
}

// RecordedRunner uses the ids the recording build stored. It is the baseline
// when no baseline binary was named: the historical result set is the thing a
// change is measured against.
type RecordedRunner struct{}

func (RecordedRunner) Rank(_ context.Context, q Query) ([]string, error) {
	return q.Results, nil
}

// BinaryRunner asks another ghost build to rank a query through the same
// keyword-only path, using the rank mode of the replay command so the stable
// contract is a JSON array of ids on stdout.
type BinaryRunner struct {
	Binary   string
	Snapshot string
	Limit    int
}

func (r BinaryRunner) Rank(ctx context.Context, q Query) ([]string, error) {
	f, err := os.CreateTemp("", "ghost-replay-*.json")
	if err != nil {
		return nil, fmt.Errorf("write query for the baseline build: %w", err)
	}
	name := f.Name()
	defer os.Remove(name) //nolint:errcheck // best-effort scratch cleanup
	if err := json.NewEncoder(f).Encode(q); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write query for the baseline build: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write query for the baseline build: %w", err)
	}
	args := []string{"replay", "--rank", "--snapshot", r.Snapshot, "--record", name}
	if limit := effectiveLimit(q, r.Limit); limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("baseline build could not rank the query: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(out, &ids); err != nil {
		return nil, fmt.Errorf("baseline build returned no id list: %w", err)
	}
	return ids, nil
}

// LoadQueries reads the recorded searches from a pinned snapshot. The snapshot
// is opened twice: one handle is the primary the store is built with, the other
// the read-only handle every retrieval goes through.
func LoadQueries(snapshot string, limit int) ([]Query, func(), error) {
	db, err := memory.OpenReadDB(snapshot)
	if err != nil {
		return nil, nil, err
	}
	readDB, err := memory.OpenReadDB(snapshot)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	closeFn := func() {
		_ = readDB.Close()
		_ = db.Close()
	}
	store := memory.NewStoreWithRead(db, readDB, slog.New(slog.NewTextHandler(io.Discard, nil)))
	queries, err := store.SearchRecords(context.Background(), limit)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return queries, closeFn, nil
}

// OpenSnapshot builds a store over a pinned snapshot for the in-process rank
// mode. The caller owns the returned close function.
func OpenSnapshot(snapshot string) (*memory.Store, func(), error) {
	db, err := memory.OpenReadDB(snapshot)
	if err != nil {
		return nil, nil, err
	}
	readDB, err := memory.OpenReadDB(snapshot)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	closeFn := func() {
		_ = readDB.Close()
		_ = db.Close()
	}
	return memory.NewStoreWithRead(db, readDB, slog.New(slog.NewTextHandler(io.Discard, nil))), closeFn, nil
}

// Run compares every recorded query with the two runners and collects the
// diffs. A query that one build cannot rank is reported as an error for that
// query rather than stopping the rest of the gate.
func Run(ctx context.Context, queries []Query, baseline, current Runner, topK int) []Result {
	out := make([]Result, 0, len(queries))
	for _, q := range queries {
		// A call's own answer window decides the comparison: a limit-1 call
		// has one baseline position, so the top-k window for it is one too.
		k := topK
		if q.Limit > 0 && q.Limit < k {
			k = q.Limit
		}
		baseIDs, err := baseline.Rank(ctx, q)
		if err != nil {
			out = append(out, Result{Query: q, Err: fmt.Errorf("baseline: %w", err)})
			continue
		}
		currIDs, err := current.Rank(ctx, q)
		if err != nil {
			out = append(out, Result{Query: q, Err: fmt.Errorf("current: %w", err)})
			continue
		}
		out = append(out, Result{Query: q, Diff: ComputeDiff(baseIDs, currIDs, k)})
	}
	return out
}
