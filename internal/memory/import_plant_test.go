package memory

import (
	"context"
	"testing"
)

// The four helpers below plant rows in SQL under ids a caller chose, which is the
// only way to reach a state this build refuses to create.
//
// That state is real, and it is the whole reason the importers have to be careful
// about what they do with a record they would otherwise refuse: a store written
// before a guard landed, one restored from a snapshot an older Ghost took, or one
// a person edited in place. `ghost import` is handed such a store, and re-running
// an import over it must stay a no-op — the portable format's "never overwrites an
// id that already exists, so re-running is always safe" is a promise about exactly
// this case.
//
// The importers are the only place these can be written: `ImportMemory` refuses
// the id at the boundary, `CreateFromCorpus` and `RestoreSnapshot` write
// byte-exact data by design, and the task/decision/project importers all refuse
// their id shapes for the same reason the memory one does.

// plantRawMemory writes one memory under a caller-chosen id.
func plantRawMemory(t *testing.T, s *Store, id, projectID, content string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at)
		 VALUES (?, ?, 'gotcha', ?, 'mcp', datetime('now'), datetime('now'))`,
		id, projectID, content); err != nil {
		t.Fatalf("plant a memory under id %q: %v", id, err)
	}
}

// plantRawTask writes one task under a caller-chosen id.
func plantRawTask(t *testing.T, s *Store, id, projectID, title string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, project_id, title, description, status, priority, created_at, updated_at)
		 VALUES (?, ?, ?, '', 'pending', 2, datetime('now'), datetime('now'))`,
		id, projectID, title); err != nil {
		t.Fatalf("plant a task under id %q: %v", id, err)
	}
}

// plantRawDecision writes one decision under a caller-chosen id.
func plantRawDecision(t *testing.T, s *Store, id, projectID, title, decision, rationale string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO decisions (id, project_id, title, decision, rationale, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'active', datetime('now'), datetime('now'))`,
		id, projectID, title, decision, rationale); err != nil {
		t.Fatalf("plant a decision under id %q: %v", id, err)
	}
}

// plantRawProject writes one project under a caller-chosen id.
func plantRawProject(t *testing.T, s *Store, id, path, name string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, path, name); err != nil {
		t.Fatalf("plant a project under id %q: %v", id, err)
	}
}

// memoryBody reads a memory's stored content back, so a test can tell a skip from
// an overwrite. It reads through the store's own database handle rather than a
// public accessor because the point is what is ON DISK.
func memoryBody(t *testing.T, s *Store, id string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(context.Background(), `SELECT content FROM memories WHERE id = ?`, id).
		Scan(&content); err != nil {
		t.Fatalf("read back the memory under id %q: %v", id, err)
	}
	return content
}
