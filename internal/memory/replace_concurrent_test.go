package memory

import (
	"context"
	"testing"
)

// Issue #1051. `ghost reflect` reads the corpus, hands it to a harness, and
// applies the answer minutes later, in a separate process from the MCP server
// that is still serving ghost_memory_update and ghost_memory_delete. An
// emission naming a row whose content or updated_at moved while the harness was
// answering is a claim on a row that is no longer the row the pass read: it
// must be dropped, and the row kept as it is, rather than overwriting the edit
// or resurrecting the delete under a new id.
//
// The pass carries the corpus it read — id, content and updated_at — so the
// write transaction can tell the two apart. This is the same guard FoldRows
// applies with FoldRow.UpdatedAt.
func TestReplaceNonManualKeepsConcurrentEditsAndDeletes(t *testing.T) {
	ctx := context.Background()

	t.Run("an edited row survives and its re-emission is dropped", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 2)

		fed := corpusRows(t, s, aID)
		emitted := []Memory{{Category: "fact", Content: "the deploy host is alpha", Importance: 0.6, Tags: []string{}}}

		// The edit lands while the harness is answering.
		moved := "the deploy host is bravo (moved)"
		if err := s.UpdateMemory(ctx, testProject, aID, &moved, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		if len(all) != 1 {
			t.Fatalf("corpus holds %d rows, want the edited row alone: %v", len(all), contents(all))
		}
		if all[0].ID != aID || all[0].Content != moved {
			t.Fatalf("row = {%s %q}, want the concurrent edit {%s %q} kept under its own id",
				all[0].ID, all[0].Content, aID, moved)
		}
	})

	t.Run("a merge claiming a concurrently edited source is dropped", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "beta", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 2)
		setAgesDaysAgo(t, s, bID, 2)

		fed := corpusRows(t, s, aID, bID)
		// The consolidator merged the two rows while the edit below was landing.
		emitted := []Memory{{
			Category: "fact", Content: "alpha and beta, one fact", Importance: 0.7,
			Tags: []string{}, ReplacesIDs: []string{aID, bID},
		}}

		edited := "alpha (edited)"
		if err := s.UpdateMemory(ctx, testProject, aID, &edited, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("corpus holds %d rows, want both sources: %v", len(all), contents(all))
		}
		gotA := getOne(t, s, aID)
		if gotA.Content != edited {
			t.Errorf("A = %q, want the concurrent edit %q kept", gotA.Content, edited)
		}
		for _, m := range all {
			if m.Content == "alpha and beta, one fact" {
				t.Errorf("the merge row was inserted beside the sources it claimed")
			}
		}
	})

	t.Run("a deleted row is not resurrected", func(t *testing.T) {
		s := testStore(t)
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "gotcha", Content: "unrelated note beta", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		setAgesDaysAgo(t, s, bID, 2)

		fed := corpusRows(t, s, bID)
		emitted := []Memory{{Category: "gotcha", Content: "unrelated note beta", Importance: 0.5, Tags: []string{}}}

		// The delete lands while the harness is answering.
		if err := s.Delete(ctx, bID); err != nil {
			t.Fatalf("Delete B: %v", err)
		}

		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		if len(all) != 0 {
			t.Fatalf("corpus holds %d rows after a drop: %v — the delete was resurrected", len(all), contents(all))
		}
	})

	t.Run("the reuse branch keeps a concurrent metadata edit", func(t *testing.T) {
		s := testStore(t)
		cID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is charlie", Source: "mcp", Importance: 0.6, Tags: []string{"deploy"},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		setAgesDaysAgo(t, s, cID, 2)

		fed := corpusRows(t, s, cID)
		// The emission restates the text the pass read, with the values it read.
		emitted := []Memory{{Category: "fact", Content: "the deploy host is charlie", Importance: 0.6, Tags: []string{"deploy"}}}

		// Same content, different importance and tags: updated_at moves, content
		// does not, so only the updated_at half of the guard can see this.
		reweighted := float32(0.95)
		if err := s.UpdateMemory(ctx, testProject, cID, nil, nil, &reweighted, []string{"moved"}); err != nil {
			t.Fatalf("UpdateMemory C: %v", err)
		}

		if _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		got := getOne(t, s, cID)
		if got.Importance != reweighted {
			t.Errorf("importance = %v, want the concurrent %v kept: the re-emission overwrote it", got.Importance, reweighted)
		}
		if len(got.Tags) != 1 || got.Tags[0] != "moved" {
			t.Errorf("tags = %v, want the concurrent [moved] kept: the re-emission overwrote them", got.Tags)
		}
	})
}

// corpusRows reads the id, content and updated_at of each row the way
// ghost reflect does before handing the corpus to a harness.
func corpusRows(t *testing.T, s *Store, ids ...string) []CorpusRow {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	out := make([]CorpusRow, 0, len(rows))
	for _, m := range rows {
		out = append(out, CorpusRow{ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt})
	}
	return out
}
