package memory

import (
	"context"
	"testing"
)

func retainedMemory(category, content string, importance float32, tags []string) Memory {
	return Memory{Category: category, Content: content, Importance: importance, Tags: tags}
}

func TestReplaceNonManualRetainedRowKeepsCreatedAtAndSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the ingestion worker retries a dead-lettered event three times before parking it"
	confidence := 0.8
	id, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.6,
		Tags: []string{"worker"}, Agent: "claude-code", SessionID: "sess-1", SourceRef: "PR-623",
		Confidence: &confidence,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, id, 100)
	staleCreatedAt := memoryCreatedAt(t, s, id)

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", content, 0.6, []string{"worker"}),
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	got := rows[0]
	if got.CreatedAt != staleCreatedAt {
		t.Errorf("created_at = %q, want the stored %q — retention re-stamped the row as new, "+
			"restarting the decay clock of a memory the consolidator omitted as stale",
			got.CreatedAt, staleCreatedAt)
	}
	if got.Source != "mcp" {
		t.Errorf("source = %q, want mcp — retention overwrote the provenance of a memory it did not rewrite", got.Source)
	}
	if got.Agent != "claude-code" || got.SessionID != "sess-1" || got.SourceRef != "PR-623" ||
		got.Confidence == nil || *got.Confidence != confidence {
		t.Errorf("provenance = (%q, %q, %q, %v), want (claude-code, sess-1, PR-623, 0.8)",
			got.Agent, got.SessionID, got.SourceRef, got.Confidence)
	}
}

func TestReplaceNonManualRetainedRowStillDecays(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const staleContent = "the ledger store keeps its write-ahead log on a dedicated volume"
	staleID, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: staleContent, Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create stale: %v", err)
	}
	setCreatedAtDaysAgo(t, s, staleID, 100)

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		retainedMemory("architecture", staleContent, 0.6, []string{}),
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: "the scheduler drains its work queue in batches of 64",
		Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("top memories = %d, want 2", len(top))
	}
	if top[0].ID == staleID {
		t.Errorf("a 100-day-old retained memory outranks a fresh one: %+v — retention "+
			"restarted its decay", top)
	}
}

func TestReplaceNonManualUnchangedReuseStillAppliesRewriteFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the deploy pipeline requires a manual approval gate"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "convention", Content: content, Source: "mcp", Importance: 0.4, Tags: []string{},
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setCreatedAtDaysAgo(t, s, id, 100)
	createdAt := memoryCreatedAt(t, s, id)

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "convention", Content: content, Importance: 0.9, Tags: []string{"deploy", "ci"},
		Scope: map[string]string{"environment": "production"},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	got := rows[0]
	if got.Importance != 0.9 {
		t.Errorf("importance = %v, want 0.9", got.Importance)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "deploy" || got.Tags[1] != "ci" {
		t.Errorf("tags = %v, want [deploy ci]", got.Tags)
	}
	if got.Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production", got.Scope)
	}
	if got.CreatedAt != createdAt {
		t.Errorf("created_at = %q, want the stored %q", got.CreatedAt, createdAt)
	}
}

func TestAReusedRowAbsorbingAConsumedSourceRecordsAProvenanceVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the queue backend is redis lists"
	reused, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the reused row: %v", err)
	}
	other, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a stale note the merge folds in", "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the other source: %v", err)
	}
	setUpdatedAt(t, s, reused, "2020-01-01 00:00:00")
	before, err := s.MemoryHistory(ctx, reused, 10)
	if err != nil {
		t.Fatalf("MemoryHistory before the merge: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.5,
		ReplacesIDs: []string{reused, other},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if !rowExists(t, s, reused) || rowExists(t, s, other) {
		t.Fatalf("the merge did not reuse the row and take the other source (reused live=%v, other live=%v)",
			rowExists(t, s, reused), rowExists(t, s, other))
	}

	updated := readUpdatedAt(t, s, reused)
	if updated == "2020-01-01 00:00:00" {
		t.Error("the reused row's updated_at is still the backdated value: the provenance stamp filed a version without moving the stamp column")
	}

	after, err := s.MemoryHistory(ctx, reused, 10)
	if err != nil {
		t.Fatalf("MemoryHistory after the merge: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("history versions = %d, want %d: the provenance change was not recorded",
			len(after), len(before)+1)
	}
	newest := after[len(after)-1]
	if newest.Source != "reflection" {
		t.Errorf("the newest version's source = %q, want reflection: an as_of read after the merge still reports the agent save's provenance", newest.Source)
	}
	if newest.Phase != phaseReflect {
		t.Errorf("the newest version's phase = %q, want %q", newest.Phase, phaseReflect)
	}
	if newest.RecordedAt != updated {
		t.Errorf("the stamp version's recorded_at = %q, the row's updated_at = %q: a version filed by a stamp-moving writer must carry the time that writer moved the column to",
			newest.RecordedAt, updated)
	}
}

func TestAStampMovingVersionCarriesTheUpdatedAtItsWriterLeft(t *testing.T) {
	const pinned = "2021-02-03 04:05:06"
	pin := func(t *testing.T, s *Store) {
		t.Helper()
		for _, ev := range []string{"INSERT", "UPDATE"} {
			if _, err := s.db.Exec(`CREATE TRIGGER pin_stamp_` + ev + ` AFTER ` + ev + ` ON memories
				WHEN NEW.updated_at <> '` + pinned + `'
				BEGIN UPDATE memories SET updated_at = '` + pinned + `' WHERE id = NEW.id; END`); err != nil {
				t.Fatalf("create trigger: %v", err)
			}
		}
	}
	unpin := func(t *testing.T, s *Store) {
		t.Helper()
		for _, ev := range []string{"INSERT", "UPDATE"} {
			if _, err := s.db.Exec(`DROP TRIGGER pin_stamp_` + ev); err != nil {
				t.Fatalf("drop trigger: %v", err)
			}
		}
	}
	check := func(t *testing.T, s *Store, id, phase string) {
		t.Helper()
		vs, err := s.MemoryHistory(context.Background(), id, 10)
		if err != nil || len(vs) == 0 {
			t.Fatalf("MemoryHistory: %v (%d versions)", err, len(vs))
		}
		newest := vs[len(vs)-1]
		if newest.Phase != phase {
			t.Fatalf("newest phase = %q, want %q", newest.Phase, phase)
		}
		if got := readUpdatedAt(t, s, id); got != pinned {
			t.Fatalf("row updated_at = %q, want the pinned %q", got, pinned)
		}
		if newest.RecordedAt != pinned {
			t.Errorf("%s version recorded_at = %q, want the row's updated_at %q", phase, newest.RecordedAt, pinned)
		}
	}

	t.Run("save", func(t *testing.T) {
		s := testStore(t)
		pin(t, s)
		id, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "a saved fact", "mcp", 0.5, nil, UpsertOptions{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		check(t, s, id, phaseSave)
	})
	t.Run("update", func(t *testing.T) {
		s := testStore(t)
		id, _, _, err := s.UpsertWithOptions(context.Background(), testProject, "fact", "before the edit", "mcp", 0.5, nil, UpsertOptions{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		pin(t, s)
		next := "after the edit"
		if err := s.UpdateMemory(context.Background(), testProject, id, &next, nil, nil, nil); err != nil {
			t.Fatalf("update: %v", err)
		}
		check(t, s, id, phaseUpdate)
	})
	t.Run("reflect", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const content = "the queue backend is redis lists"
		reused, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.5, nil, UpsertOptions{})
		if err != nil {
			t.Fatalf("save reused: %v", err)
		}
		other, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a stale note the merge folds in", "mcp", 0.5, nil, UpsertOptions{})
		if err != nil {
			t.Fatalf("save other: %v", err)
		}
		pin(t, s)
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: content, Importance: 0.5, ReplacesIDs: []string{reused, other},
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		check(t, s, reused, phaseReflect)
		unpin(t, s)
	})
}

// A reuse is relabelled only when it CONSUMES a named source (#1283): an id no
// row holds, a manual row, and a row the pass keeps are all named by an emission
// but are never folded into the reused row, so stamping reflection provenance on
// it would announce a merge that did not happen.
func TestAReuseIsNotRelabelledByANamedSourceItDoesNotConsume(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the staging cluster answers on port 8443"
	save, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the row: %v", err)
	}
	manual, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a hand-written note", "manual", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the manual row: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.5,
		ReplacesIDs: []string{save, "NOSUCHID", manual},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if got := getOne(t, s, save); got.Source != "mcp" {
		t.Errorf("source = %q, want mcp: neither a missing id nor a manual row is consumed by the merge, so the reused row was not made a successor", got.Source)
	}
	if !rowExists(t, s, manual) {
		t.Error("the manual row was deleted")
	}
	hist, err := s.MemoryHistory(ctx, save, 10)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Errorf("history has %d version(s), want the save's own 1: a reuse that consumed nothing recorded a version", len(hist))
	}
}
