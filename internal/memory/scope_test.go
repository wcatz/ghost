package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestScopeRoundTrips: scope is only useful if it survives the write and comes
// back intact. Without this the column exists and nothing can ever read a
// scope a caller supplied.
func TestScopeRoundTrips(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "scope.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/scope", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"Production uses PostgreSQL for the primary datastore.", "manual", 0.7, nil,
		UpsertOptions{Scope: map[string]string{"environment": "production", "component": "api"}})
	if err != nil {
		t.Fatalf("UpsertWithScope: %v", err)
	}

	got, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetByIDs: err=%v n=%d", err, len(got))
	}
	m := got[0]
	if m.Scope["environment"] != "production" {
		t.Errorf("Scope[environment] = %q, want production (full scope: %v)", m.Scope["environment"], m.Scope)
	}
	if m.Scope["component"] != "api" {
		t.Errorf("Scope[component] = %q, want api", m.Scope["component"])
	}
}

// TestScopeAbsentMeansUnscoped: a memory saved with no scope must read back
// with no scope rather than with an empty-but-present entry that would
// afterwards look like a deliberate choice of "".
func TestScopeAbsentMeansUnscoped(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "scope-none.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/scope-none", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "Helmfile uses SOPS.", "manual", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetByIDs: err=%v n=%d", err, len(got))
	}
	if len(got[0].Scope) != 0 {
		t.Errorf("Scope = %v, want empty — an unscoped memory must not acquire a scope nobody set", got[0].Scope)
	}
}

// TestScopeMatches pins the rule that makes scope usable in retrieval.
//
// A memory that does not MENTION a requested key is in scope. That is the
// decision that matters: a fact with no stated environment applies to every
// environment, and the alternative — hiding anything without an explicit
// match — would make missing scope a reason to suppress the most general,
// most reusable knowledge in the store. Only an explicit mismatch excludes.
//
// The inverse also holds: scope never invents agreement. A row that says
// environment=development does not satisfy a request for production, however
// similar the text reads.
func TestScopeMatches(t *testing.T) {
	cases := []struct {
		name  string
		scope map[string]string
		want  map[string]string
		in    bool
	}{
		{"explicit match", map[string]string{"environment": "production"}, map[string]string{"environment": "production"}, true},
		{"explicit mismatch excludes", map[string]string{"environment": "development"}, map[string]string{"environment": "production"}, false},
		{"unscoped row stays eligible", nil, map[string]string{"environment": "production"}, true},
		{"row lacking the key stays eligible", map[string]string{"component": "api"}, map[string]string{"environment": "production"}, true},
		{"extra keys on the row are irrelevant", map[string]string{"environment": "production", "component": "api"}, map[string]string{"environment": "production"}, true},
		{"both keys must agree", map[string]string{"environment": "production", "component": "web"}, map[string]string{"environment": "production", "component": "api"}, false},
		{"empty request matches everything", map[string]string{"environment": "production"}, map[string]string{}, true},
	}
	for _, c := range cases {
		if got := ScopeMatches(c.scope, c.want); got != c.in {
			t.Errorf("%s: ScopeMatches(%v, %v) = %v, want %v", c.name, c.scope, c.want, got, c.in)
		}
	}
}

// TestMigrateV12AddsScope and its fresh-DB twin: the column must exist on both
// paths, and a migrated row must keep NULL rather than a fabricated scope —
// inventing environment=production for every existing memory would be a claim
// about where knowledge applies that nobody made.
func TestMigrateV12AddsScope(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE memories DROP COLUMN scope`); err != nil {
		t.Skipf("this SQLite build cannot drop a column to simulate v11: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/a-very-long-v11-path', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'a memory written before scope existed')`,
		`PRAGMA user_version = 11`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrate(db, 11); err != nil {
		t.Fatalf("migrate v11->v12: %v", err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	var has int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('memories') WHERE name = 'scope'`,
	).Scan(&has); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if has != 1 {
		t.Error("memories.scope missing after migrateV12")
	}

	var scope sql.NullString
	var content string
	if err := db.QueryRow(`SELECT content, scope FROM memories WHERE id = 'm1'`).Scan(&content, &scope); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if content != "a memory written before scope existed" {
		t.Errorf("content changed: %q", content)
	}
	if scope.Valid {
		t.Errorf("scope = %v, want NULL — inventing a scope asserts where knowledge applies", scope.String)
	}
}

func TestMigrateFreshDBHasScope(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	var has int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('memories') WHERE name = 'scope'`,
	).Scan(&has); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if has != 1 {
		t.Error("memories.scope missing on a fresh database (initSQL)")
	}
}
