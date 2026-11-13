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

		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
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

		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
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

		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
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

		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed); err != nil {
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

// corpusRows reads the id, content, updated_at and the metadata the reuse
// branch writes — category, importance, tags — of each row the way ghost
// reflect does before handing the corpus to a harness.
func corpusRows(t *testing.T, s *Store, ids ...string) []CorpusRow {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	out := make([]CorpusRow, 0, len(rows))
	for _, m := range rows {
		out = append(out, CorpusRow{
			ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt,
			Category: m.Category, Importance: m.Importance, Tags: m.Tags,
		})
	}
	return out
}

// The global half of the apply writes into _global through the promotion loop,
// which never consults the project's reuse map, so #1051's guard has to be
// applied to the cross-project candidates as well. Without it a concurrently
// deleted project row came back in _global, and a global emission merging a
// concurrently edited project row was written beside the edit.
func TestApplyReflectionGuardsTheGlobalHalf(t *testing.T) {
	ctx := context.Background()

	t.Run("a promoted candidate claiming a concurrently deleted row is dropped", func(t *testing.T) {
		s := testStore(t)
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "gotcha", Content: "unrelated note beta", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		setAgesDaysAgo(t, s, bID, 2)

		fed := corpusRows(t, s, bID)
		// The delete lands while the harness is answering, and the candidate
		// the harness returned still claims B's text.
		if err := s.Delete(ctx, bID); err != nil {
			t.Fatalf("Delete B: %v", err)
		}

		_, promoted, _, _, _, _, _, err := s.ApplyReflection(ctx, testProject,
			nil, []Memory{{Category: "gotcha", Content: "unrelated note beta", Importance: 0.5, Tags: []string{}}},
			"", true, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}
		if promoted != 0 {
			t.Errorf("promoted = %d, want 0: the deleted row was resurrected in _global", promoted)
		}
		globals, err := s.GetAll(ctx, "_global", -1)
		if err != nil {
			t.Fatalf("GetAll _global: %v", err)
		}
		if len(globals) != 0 {
			t.Errorf("_global holds %d rows: %v — the deleted project row was resurrected there",
				len(globals), contents(globals))
		}
		project, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll project: %v", err)
		}
		if len(project) != 0 {
			t.Errorf("project holds %d rows: %v — the dropped candidate was recovered into the project",
				len(project), contents(project))
		}
	})

	t.Run("a global emission merging a concurrently edited row is dropped", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the relay host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 2)

		fed := corpusRows(t, s, aID)

		edited := "the relay host is bravo"
		if err := s.UpdateMemory(ctx, testProject, aID, &edited, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		// A cross-project merge naming the row that was just edited.
		_, promoted, _, _, _, _, _, err := s.ApplyReflection(ctx, testProject,
			nil, []Memory{{
				Category: "fact", Content: "the relay host, one fact across projects", Importance: 0.7,
				Tags: []string{}, ReplacesIDs: []string{aID},
			}},
			"", true, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}
		if promoted != 0 {
			t.Errorf("promoted = %d, want 0: the merge was written beside the concurrent edit", promoted)
		}
		globals, err := s.GetAll(ctx, "_global", -1)
		if err != nil {
			t.Fatalf("GetAll _global: %v", err)
		}
		for _, m := range globals {
			if m.Content == "the relay host, one fact across projects" {
				t.Errorf("_global holds the merge row written beside the concurrent edit")
			}
		}
		got := getOne(t, s, aID)
		if got.Content != edited {
			t.Errorf("A = %q, want the concurrent edit %q kept", got.Content, edited)
		}
	})

	t.Run("a refused cross-project merge keeps its unchanged sources from the project replace", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the relay host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 2)
		bID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is beta", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create B: %v", err)
		}
		setAgesDaysAgo(t, s, bID, 2)
		thirdID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "unrelated stable fact", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create C: %v", err)
		}
		setAgesDaysAgo(t, s, thirdID, 2)

		fed := corpusRows(t, s, aID, bID, thirdID)

		edited := "the relay host is bravo"
		if err := s.UpdateMemory(ctx, testProject, aID, &edited, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory A: %v", err)
		}

		// The project half is non-empty (a verbatim keep of the unrelated row),
		// so the whole-set project replace runs. The only claim on A or B is the
		// cross-project merge, which the guard refuses because A moved mid-round;
		// the refused merge must keep ALL of its sources, so the replace must not
		// delete the unchanged B as unaccounted. That is the project replace doing
		// exactly what the guard exists to prevent — the merge was not allowed to
		// consume A or B, so neither may be taken.
		_, promoted, kept, projectDropped, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject,
			[]Memory{{Category: "fact", Content: "unrelated stable fact", Importance: 0.6, Tags: []string{}}},
			[]Memory{{
				Category: "fact", Content: "alpha and beta, one fact across projects", Importance: 0.7,
				Tags: []string{}, ReplacesIDs: []string{aID, bID},
			}},
			"", false, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}
		if globalDropped.Changed != 1 {
			t.Errorf("globalDropped.Changed = %d, want 1: the merge claiming the edited A was not refused", globalDropped.Changed)
		}
		if projectDropped.Changed != 0 {
			t.Errorf("projectDropped.Changed = %d, want 0: the refusal belongs to the global half", projectDropped.Changed)
		}
		if promoted != 0 || len(kept) != 0 {
			t.Errorf("promoted/kept = %d/%d, want 0/0", promoted, len(kept))
		}
		project, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll project: %v", err)
		}
		byID := map[string]Memory{}
		for _, m := range project {
			byID[m.ID] = m
		}
		if _, ok := byID[bID]; !ok {
			t.Errorf("B (%s) is gone from the project after the refused merge: an unchanged source of a dropped cross-project emission must survive the project replace, got %v", bID, contents(project))
		}
		if got := byID[aID]; got.Content != edited {
			t.Errorf("A = %q, want the concurrent edit %q kept", got.Content, edited)
		}
		if _, ok := byID[thirdID]; !ok {
			t.Errorf("the unrelated row C was lost: %v", contents(project))
		}
	})

	// With promotion off the surviving cross-project candidates are folded into
	// the replace's input, and the replace guards that folded list AGAIN. The
	// second pass refuses a candidate the global half's own pass accepted, and a
	// refusal about a folded candidate belongs to the half that PRODUCED it: the
	// caller's cross-project component subtracts only the global half's drops, so
	// leaving the refusal in the project count makes the Applied line claim a
	// candidate was kept project-scoped beside a total that never counted it.
	t.Run("a folded candidate the replace refuses second counts as the global half's drop", func(t *testing.T) {
		s := testStore(t)
		xID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the relay host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create X: %v", err)
		}
		setAgesDaysAgo(t, s, xID, 2)
		yID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the deploy host is beta", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create Y: %v", err)
		}
		setAgesDaysAgo(t, s, yID, 2)

		fed := corpusRows(t, s, xID, yID)

		// Y moved mid-round, so the project half's merge — which claims both
		// rows — is refused, and refusing it spares BOTH of its sources.
		edited := "the deploy host is bravo"
		if err := s.UpdateMemory(ctx, testProject, yID, &edited, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory Y: %v", err)
		}

		_, promoted, kept, projectDropped, globalDropped, _, appliedByCategory, err := s.ApplyReflection(ctx, testProject,
			[]Memory{{
				Category: "fact", Content: "alpha and beta, one fact", Importance: 0.7,
				Tags: []string{}, ReplacesIDs: []string{xID, yID},
			}},
			[]Memory{{Category: "fact", Content: "the relay host is alpha", Importance: 0.6, Tags: []string{}}},
			"", false, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}

		// The verbatim keep of X passed the global half's guard (X was untouched
		// when the fold happened) and was refused only inside the replace, after
		// the refused merge marked X concurrent. It is still a cross-project
		// candidate's refusal, so the global half owns it.
		if globalDropped.Changed != 1 {
			t.Errorf("globalDropped.Changed = %d, want 1: the folded candidate the replace refused belongs to the half that produced it", globalDropped.Changed)
		}
		if projectDropped.Changed != 1 {
			t.Errorf("projectDropped.Changed = %d, want 1: only the project half's own merge was refused", projectDropped.Changed)
		}
		if projectDropped.Total()+globalDropped.Total() != 2 {
			t.Errorf("dropped total = %d, want 2: re-attribution moves a refusal between halves, it does not create one", projectDropped.Total()+globalDropped.Total())
		}
		if len(appliedByCategory) != 0 {
			t.Errorf("appliedByCategory = %v, want empty: every emission was refused", appliedByCategory)
		}
		if promoted != 0 || len(kept) != 0 {
			t.Errorf("promoted/kept = %d/%d, want 0/0", promoted, len(kept))
		}
		project, err := s.GetAll(ctx, testProject, -1)
		if err != nil {
			t.Fatalf("GetAll project: %v", err)
		}
		byID := map[string]Memory{}
		for _, m := range project {
			byID[m.ID] = m
		}
		if got := byID[xID]; got.Content != "the relay host is alpha" {
			t.Errorf("X = %q, want the untouched row kept: a refused merge spares its sources from the delete set", got.Content)
		}
		if got := byID[yID]; got.Content != edited {
			t.Errorf("Y = %q, want the concurrent edit %q kept", got.Content, edited)
		}
	})

	t.Run("a promotion with nothing concurrent still reaches _global", func(t *testing.T) {
		s := testStore(t)
		aID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the relay host is alpha", Source: "mcp", Importance: 0.6, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create A: %v", err)
		}
		setAgesDaysAgo(t, s, aID, 2)

		fed := corpusRows(t, s, aID)

		_, promoted, _, _, _, _, _, err := s.ApplyReflection(ctx, testProject,
			[]Memory{{Category: "fact", Content: "the relay host is alpha", Importance: 0.6, Tags: []string{}}},
			[]Memory{{Category: "fact", Content: "always run the linter before pushing", Importance: 0.6, Tags: []string{}}},
			"", true, fed)
		if err != nil {
			t.Fatalf("ApplyReflection: %v", err)
		}
		if promoted != 1 {
			t.Errorf("promoted = %d, want 1", promoted)
		}
		globals, err := s.GetAll(ctx, "_global", -1)
		if err != nil {
			t.Fatalf("GetAll _global: %v", err)
		}
		if len(globals) != 1 || globals[0].Content != "always run the linter before pushing" {
			t.Errorf("_global = %v, want the cross-project candidate", contents(globals))
		}
	})
}
