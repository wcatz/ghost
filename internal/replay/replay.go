package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// Query is the shape a replay needs from a recorded search call. It is
// the store's record under a local name so the gate does not depend on fields
// the audit adds later.
type Query = memory.SearchRecord

// replayClock pins decay against a fixed instant so two runs of one build on
// one snapshot print the same ids. Replay is deliberately keyword-only and
// offline: a snapshot's vectors describe the stored memories, not the fresh
// query, and producing a query vector would need the embedding endpoint. The
// gate therefore compares lexical result sets, which need no network at all.
var replayClock = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

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
	if q.Source == memory.SearchAllSource {
		// ghost_search_all never goes through the assembler: it is one
		// whole-corpus SearchHybridAll read, and its ProjectID is the "_global"
		// label meaning "no one project". Scoping it to project "_global"
		// would rank a different corpus and report a regression that is only
		// the replay's own mistake. nil keeps it keyword-only like the rest
		// of the gate; the id order is the ranking the tool returned.
		rows, err := r.Store.SearchHybridAll(ctx, q.Query, nil, r.Limit)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, m.ID)
		}
		return ids, nil
	}
	req := assemble.Request{
		ProjectID: q.ProjectID,
		Query:     q.Query,
		Scope:     q.Scope,
		Category:  q.Category,
		Retention: q.Retention,
		Source:    assemble.SourceSearch,
		Budget:    assemble.Budget{MaxItems: r.Limit},
		Condition: assemble.CondFTSOnly,
		Now:       replayClock,
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
	cmd := exec.CommandContext(ctx, r.Binary, "replay", "--rank", "--snapshot", r.Snapshot, "--record", name)
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
		out = append(out, Result{Query: q, Diff: ComputeDiff(baseIDs, currIDs, topK)})
	}
	return out
}
