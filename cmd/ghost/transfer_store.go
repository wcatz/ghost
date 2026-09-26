package main

import (
	"database/sql"
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
	if err := requireMigratedSchema(db, dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}
	// A nil logger is honoured as silence: this store only reads.
	return memory.NewStore(db, nil), nil
}

// requireMigratedSchema refuses a store whose schema is behind the columns the
// transfer readers select, or ahead of this Ghost.
//
// The check exists because "read-only" is what makes this hard to get right. No
// migration runs on this path — deliberately, it is the read-only path — so a
// database from an older Ghost is missing projects.repo_remote (v11) and
// memories.scope (v12), and the first query fails with SQLite's own "no such
// column: repo_remote". That names a column, which reads as a Ghost bug rather
// than as "this store is older than this Ghost", and it lands on exactly the
// machine the docs tell a user to prepare: a fresh install, or a binary upgraded
// but never yet opened against its own database. Meanwhile `ghost import
// --apply`, which opens read-write, migrates and works, and `ghost backup` works
// too, because VACUUM INTO reads no columns — so a user comparing the three sees
// the "safe copy" succeed and the export fail for no stated reason.
//
// The remedy sentence is the one the missing-database case already uses, because
// it is the same remedy: a read-write open, which is a session or `ghost mcp
// init`. A store from a NEWER Ghost is reported separately and does not get that
// sentence — OpenDB already refuses it, and "migrate" is not what that user
// should do.
func requireMigratedSchema(db *sql.DB, dbPath string) error {
	version, err := memory.DBUserVersion(db)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	switch {
	case version < memory.SchemaVersion():
		return fmt.Errorf("the database at %s is at schema v%d and this Ghost reads v%d — start a session, or run ghost mcp init, to migrate it before exporting it or previewing an import into it",
			dbPath, version, memory.SchemaVersion())
	case version > memory.SchemaVersion():
		return fmt.Errorf("the database at %s is at schema v%d, which is newer than this Ghost (v%d) — upgrade Ghost, or point at a different store",
			dbPath, version, memory.SchemaVersion())
	}
	return nil
}

// openImportStore opens the database for an import run whose apply flag is
// `apply`.
//
// A dry run opens read-only, and that is the whole reason this is a function
// rather than a line in runImport. bootstrap() is a read-write open, and a
// read-write open is not a neutral way to look: memory.OpenDB runs migrations
// and stamps user_version, and the caller then seeds the builtin global rows. So
// a dry run reached through bootstrap() could migrate a database whose schema is
// behind, insert rows, and report "nothing written" while having written them —
// a preview that changes the thing it is previewing. The import's dry run writes
// nothing on every path (every store import method returns before its INSERT when
// apply is false), so the read-only connection loses no capability it needs.
//
// The apply path goes through bootstrap as every other writing command does, so
// it still fails on a config file that does not parse, and still gets the
// logger and the demotion threshold.
func openImportStore(apply bool) (*memory.Store, error) {
	if apply {
		_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
		return store, nil
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return nil, err
	}
	return openReadOnlyTransferStore(dataDir)
}
