package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const secretFixture = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

func memoryContainingSecret(t *testing.T, s *Store) string {
	t.Helper()
	var id string
	if err := s.db.QueryRowContext(context.Background(), `INSERT INTO memories (project_id, category, content, source, importance, tags) VALUES (?, 'fact', ?, 'mcp', 0.7, '[]') RETURNING id`, testProject, "the token is "+secretFixture).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func projectMemoryCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM memories WHERE project_id = ?`, testProject).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func storedContent(t *testing.T, s *Store, id string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(context.Background(), `SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

func assertRefusal(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrSecretContent) || !strings.Contains(err.Error(), wantField) || strings.Contains(err.Error(), secretFixture) {
		t.Fatalf("err=%v wantField=%q", err, wantField)
	}
}

// #553
func TestUpsertRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	_, _, _, err := s.Upsert(context.Background(), testProject, "fact", "the deploy token is "+secretFixture, "mcp", 0.7, nil)
	assertRefusal(t, err, "content")
	if n := projectMemoryCount(t, s); n != 0 {
		t.Fatalf("count=%d", n)
	}
}

func TestUpsertWithOptionsRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	_, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "global note: "+secretFixture, "mcp", 0.7, nil, UpsertOptions{})
	assertRefusal(t, err, "content")
	if n := projectMemoryCount(t, s); n != 0 {
		t.Fatalf("count=%d", n)
	}
}

func TestUpsertAllowsOrdinaryCredentialTalk(t *testing.T) {
	for _, content := range []string{
		"The api key for this service is sk-live-123.",
		"Rotate the deployment password every quarter.",
		"The CI access_token lives in the runner env.",
		"GITHUB_TOKEN=ghp_example",
		"CLIENT_SECRET: rotate this value",
		"api-key: example-secret",
		"The KES signing key lives in /etc/cardano/kes/keys and is never in git.",
		"password: ${SECRET_MANAGER_DB_PASSWORD}",
	} {
		t.Run(content, func(t *testing.T) {
			s := testStore(t)
			if _, _, _, err := s.Upsert(context.Background(), testProject, "fact", content, "mcp", 0.7, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpdateMemoryRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, err := s.Upsert(ctx, testProject, "fact", "the deploy token is stored in vault", "mcp", 0.7, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.UpdateMemory(ctx, testProject, id, strPtr("now it is "+secretFixture), nil, nil, nil)
	assertRefusal(t, err, "content")
	if got := storedContent(t, s, id); got != "the deploy token is stored in vault" {
		t.Fatalf("content=%q", got)
	}
}

func TestUpdateMemoryGuardsTheIncomingContentOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := memoryContainingSecret(t, s)
	t.Run("metadata-only edit", func(t *testing.T) {
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, float32Ptr(0.2), []string{"legacy"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rewriting the content without the value", func(t *testing.T) {
		if err := s.UpdateMemory(ctx, testProject, id, strPtr("the deploy token is in the vault"), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(storedContent(t, s, id), secretFixture) {
			t.Fatal("value still present")
		}
	})
}

func TestRecordDecisionRefusesCredentialContent(t *testing.T) {
	cases := []struct{ field, title, decision, rationale string }{
		{"title", "rotate the " + secretFixture, "rotate quarterly", "operational hygiene"},
		{"decision", "rotation", "store " + secretFixture + " in the vault", "operational hygiene"},
		{"rationale", "rotation", "rotate quarterly", "the current value is " + secretFixture},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.RecordDecision(context.Background(), testProject, tc.title, tc.decision, tc.rationale, nil, nil)
			assertRefusal(t, err, tc.field)
			var decisions int
			if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM decisions WHERE project_id = ?`, testProject).Scan(&decisions); err != nil || decisions != 0 {
				t.Fatalf("decisions=%d err=%v", decisions, err)
			}
			if n := projectMemoryCount(t, s); n != 0 {
				t.Fatalf("memories=%d", n)
			}
		})
	}
}

func TestCreateTaskRefusesCredentialContent(t *testing.T) {
	cases := []struct{ field, title, description string }{
		{"title", "rotate " + secretFixture, "quarterly"},
		{"description", "rotation", "the current value is " + secretFixture},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := testStore(t)
			_, err := s.CreateTask(context.Background(), testProject, tc.title, tc.description, 2)
			assertRefusal(t, err, tc.field)
			var tasks int
			if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM tasks WHERE project_id = ?`, testProject).Scan(&tasks); err != nil || tasks != 0 {
				t.Fatalf("tasks=%d err=%v", tasks, err)
			}
		})
	}
}

func TestUpdateTaskRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateTask(ctx, testProject, "rotate the token", "quarterly", 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateTask(ctx, id, nil, nil, strPtr("the current value is "+secretFixture))
	assertRefusal(t, err, "description")
	var description string
	if err := s.db.QueryRowContext(ctx, `SELECT description FROM tasks WHERE id = ?`, id).Scan(&description); err != nil || description != "quarterly" {
		t.Fatalf("description=%q err=%v", description, err)
	}
}

func TestCompleteTaskRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateTask(ctx, testProject, "rotation", "quarterly", 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteTask(ctx, id, "done, the value was "+secretFixture)
	assertRefusal(t, err, "notes")
	var status, notes string
	if err := s.db.QueryRowContext(ctx, `SELECT status, notes FROM tasks WHERE id = ?`, id).Scan(&status, &notes); err != nil {
		t.Fatal(err)
	}
	if status == "done" || notes != "" {
		t.Fatalf("status=%q notes=%q", status, notes)
	}
}

func TestPortableImportRefusesCredentialContent(t *testing.T) {
	ctx := context.Background()
	t.Run("memory content", func(t *testing.T) {
		for _, apply := range []bool{false, true} {
			store := portableTestStore(t)
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := store.ImportMemory(ctx, PortableMemory{ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp", Content: "the token is " + secretFixture}, ImportOptions{Apply: apply})
			if err == nil || !errors.Is(err, ErrSecretContent) || strings.Contains(err.Error(), secretFixture) {
				t.Fatalf("apply=%v err=%v", apply, err)
			}
			if n := countPortableRows(t, store, "memories"); n != 0 {
				t.Fatalf("count=%d", n)
			}
		}
	})
	t.Run("task notes", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.ImportTask(ctx, Task{ID: "t1", ProjectID: "p1", Title: "rotation", Description: "quarterly", Status: "done", Notes: "the value was " + secretFixture}, true)
		assertRefusal(t, err, "notes")
		if n := countPortableRows(t, store, "tasks"); n != 0 {
			t.Fatalf("count=%d", n)
		}
	})
	t.Run("decision rationale", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.ImportDecision(ctx, Decision{ID: "d1", ProjectID: "p1", Title: "rotation", Status: "active", Decision: "rotate quarterly", Rationale: "the value is " + secretFixture}, true)
		assertRefusal(t, err, "rationale")
		if n := countPortableRows(t, store, "decisions"); n != 0 {
			t.Fatalf("count=%d", n)
		}
	})
}

func TestPortableImportSkipsAnAlreadyImportedRecordHoldingACredential(t *testing.T) {
	ctx := context.Background()
	withCredential := "the token is " + secretFixture
	t.Run("memory", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatal(err)
		}
		rec := PortableMemory{ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp", Content: withCredential, CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00"}
		plantPortableMemory(t, store, rec)
		for _, apply := range []bool{false, true} {
			created, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: apply})
			if err != nil || created {
				t.Fatalf("apply=%v created=%v err=%v", apply, created, err)
			}
		}
	})
	t.Run("task", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatal(err)
		}
		rec := Task{ID: "t1", ProjectID: "p1", Title: "rotation", Description: withCredential, Status: "pending"}
		plantPortableTask(t, store, rec)
		for _, apply := range []bool{false, true} {
			created, _, err := store.ImportTask(ctx, rec, apply)
			if err != nil || created {
				t.Fatalf("apply=%v created=%v err=%v", apply, created, err)
			}
		}
	})
	t.Run("decision", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatal(err)
		}
		rec := Decision{ID: "d1", ProjectID: "p1", Title: "rotation", Status: "active", Decision: "rotate quarterly", Rationale: withCredential}
		plantPortableDecision(t, store, rec)
		for _, apply := range []bool{false, true} {
			created, _, err := store.ImportDecision(ctx, rec, apply)
			if err != nil || created {
				t.Fatalf("apply=%v created=%v err=%v", apply, created, err)
			}
		}
	})
}

func plantPortableMemory(t *testing.T, s *Store, m PortableMemory) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO memories (id, project_id, category, content, source, importance, tags) VALUES (?, ?, ?, ?, 'mcp', 0.7, '[]')`, m.ID, m.ProjectID, m.Category, m.Content); err != nil {
		t.Fatal(err)
	}
}

func plantPortableTask(t *testing.T, s *Store, task Task) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, ?, ?, ?, ?, ?)`, task.ID, task.ProjectID, task.Title, task.Description, task.Status, task.Priority); err != nil {
		t.Fatal(err)
	}
}

func plantPortableDecision(t *testing.T, s *Store, d Decision) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale, status, tags) VALUES (?, ?, ?, ?, '[]', ?, ?, '[]')`, d.ID, d.ProjectID, d.Title, d.Decision, d.Rationale, d.Status); err != nil {
		t.Fatal(err)
	}
}

func countPortableRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordDecisionRefusesCredentialAlternatives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, _, _, err := s.RecordDecision(ctx, testProject, "rotation", "rotate quarterly", "operational hygiene", []string{"keep it in the vault", "the old one was " + secretFixture}, nil)
	if !errors.Is(err, ErrSecretContent) || !strings.Contains(err.Error(), "alternatives[1]") || strings.Contains(err.Error(), secretFixture) {
		t.Fatalf("err=%v", err)
	}
	var decisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM decisions WHERE project_id = ?`, testProject).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("decisions=%d err=%v", decisions, err)
	}
}

func TestUpdateLearnedContextRefusesCredentialContent(t *testing.T) {
	cases := []struct{ field, learned, summary, wantName string }{
		{"learned_context", "the deploy token is " + secretFixture, "a summary", "learned_context"},
		{"reflection_summary", "a summary", "about the token " + secretFixture, "reflection_summary"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			err := s.UpdateLearnedContext(ctx, testProject, tc.learned, tc.summary)
			assertRefusal(t, err, tc.wantName)
			if err := s.UpdateLearnedContext(ctx, testProject, "an ordinary summary", "of the project"); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetLearnedContext(ctx, testProject)
			if err != nil || got != "an ordinary summary" {
				t.Fatalf("got=%q err=%v", got, err)
			}
		})
	}
}

func TestCreateRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "the token is " + secretFixture, Source: "mcp", Importance: 0.7})
	assertRefusal(t, err, "content")
	if n := projectMemoryCount(t, s); n != 0 {
		t.Fatalf("count=%d", n)
	}
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "an ordinary fact worth keeping", Source: "mcp", Importance: 0.7}); err != nil {
		t.Fatal(err)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Fatalf("count=%d", n)
	}
}

func TestCreateFromCorpusSkipsTheGuard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateFromCorpus(ctx, testProject, Memory{Category: "fact", Content: "password: dckr\\_pat\\_QfMnjESK8cyTT6FTQ2tXbEeTubs", Source: "mcp", Importance: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedContent(t, s, id); !strings.Contains(got, "dckr") {
		t.Fatalf("content=%q", got)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Fatalf("count=%d", n)
	}
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "password: dckr\\_pat\\_QfMnjESK8cyTT6FTQ2tXbEeTubs", Source: "mcp", Importance: 0.7}); !errors.Is(err, ErrSecretContent) {
		t.Fatalf("err=%v", err)
	}
}

func TestSecretRefusalCarriesNoStoreWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", secretFixture, "mcp", 0.7, nil); err == nil {
		t.Fatal("expected refusal")
	}
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", "an ordinary fact worth keeping", "mcp", 0.7, nil); err != nil {
		t.Fatal(err)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Fatalf("count=%d", n)
	}
}

func float32Ptr(f float32) *float32 { return &f }
