package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestScopeRoundTrips(t *testing.T) {
	s := NewStore(mustOpenDB(t, filepath.Join(t.TempDir(), "scope.sqlite")), nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/scope", testProject); err != nil {
		t.Fatal(err)
	}
	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "Production uses PostgreSQL for the primary datastore.", "manual", 0.7, nil, UpsertOptions{Scope: map[string]string{"environment": "production", "component": "api"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetByIDs: %v %d", err, len(got))
	}
	if got[0].Scope["environment"] != "production" || got[0].Scope["component"] != "api" {
		t.Fatalf("scope=%v", got[0].Scope)
	}
}

func mustOpenDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestScopeAbsentMeansUnscoped(t *testing.T) {
	s := NewStore(mustOpenDB(t, filepath.Join(t.TempDir(), "scope-none.sqlite")), nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/scope-none", testProject); err != nil {
		t.Fatal(err)
	}
	id, _, _, err := s.Upsert(ctx, testProject, "fact", "Helmfile uses SOPS.", "manual", 0.7, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(got) != 1 || len(got[0].Scope) != 0 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

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
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestMigrateV12AddsScope(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE memories DROP COLUMN scope`); err != nil {
		t.Skipf("cannot drop scope: %v", err)
	}
	for _, s := range []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/a-very-long-v11-path', 'p1')`,
		`INSERT INTO memories (id, project_id, content) VALUES ('m1', 'p1', 'a memory written before scope existed')`,
		`PRAGMA user_version = 11`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, 11); err != nil {
		t.Fatal(err)
	}
	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Fatalf("user_version=%d want %d", v, schemaVersion)
	}
	var has int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('memories') WHERE name = 'scope'`).Scan(&has); err != nil || has != 1 {
		t.Fatalf("has=%d err=%v", has, err)
	}
	var scope sql.NullString
	var content string
	if err := db.QueryRow(`SELECT content, scope FROM memories WHERE id = 'm1'`).Scan(&content, &scope); err != nil {
		t.Fatal(err)
	}
	if content != "a memory written before scope existed" || scope.Valid {
		t.Fatalf("content=%q scope=%v", content, scope)
	}
}

func TestMigrateFreshDBHasScope(t *testing.T) {
	db := mustOpenDB(t, filepath.Join(t.TempDir(), "fresh.db"))
	var has int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('memories') WHERE name = 'scope'`).Scan(&has); err != nil || has != 1 {
		t.Fatalf("has=%d err=%v", has, err)
	}
}
