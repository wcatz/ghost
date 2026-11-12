package memory

import (
	"context"
	"testing"
)

func evidenceKinds(t *testing.T, ev []Evidence) []string {
	t.Helper()
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Kind+":"+e.Agent+":"+e.SessionID)
	}
	return out
}

func TestSaveAppendsOneObservedEvidenceRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"the API gateway listens on port 8443", "mcp", 0.7, nil,
		Provenance{Agent: "opencode", SessionID: "ses_123", SourceRef: "helmfile.yaml:L20", Confidence: f64Ptr(0.9)})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("evidence has %d records, want 1: %v", len(ev), evidenceKinds(t, ev))
	}
	got := ev[0]
	if got.Kind != evidenceObserved {
		t.Errorf("kind = %q, want %q", got.Kind, evidenceObserved)
	}
	if got.MemoryID != id {
		t.Errorf("memory_id = %q, want %q", got.MemoryID, id)
	}
	if got.Agent != "opencode" || got.SessionID != "ses_123" || got.SourceRef != "helmfile.yaml:L20" {
		t.Errorf("record = %+v, want the agent, session and reference the caller reported", got)
	}
	if got.Confidence == nil || *got.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", derefF(got.Confidence))
	}
	// observed_at is stamped by the writer, not by the caller: it is when Ghost
	// recorded the observation, and a caller cannot backdate it.
	if got.ObservedAt == nil || *got.ObservedAt == "" {
		t.Error("the record carries no observed_at; an undated observation cannot be ordered against another")
	}
	if got.VerifiedAt != nil {
		t.Errorf("verified_at = %v, want NULL — nothing verified this observation", *got.VerifiedAt)
	}
	if got.ID == "" {
		t.Error("the record has no id")
	}
}

func TestFoldAppendsTheSecondObservationToTheSurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two spellings of one sentence, which is the shape a near-duplicate save
	// takes: same words, presentation apart.
	const stored = "The staging database is reached over the bastion at port 2222 not 22"
	const restated = "the staging database is reached over the bastion at port 2222 not 22"

	survivor, _, _, err := s.UpsertWithProvenance(ctx, testProject, "convention", stored, "mcp", 0.7, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/bastion.md"})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// The default fold: the incoming wording is stored as a linked copy of its
	// own, and the target is returned as the row this save strengthens.
	copyID, duplicateOf, _, err := s.UpsertWithProvenance(ctx, testProject, "convention", restated, "mcp", 0.7, nil,
		Provenance{Agent: "codex", SessionID: "ses_b", SourceRef: "docs/runbook.md"})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if duplicateOf != survivor {
		t.Fatalf("the second save folded into %q, want the survivor %q — the fixture does not test a fold", duplicateOf, survivor)
	}

	ev, err := s.MemoryProvenance(ctx, survivor)
	if err != nil {
		t.Fatalf("MemoryProvenance(survivor): %v", err)
	}
	if got := evidenceKinds(t, ev); !equalStrings(got, []string{"observed:claude-code:ses_a", "observed:codex:ses_b"}) {
		t.Fatalf("survivor evidence = %v, want both agents' observations in the order they were reported", got)
	}
	// The second record carries the SECOND reporter's reference, not the first's
	// and not the survivor's own: the point is that each observation says who
	// reported it.
	if ev[1].SourceRef != "docs/runbook.md" {
		t.Errorf("the fold's record source_ref = %q, want the folding save's own reference", ev[1].SourceRef)
	}

	// And the linked copy is evidence of its own, not a shadow of the survivor's.
	copyEv, err := s.MemoryProvenance(ctx, copyID)
	if err != nil {
		t.Fatalf("MemoryProvenance(copy): %v", err)
	}
	if got := evidenceKinds(t, copyEv); !equalStrings(got, []string{"observed:codex:ses_b"}) {
		t.Errorf("the stored copy's evidence = %v, want its own single observation", got)
	}
}

func TestDeleteCascadesEvidenceAndKeepsTheHistoryTombstone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a fact that will be deleted", "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if ev, err := s.MemoryProvenance(ctx, id); err != nil || len(ev) != 1 {
		t.Fatalf("the fixture needs one evidence record on the memory: %d %v", len(ev), err)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance after delete: %v", err)
	}
	if len(ev) != 0 {
		t.Errorf("evidence survived the delete: %v — evidence without its memory means nothing", evidenceKinds(t, ev))
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory after delete: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the delete left no history; the change log's tombstone is the contrast this test is about")
	}
	if last := entries[len(entries)-1]; last.Phase != phaseDelete {
		t.Errorf("last history row is %q, want the delete tombstone", last.Phase)
	}
}

func TestPurgeRemovesTheEvidenceOfALiveMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", "a gotcha that will be purged", "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/secret-runbook.md"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	other, _, _, err := s.Upsert(ctx, testProject, "fact", "an unrelated live memory", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert(other): %v", err)
	}

	if _, err := s.PurgeMemoryHistory(ctx, id); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if len(ev) != 0 {
		t.Errorf("evidence survived the purge: %v", evidenceKinds(t, ev))
	}
	if live, err := s.GetByIDs(ctx, []string{id}); err != nil || len(live) != 1 {
		t.Fatalf("the purge deleted a live memory: %v %v", live, err)
	}
	// Another memory's evidence is not this memory's business.
	if ev, err := s.MemoryProvenance(ctx, other); err != nil || len(ev) != 1 {
		t.Errorf("an unrelated memory's evidence = %d records (%v), want its own 1", len(ev), err)
	}
}

func TestImportAppendsAnImportedEvidenceRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const id = "EEEEEEEEEEEEEEEEEEEEEEEEEEEE"
	if _, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID:        id,
		ProjectID: testProject,
		Category:  "fact",
		Content:   "a fact that arrived in an artifact",
		Source:    "manual",
		Agent:     "claude-code",
		SessionID: "ses_from_artifact",
		SourceRef: "artifact:line-7",
	}, ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if got := evidenceKinds(t, ev); !equalStrings(got, []string{"imported:claude-code:ses_from_artifact"}) {
		t.Fatalf("evidence = %v, want one imported record attributed to the artifact's agent", got)
	}
	if ev[0].SourceRef != "artifact:line-7" {
		t.Errorf("source_ref = %q, want the artifact's own reference", ev[0].SourceRef)
	}
}

func TestMemoryProvenanceReadsOldestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Three saves of ONE fact, each folded onto the first, so all three
	// observations land on the row that survived. The wording varies only in case
	// and spacing, which is the one shape FoldOnly folds.
	const wording = "a fact three agents report the same way"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", wording, "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	for i, restatement := range []string{
		"A Fact Three Agents Report The Same Way",
		"A  FACT  three agents report the same way",
	} {
		agent := []string{"codex", "goose"}[i]
		got, duplicateOf, _, err := s.UpsertWithOptions(ctx, testProject, "fact", restatement, "mcp", 0.5, nil,
			UpsertOptions{FoldOnly: true, Provenance: Provenance{Agent: agent, SessionID: "ses_" + agent}})
		if err != nil {
			t.Fatalf("save as %s: %v", agent, err)
		}
		if got != id || duplicateOf != id {
			t.Fatalf("the %s save did not fold onto the survivor: (%q, %q)", agent, got, duplicateOf)
		}
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if got := evidenceKinds(t, ev); !equalStrings(got, []string{
		"observed:claude-code:ses_a", "observed:codex:ses_codex", "observed:goose:ses_goose",
	}) {
		t.Errorf("evidence = %v, want the three observations in the order they were reported", got)
	}
	if ev, err := s.MemoryProvenance(ctx, "no-such-memory"); err != nil || len(ev) != 0 {
		t.Errorf("an unknown id returned %d records (%v), want an empty slice and no error", len(ev), err)
	}
}
