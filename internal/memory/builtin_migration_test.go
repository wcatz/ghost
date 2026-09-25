package memory

import (
	"database/sql"
	"testing"
)

func TestMigrateV15RelabelsBuiltinSeed(t *testing.T) {
	path := newV3DB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v3 database: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert global project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO memories (project_id, category, content, source, importance, tags, pinned)
		VALUES ('_global', 'preference', 'NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.', 'manual', 1.0, '[]', 0)
	`); err != nil {
		t.Fatalf("insert legacy builtin seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v3 database: %v", err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB after migration: %v", err)
	}
	defer migrated.Close() //nolint:errcheck

	var source string
	var pinned bool
	if err := migrated.QueryRow(`
		SELECT source, pinned FROM memories
		WHERE project_id = '_global' AND content LIKE '%Co-Authored-By%'
	`).Scan(&source, &pinned); err != nil {
		t.Fatalf("read migrated seed: %v", err)
	}
	if source != "builtin" || pinned {
		t.Errorf("migrated legacy seed = source %q pinned %t, want builtin/unpinned", source, pinned)
	}
}
