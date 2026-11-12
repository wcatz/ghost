package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// backdate rewrites a memory's created_at/updated_at.
func backdate(ctx context.Context, db *sql.DB, id string, ageDays int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime('now', ?), updated_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", ageDays), fmt.Sprintf("-%d days", ageDays), id)
	return err
}

func seedFacts(ctx context.Context, store *memory.Store, db *sql.DB, project string, facts []string) ([]string, error) {
	ids := make([]string, len(facts))
	n := len(facts)
	for i, fact := range facts {
		id, err := store.CreateFromCorpus(ctx, project, memory.Memory{
			Category: "fact", Content: fact, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			return ids[:i], fmt.Errorf("seed fact %d: %w", i, err)
		}
		if err := backdate(ctx, db, id, n-i); err != nil {
			return ids[:i], fmt.Errorf("backdate fact %d: %w", i, err)
		}
		ids[i] = id
	}
	return ids, nil
}
