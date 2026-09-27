package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// secretFixture is a value no innocent memory contains and every rule in
// internal/secret has a shape for. Using one fixture across the store tests
// keeps a change to the detector from moving these tests: they are about which
// write paths refuse, not about which formats are recognised.
const secretFixture = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

// memoryContainingSecret saves secretFixture and returns its ID, bypassing the
// guard under test the way a database written before the guard existed would
// hold it: a raw statement on the same store. Used to prove the guard is about
// incoming text, not about rows.
func memoryContainingSecret(t *testing.T, s *Store) string {
	t.Helper()
	var id string
	err := s.db.QueryRowContext(context.Background(), `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'fact', ?, 'mcp', 0.7, '[]')
		RETURNING id
	`, testProject, "the token is "+secretFixture).Scan(&id)
	if err != nil {
		t.Fatalf("seed a legacy memory holding a secret: %v", err)
	}
	return id
}

func projectMemoryCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE project_id = ?`, testProject).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

func storedContent(t *testing.T, s *Store, id string) string {
	t.Helper()
	var content string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read memory %s: %v", id, err)
	}
	return content
}

// assertRefusal checks the two properties a caller depends on: the error is
// recognisable as this refusal and not as a database failure, and it does not
// carry the value that caused it.
func assertRefusal(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("write was accepted, want a refusal naming field %q", wantField)
	}
	if !errors.Is(err, ErrSecretContent) {
		t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
	}
	if !strings.Contains(err.Error(), wantField) {
		t.Errorf("refusal does not name the field %q: %v", wantField, err)
	}
	if strings.Contains(err.Error(), secretFixture) {
		t.Errorf("refusal echoes the value it refused: %v", err)
	}
}

// TestUpsertRefusesCredentialContent is the core of issue #553: the save path
// the MCP tools, the first-contact import, and reflection's own writes all
// converge on had no check at all, so a credential pasted by an agent was
// stored, embedded, injected into later sessions, mirrored to the Obsidian
// vault, and sent to a third-party model in the next reflection prompt.
func TestUpsertRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, _, _, err := s.Upsert(ctx, testProject, "fact", "the deploy token is "+secretFixture, "mcp", 0.7, nil)
	assertRefusal(t, err, "content")

	if n := projectMemoryCount(t, s); n != 0 {
		t.Errorf("a refused save stored %d memories, want 0", n)
	}
}

// TestUpsertWithOptionsRefusesCredentialContent covers the same refusal on the
// provenance/scope entry point, which is what ghost_save_global and reflection
// candidate promotion actually call. A guard only on Upsert would leave
// save_global — the tool whose whole purpose is to widen a memory's reach — as
// the way around it.
func TestUpsertWithOptionsRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"global note: "+secretFixture, "mcp", 0.7, nil, UpsertOptions{})
	assertRefusal(t, err, "content")

	if n := projectMemoryCount(t, s); n != 0 {
		t.Errorf("a refused save stored %d memories, want 0", n)
	}
}

// TestUpsertAllowsOrdinaryCredentialTalk is the false-positive guard at the
// store boundary, where a false positive is a blocked save rather than a
// dropped test case. The strings are the ones this repo's own reflection tier
// already treats as ordinary knowledge about credentials.
func TestUpsertAllowsOrdinaryCredentialTalk(t *testing.T) {
	cases := []string{
		"The api key for this service is sk-live-123.",
		"Rotate the deployment password every quarter.",
		"The CI access_token lives in the runner env.",
		"GITHUB_TOKEN=ghp_example",
		"CLIENT_SECRET: rotate this value",
		"api-key: example-secret",
		"The KES signing key lives in /etc/cardano/kes/keys and is never in git.",
		"password: ${SECRET_MANAGER_DB_PASSWORD}",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			s := testStore(t)
			if _, _, _, err := s.Upsert(context.Background(), testProject, "fact", content, "mcp", 0.7, nil); err != nil {
				t.Errorf("Upsert(%q) = %v, want it stored", content, err)
			}
		})
	}
}

// TestUpdateMemoryRefusesCredentialContent covers ghost_memory_update. An edit
// is a save path with the same blast radius: the new text is re-embedded, read
// back into context, and mirrored, so a guard on insert alone would leave
// "read it, then rewrite it with the value inlined" as the way around it.
func TestUpdateMemoryRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, err := s.Upsert(ctx, testProject, "fact", "the deploy token is stored in vault", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = s.UpdateMemory(ctx, testProject, id, strPtr("now it is "+secretFixture), nil, nil, nil)
	assertRefusal(t, err, "content")

	if got := storedContent(t, s, id); got != "the deploy token is stored in vault" {
		t.Errorf("a refused update changed the stored content to %q", got)
	}
}

// TestUpdateMemoryGuardsTheIncomingContentOnly pins the boundary: the guard
// reads what the caller is trying to store, not what the row already holds. A
// database written before this guard can hold a credential, and a user who
// wants it gone must still be able to retag, re-categorise, or re-prioritise
// that row on the way to deleting it. Refusing a metadata-only edit would
// strand the row with no way to touch it but ghost_memory_delete.
func TestUpdateMemoryGuardsTheIncomingContentOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := memoryContainingSecret(t, s)

	t.Run("metadata-only edit", func(t *testing.T) {
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, float32Ptr(0.2), []string{"legacy"}); err != nil {
			t.Errorf("a tags-and-importance edit of a legacy row was refused: %v", err)
		}
	})

	t.Run("rewriting the content without the value", func(t *testing.T) {
		if err := s.UpdateMemory(ctx, testProject, id, strPtr("the deploy token is in the vault"), nil, nil, nil); err != nil {
			t.Errorf("rewriting a legacy row's content without the value was refused: %v", err)
		}
		if strings.Contains(storedContent(t, s, id), secretFixture) {
			t.Error("the rewrite did not remove the stored credential")
		}
	})
}

func TestRecordDecisionRefusesCredentialContent(t *testing.T) {
	cases := []struct {
		field     string
		title     string
		decision  string
		rationale string
	}{
		{"title", "rotate the " + secretFixture, "rotate quarterly", "operational hygiene"},
		{"decision", "rotation", "store " + secretFixture + " in the vault", "operational hygiene"},
		{"rationale", "rotation", "rotate quarterly", "the current value is " + secretFixture},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := testStore(t)
			_, _, _, err := s.RecordDecision(context.Background(), testProject,
				tc.title, tc.decision, tc.rationale, nil, nil)
			assertRefusal(t, err, tc.field)

			var decisions int
			if err := s.db.QueryRowContext(context.Background(),
				`SELECT count(*) FROM decisions WHERE project_id = ?`, testProject).Scan(&decisions); err != nil {
				t.Fatalf("count decisions: %v", err)
			}
			if decisions != 0 {
				t.Errorf("a refused decision stored %d rows, want 0", decisions)
			}
			// The companion memory is a second copy of the same text, so it
			// has to be refused with the decision or the refusal is cosmetic.
			if n := projectMemoryCount(t, s); n != 0 {
				t.Errorf("a refused decision stored %d companion memories, want 0", n)
			}
		})
	}
}

func TestCreateTaskRefusesCredentialContent(t *testing.T) {
	cases := []struct {
		field       string
		title       string
		description string
	}{
		{"title", "rotate " + secretFixture, "quarterly"},
		{"description", "rotation", "the current value is " + secretFixture},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := testStore(t)
			_, err := s.CreateTask(context.Background(), testProject, tc.title, tc.description, 2)
			assertRefusal(t, err, tc.field)

			var tasks int
			if err := s.db.QueryRowContext(context.Background(),
				`SELECT count(*) FROM tasks WHERE project_id = ?`, testProject).Scan(&tasks); err != nil {
				t.Fatalf("count tasks: %v", err)
			}
			if tasks != 0 {
				t.Errorf("a refused task stored %d rows, want 0", tasks)
			}
		})
	}
}

func TestUpdateTaskRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateTask(ctx, testProject, "rotate the token", "quarterly", 2)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = s.UpdateTask(ctx, id, nil, nil, strPtr("the current value is "+secretFixture))
	assertRefusal(t, err, "description")

	var description string
	if err := s.db.QueryRowContext(ctx, `SELECT description FROM tasks WHERE id = ?`, id).Scan(&description); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if description != "quarterly" {
		t.Errorf("a refused task update changed the description to %q", description)
	}
}

// TestCompleteTaskRefusesCredentialNotes covers the last text write in the
// schema and the one that had no length cap either: a completion note is where
// "here is the value that fixed it" gets written, and it is rendered by
// ghost_task_list into the next session's context.
func TestCompleteTaskRefusesCredentialContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateTask(ctx, testProject, "rotation", "quarterly", 2)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = s.CompleteTask(ctx, id, "done, the value was "+secretFixture)
	assertRefusal(t, err, "notes")

	var status, notes string
	if err := s.db.QueryRowContext(ctx, `SELECT status, notes FROM tasks WHERE id = ?`, id).Scan(&status, &notes); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status == "done" {
		t.Error("a refused completion still marked the task done")
	}
	if notes != "" {
		t.Errorf("a refused completion stored notes %q", notes)
	}
}

// TestPortableImportRefusesCredentialContent covers the write path the first
// version of this guard left open, and it is the one that should worry anyone
// least omitted: `ghost import <file> --apply` takes a JSONL artifact that
// arrived from somewhere, and all three importers wrote it straight through
// with raw INSERTs — no Upsert, so no guard. A credential planted in an
// artifact was then embedded, replayed into every later session, mirrored to the
// vault, and quoted into the next reflect prompt. The least trusted input in
// the system was the one input with no check.
//
// The refusal has to happen BEFORE the apply=false early return, or a dry run
// would preview an import that the real run then refuses — the property
// ImportMemory's own doc claims for every other validation it performs.
func TestPortableImportRefusesCredentialContent(t *testing.T) {
	ctx := context.Background()

	t.Run("memory content", func(t *testing.T) {
		for _, apply := range []bool{false, true} {
			store := portableTestStore(t)
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			_, _, _, err := store.ImportMemory(ctx, PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
				Content: "the token is " + secretFixture,
			}, ImportOptions{Apply: apply})
			if err == nil {
				t.Fatalf("apply=%v: ImportMemory accepted a credential", apply)
			}
			if !errors.Is(err, ErrSecretContent) {
				t.Errorf("apply=%v: errors.Is(err, ErrSecretContent) = false for %v", apply, err)
			}
			if strings.Contains(err.Error(), secretFixture) {
				t.Errorf("apply=%v: refusal echoes the value: %v", apply, err)
			}
			if n := countPortableRows(t, store, "memories"); n != 0 {
				t.Errorf("apply=%v: imported %d memories, want 0", apply, n)
			}
		}
	})

	t.Run("task notes", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "rotation", Description: "quarterly",
			Status: "done", Notes: "the value was " + secretFixture,
		}, true)
		assertRefusal(t, err, "notes")
		if n := countPortableRows(t, store, "tasks"); n != 0 {
			t.Errorf("imported %d tasks, want 0", n)
		}
	})

	t.Run("decision rationale", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "rotation", Status: "active",
			Decision: "rotate quarterly", Rationale: "the value is " + secretFixture,
		}, true)
		assertRefusal(t, err, "rationale")
		if n := countPortableRows(t, store, "decisions"); n != 0 {
			t.Errorf("imported %d decisions, want 0", n)
		}
	})
}

// TestPortableImportSkipsAnAlreadyImportedRecordHoldingACredential pins that the
// guard does not change what an import does to a row it is not going to write.
// The portable format promises "Import never overwrites an id that already
// exists, so re-running is always safe", and an importer answers a
// already-present id with a skip — so a record that is present must keep
// answering "skip" even when its text holds a credential, because nothing is
// being stored and the refusal would protect nothing. It matters in practice:
// re-importing an artifact a pre-guard build exported, or one whose records
// already landed on the destination, would otherwise fail with
// "fix them in the artifact and re-run, which is safe" on rows that were
// previously no-ops.
func TestPortableImportSkipsAnAlreadyImportedRecordHoldingACredential(t *testing.T) {
	ctx := context.Background()
	withCredential := "the token is " + secretFixture

	t.Run("memory", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		rec := PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: withCredential, CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
		}
		// Planted with a raw statement, standing in for a row a pre-guard build
		// exported.
		plantPortableMemory(t, store, rec)

		for _, apply := range []bool{false, true} {
			created, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: apply})
			if err != nil {
				t.Errorf("apply=%v: an already-present id was refused: %v", apply, err)
			}
			if created {
				t.Errorf("apply=%v: created = true for an already-present id", apply)
			}
		}
	})

	t.Run("task", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		rec := Task{ID: "t1", ProjectID: "p1", Title: "rotation", Description: withCredential, Status: "pending"}
		plantPortableTask(t, store, rec)

		for _, apply := range []bool{false, true} {
			created, err := store.ImportTask(ctx, rec, apply)
			if err != nil {
				t.Errorf("apply=%v: an already-present id was refused: %v", apply, err)
			}
			if created {
				t.Errorf("apply=%v: created = true for an already-present id", apply)
			}
		}
	})

	t.Run("decision", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		rec := Decision{
			ID: "d1", ProjectID: "p1", Title: "rotation", Status: "active",
			Decision: "rotate quarterly", Rationale: withCredential,
		}
		plantPortableDecision(t, store, rec)

		for _, apply := range []bool{false, true} {
			created, err := store.ImportDecision(ctx, rec, apply)
			if err != nil {
				t.Errorf("apply=%v: an already-present id was refused: %v", apply, err)
			}
			if created {
				t.Errorf("apply=%v: created = true for an already-present id", apply)
			}
		}
	})
}

func plantPortableMemory(t *testing.T, s *Store, m PortableMemory) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO memories (id, project_id, category, content, source, importance, tags)
		VALUES (?, ?, ?, ?, 'mcp', 0.7, '[]')
	`, m.ID, m.ProjectID, m.Category, m.Content); err != nil {
		t.Fatalf("plant memory %s: %v", m.ID, err)
	}
}

func plantPortableTask(t *testing.T, s *Store, task Task) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO tasks (id, project_id, title, description, status, priority)
		VALUES (?, ?, ?, ?, ?, ?)
	`, task.ID, task.ProjectID, task.Title, task.Description, task.Status, task.Priority); err != nil {
		t.Fatalf("plant task %s: %v", task.ID, err)
	}
}

func plantPortableDecision(t *testing.T, s *Store, d Decision) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale, status, tags)
		VALUES (?, ?, ?, ?, '[]', ?, ?, '[]')
	`, d.ID, d.ProjectID, d.Title, d.Decision, d.Rationale, d.Status); err != nil {
		t.Fatalf("plant decision %s: %v", d.ID, err)
	}
}

func countPortableRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	// The table name is a test literal from the three above, never input.
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestRecordDecisionRefusesCredentialAlternatives closes the narrower gap the
// first version left in the decision tool itself. `alternatives` is a list of
// strings rendered by ghost_decisions_list straight back to the agent, so a
// credential in one entry is stored and replayed exactly like one in the
// rationale — the field is just a list, and the guard has to walk it.
func TestRecordDecisionRefusesCredentialAlternatives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, _, _, err := s.RecordDecision(ctx, testProject, "rotation", "rotate quarterly",
		"operational hygiene", []string{"keep it in the vault", "the old one was " + secretFixture}, nil)
	if !errors.Is(err, ErrSecretContent) {
		t.Fatalf("errors.Is(err, ErrSecretContent) = false for %v", err)
	}
	// The index, not just the field: with several alternatives, "alternatives"
	// alone does not say which entry to fix.
	if !strings.Contains(err.Error(), "alternatives[1]") {
		t.Errorf("refusal does not name the offending entry: %v", err)
	}
	if strings.Contains(err.Error(), secretFixture) {
		t.Errorf("refusal echoes the value: %v", err)
	}

	var decisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM decisions WHERE project_id = ?`, testProject).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 0 {
		t.Errorf("a refused decision stored %d rows, want 0", decisions)
	}
}

// TestUpdateLearnedContextRefusesCredentialContent covers the last
// model-written field the reflection round produces. It is injected verbatim
// into every later session for the project, and unlike the memories it
// summarises it is never re-derived, so a credential the model quoted into its
// summary would sit in the context of every future session with nothing
// downstream that would ever rewrite it.
func TestUpdateLearnedContextRefusesCredentialContent(t *testing.T) {
	cases := []struct {
		field    string
		learned  string
		summary  string
		wantName string
	}{
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
				t.Fatalf("an ordinary learned context was refused: %v", err)
			}
			got, err := s.GetLearnedContext(ctx, testProject)
			if err != nil {
				t.Fatalf("read learned context: %v", err)
			}
			if got != "an ordinary summary" {
				t.Errorf("learned context = %q, want the ordinary summary; the refusal wrote something", got)
			}
		})
	}
}

// TestSecretRefusalCarriesNoStoreWrites is the atomicity claim behind the
// per-path count assertions above, stated once: whichever write path refuses,
// the refusal happens before the statement rather than as a compensating
// delete, so a caller that retries a failed save is never racing a partial one.
func TestSecretRefusalCarriesNoStoreWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, _, _, err := s.Upsert(ctx, testProject, "fact", secretFixture, "mcp", 0.7, nil); err == nil {
		t.Fatal("Upsert accepted a bare credential")
	}
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", "an ordinary fact worth keeping", "mcp", 0.7, nil); err != nil {
		t.Fatalf("the next save after a refusal failed too: %v", err)
	}
	if n := projectMemoryCount(t, s); n != 1 {
		t.Errorf("store holds %d memories, want exactly the one ordinary save", n)
	}
}

func strPtr(s string) *string { return &s }

func float32Ptr(f float32) *float32 { return &f }
