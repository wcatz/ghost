package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// dataDirPath resolves the Ghost data directory without creating it. The backup
// and export commands use it to place their default files beside the database,
// and neither should leave a data directory behind on a machine that has none —
// a backup of a store that does not exist is an error, not a reason to make an
// empty one.
func dataDirPath() (string, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	return dir, nil
}

// openReadOnlyTransferStore opens the database at dataDir for reading only,
// without running migrations, seeding builtin memories, or creating the file.
//
// An export must be safe to run against a live MCP server and must change
// nothing. A read-write open would migrate a database whose schema is behind and
// seed rows into it, so "export" would be a write — and a user inspecting their
// store would not expect a file to appear because they ran a read. That is also
// why the missing-database case is named here rather than left as a stat error:
// the actionable next step is to start a session.
func openReadOnlyTransferStore(dataDir string) (*memory.Store, error) {
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDBReadOnly(dbPath)
	if err != nil {
		if errors.Is(err, memory.ErrNoDatabase) || os.IsNotExist(err) {
			return nil, fmt.Errorf("no database at %s — start a session, or run ghost mcp init, first", dbPath)
		}
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	// A nil logger is honoured as silence: this store only reads.
	return memory.NewStore(db, nil), nil
}
