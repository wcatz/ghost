//go:build ignore

// Fixture for reviewer ground-truth testing. Not compiled into the module
// (build tag `ignore`). Each function contains exactly one planted defect;
// the reviewer is expected to find all three.
package fixtures

import (
	"database/sql"
	"sync"
)

// PLANTED BUG 1: the error from Exec is discarded, so a failed write is
// reported as success.
func saveMemory(db *sql.DB, id, content string) error {
	db.Exec("INSERT INTO memories (id, content) VALUES (?, ?)", id, content)
	return nil
}

// PLANTED BUG 2: counter is mutated from multiple goroutines with no
// synchronisation — a data race.
func countAll(items []string) int {
	counter := 0
	var wg sync.WaitGroup
	for range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter++
		}()
	}
	wg.Wait()
	return counter
}

// PLANTED BUG 3: rows is never closed, leaking a connection on every call.
func listIDs(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT id FROM memories")
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}
