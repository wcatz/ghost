package main

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/wcatz/ghost/internal/config"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/memory"
)

func dataDirPath() (string, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	return dir, nil
}

const (
	exportOperation         = "exporting it or previewing an import into it"
	exportOrImportOperation = exportOperation
)

func openReadOnlyTransferStore(dataDir, operation string) (*memory.Store, error) {
	store, db, dbPath, err := openReadOnlyTransferStoreUnchecked(dataDir)
	if err != nil {
		return nil, err
	}
	if err := requireMigratedSchema(db, dbPath, operation); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func openReadOnlyTransferStoreUnchecked(dataDir string) (*memory.Store, *sql.DB, string, error) {
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		if errors.Is(err, memory.ErrNoDatabase) || os.IsNotExist(err) {
			return nil, nil, dbPath, fmt.Errorf("no database at %s — start a session, or run ghost mcp init, first", dbPath)
		}
		return nil, nil, dbPath, fmt.Errorf("open %s: %w", dbPath, err)
	}
	return memory.NewStore(db, nil), db, dbPath, nil
}

func requireMigratedSchema(db *sql.DB, dbPath, operation string) error {
	version, err := memory.DBUserVersion(db)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	switch {
	case version < memory.SchemaVersion():
		return fmt.Errorf("the database at %s is at schema v%d and this Ghost reads v%d — start a session, or run ghost mcp init, to migrate it before %s",
			dbPath, version, memory.SchemaVersion(), operation)
	case version > memory.SchemaVersion():
		return fmt.Errorf("the database at %s is at schema v%d, which is newer than this Ghost (v%d) — upgrade Ghost, or point at a different store",
			dbPath, version, memory.SchemaVersion())
	}
	return nil
}

func openImportStore(apply bool) (*memory.Store, error) {
	if apply {
		_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
		return store, nil
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return nil, err
	}
	return openReadOnlyTransferStore(dataDir, exportOrImportOperation)
}
