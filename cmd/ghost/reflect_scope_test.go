package main

import (
	"database/sql"
	"path/filepath"
)

// execBackdate rewrites the project's memory timestamps to an hour ago.
func execBackdate(dataHome, project string) error {
	db, err := sql.Open("sqlite", filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec(`UPDATE memories SET created_at = datetime('now', '-1 hour') WHERE project_id = ?`, project)
	return err
}
