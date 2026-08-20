package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// backdate rewrites a memory's created_at/updated_at. store.Create always
// stamps now(); supersede.orient() (internal/supersede/supersede.go) decides
// newer/older by updated_at, so every seeded fact needs a distinct timestamp
// matching its list position. Duplicated from internal/bench/staleness.go's
// helper of the same name/shape — raw SQL on the harness-owned store only.
func backdate(ctx context.Context, db *sql.DB, id string, ageDays int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime('now', ?), updated_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", ageDays), fmt.Sprintf("-%d days", ageDays), id)
	return err
}

// seedFacts creates one memory per fact, oldest (facts[0]) to newest
// (facts[len(facts)-1]) — list order is temporal order in MemoryAgentBench's
// Conflict_Resolution data (see splitFacts). Returns store IDs in the same
// order.
func seedFacts(ctx context.Context, store *memory.Store, db *sql.DB, project string, facts []string) ([]string, error) {
	ids := make([]string, len(facts))
	n := len(facts)
	for i, fact := range facts {
		id, err := store.Create(ctx, project, memory.Memory{
			Category: "fact", Content: fact, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			return nil, fmt.Errorf("seed fact %d: %w", i, err)
		}
		// The oldest fact gets the largest age; the newest gets age 1 (never
		// 0 — 0 would tie with "now" for anything created after this pass).
		if err := backdate(ctx, db, id, n-i); err != nil {
			return nil, fmt.Errorf("backdate fact %d: %w", i, err)
		}
		ids[i] = id
	}
	return ids, nil
}
