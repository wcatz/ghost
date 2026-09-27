package memory

import (
	"context"
	"testing"
)

// evidenceKinds renders a memory's evidence as "kind:agent:session" so an
// assertion reads as the sequence of observations rather than a field-by-field
// comparison, and so a NULL agent shows as an empty segment rather than
// disappearing.
func evidenceKinds(t *testing.T, ev []Evidence) []string {
	t.Helper()
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Kind+":"+e.Agent+":"+e.SessionID)
	}
	return out
}

// TestSaveAppendsOneObservedEvidenceRecord: the ordinary save. A memory written
// with a provenance gets exactly one evidence record carrying what the host
// actually reported, and nothing else — one save is one observation, not a row
// per field.
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

// TestSaveWithNoProvenanceLeavesEveryEvidenceFieldNull: the compatibility
// contract for the write-time provenance columns, restated on the evidence
// table. Most saves carry no agent, no session and no reference, and an evidence
// record that stored "" for them would claim a value that happens to be empty —
// which is a provenance claim nobody made. The record is still appended: the save
// DID happen, and "an observation of unknown authorship" is a real fact.
func TestSaveWithNoProvenanceLeavesEveryEvidenceFieldNull(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a plain memory with no provenance", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("evidence has %d records, want 1: %v", len(ev), evidenceKinds(t, ev))
	}
	if ev[0].Kind != evidenceObserved {
		t.Errorf("kind = %q, want %q", ev[0].Kind, evidenceObserved)
	}

	// The NULL/empty distinction is invisible in the Go struct, so it is read
	// from the row itself. An assertion on ev[0].SessionID == "" would pass
	// against a fabricated empty string, which is the failure this is about.
	var nulls int
	if err := s.db.QueryRow(`
		SELECT (agent IS NULL) + (session_id IS NULL) + (source_ref IS NULL) + (confidence IS NULL)
		FROM memory_provenance WHERE memory_id = ?`, id).Scan(&nulls); err != nil {
		t.Fatalf("read the evidence row: %v", err)
	}
	if nulls != 4 {
		t.Errorf("%d of agent/session_id/source_ref/confidence are NULL, want 4 — an empty string would claim a value that happens to be empty", nulls)
	}
}

// TestFoldAppendsTheSecondObservationToTheSurvivor: the point of the issue. A
// near-duplicate save used to be a discard — the incoming text was kept only as a
// linked copy, and the agent and session that reported it were dropped on the
// floor. Two agents reporting the same fact is exactly the signal provenance
// exists to keep, so the fold appends the second observation to the memory that
// SURVIVED rather than only to the copy.
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

// TestFoldOnlyFoldAlsoLeavesEvidenceOnTheSurvivor: the promotion path throws the
// incoming wording away, so the evidence table is the only place the second
// observation can survive at all. Without this the strongest case for the feature
// — a fact _global already knows, reported again by a different agent — would
// record nothing.
func TestFoldOnlyFoldAlsoLeavesEvidenceOnTheSurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Normalization-equivalent, which is the only pair FoldOnly folds: same words,
	// different case and spacing.
	const stored = "the gouroboros PR workflow pushes from the laptop clone"
	const restated = "The  Gouroboros PR Workflow Pushes From The Laptop Clone"

	survivor, _, _, err := s.UpsertWithProvenance(ctx, testProject, "convention", stored, "reflection", 0.6, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	got, duplicateOf, _, err := s.UpsertWithOptions(ctx, testProject, "convention", restated, "reflection", 0.6, nil,
		UpsertOptions{FoldOnly: true, Provenance: Provenance{Agent: "goose", SessionID: "ses_c"}})
	if err != nil {
		t.Fatalf("FoldOnly save: %v", err)
	}
	if got != survivor || duplicateOf != survivor {
		t.Fatalf("FoldOnly returned (%q, %q), want the survivor %q twice — the fixture does not test a fold", got, duplicateOf, survivor)
	}

	ev, err := s.MemoryProvenance(ctx, survivor)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if kinds := evidenceKinds(t, ev); !equalStrings(kinds, []string{"observed:claude-code:ses_a", "observed:goose:ses_c"}) {
		t.Errorf("evidence = %v, want the discarded paraphrase's reporter recorded on the survivor", kinds)
	}
}

// TestDeleteCascadesEvidenceAndKeepsTheHistoryTombstone: the two tables answer
// different questions, so they survive a delete differently. Evidence about a
// memory that no longer exists is meaningless, so the foreign key cascades and
// the rows go. The change log is the audit of a deletion, so memory_history keeps
// its tombstone. A cascade applied to both, or to neither, would lose one of them.
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

// TestPurgeRemovesTheEvidenceOfALiveMemory: a history-only purge leaves the
// memory in place, so the cascade cannot fire — and the evidence has to go with
// the rest anyway, because a purge asked for at delete time must not be defeated
// by a memory that outlived it. The surviving row's own claim to be supported is
// the thing being erased.
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

// TestPurgeAtDeleteTimeRemovesTheEvidenceToo: the redaction path, where the
// memory row goes as well. The cascade would take the rows with it, and the
// explicit delete takes them when the handle has foreign keys off — a purge whose
// completeness depended on a connection pragma would be a purge that can be
// reported as done and have left the rows.
func TestPurgeAtDeleteTimeRemovesTheEvidenceToo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Ordinary text on purpose: #656 refuses a credential shape on the way in, and
	// this test is about what a purge removes afterwards, not about what a save
	// accepts.
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha",
		"the staging deploy needs the bastion tunnel before it starts", "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a", SourceRef: "deploy/runbook.md"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = ?`, id).Scan(&left); err != nil {
		t.Fatalf("count evidence rows: %v", err)
	}
	if left != 0 {
		t.Errorf("%d evidence row(s) survived a redaction", left)
	}
	var leaked int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE source_ref = ?`, "deploy/runbook.md").Scan(&leaked); err != nil {
		t.Fatalf("count leaked references: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the purged memory's reference survives in %d evidence row(s)", leaked)
	}
}

// TestImportAppendsAnImportedEvidenceRecord: an artifact is a file that arrived
// from somewhere, and the evidence table has to be able to say that this fact
// reached the store through one — a claim no observed row makes, and one a
// restored corpus would otherwise lose entirely.
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

// TestEvidenceCountsReportWhatSupportsAMemory: the compact rendering the
// assembler's trace reports. It counts RECORDS, so a second agent corroborating a
// fact raises it, and a record nothing has verified does not claim to be verified.
func TestEvidenceCountsReportWhatSupportsAMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a fact two agents report", "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a fact two agents report, per the runbook", "mcp", 0.5, nil,
		Provenance{Agent: "codex", SessionID: "ses_b"}); err != nil {
		t.Fatalf("second save: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Observations != 2 {
		t.Errorf("observations = %d, want 2", counts.Observations)
	}
	if counts.Verified != 0 {
		t.Errorf("verified = %d, want 0 — nothing verified either observation", counts.Verified)
	}
	if want := "supported by 2 observations"; counts.Label() != want {
		t.Errorf("label = %q, want %q", counts.Label(), want)
	}

	// A record that carries a verification stamp counts as verified, whatever its
	// kind: the seed and any future verified writer both land here.
	if _, err := s.db.Exec(
		`UPDATE memory_provenance SET verified_at = datetime('now') WHERE memory_id = ? AND agent = 'codex'`, id,
	); err != nil {
		t.Fatalf("stamp a verification: %v", err)
	}
	counts, err = s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("verified = %d, want 1", counts.Verified)
	}
	if want := "supported by 2 observations, 1 verified"; counts.Label() != want {
		t.Errorf("label = %q, want %q", counts.Label(), want)
	}

	// A memory no evidence names is a real answer, not a zero to render around.
	if counts, err := s.MemoryEvidenceCounts(ctx, "no-such-memory"); err != nil || counts.Observations != 0 {
		t.Errorf("counts for an unknown id = %+v (%v), want zeroes", counts, err)
	}
	if got := (EvidenceCounts{}).Label(); got != "no recorded evidence" {
		t.Errorf("empty label = %q, want %q", got, "no recorded evidence")
	}
}

// TestMemoryProvenanceReadsOldestFirst: the evidence table is a log of what
// supported a memory over time, and "read forwards" is the only order in which
// the sequence means anything. observed_at is second-precision, so the order is
// rowid.
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
