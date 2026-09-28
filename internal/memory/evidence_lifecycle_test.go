package memory

import (
	"context"
	"strings"
	"testing"
)

// evidenceRowsOf reads a memory's evidence as "kind:agent:carried-from", so an
// assertion reads as a sequence of observations rather than a field-by-field
// comparison.
func evidenceRowsOf(t *testing.T, ev []Evidence) []string {
	t.Helper()
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Kind+":"+e.Agent+":"+e.CarriedFrom)
	}
	return out
}

// TestDecisionCompanionMemoryHasAnObservedEvidenceRecord: the companion memory
// a decision writes is an ordinary memory — it is returned by search, quoted into
// the next reflect prompt, and folded like any other — so it is an observation
// like any other. A history row for it and none for its evidence is the
// inconsistency this pins: the corpus would report "no recorded evidence" about a
// memory Ghost itself wrote, in the same transaction that recorded its origin.
func TestDecisionCompanionMemoryHasAnObservedEvidenceRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, companion, _, err := s.RecordDecision(ctx, testProject,
		"pin the release tag", "tags are cut from the release branch", "the tag is immutable once pushed", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if companion == "" {
		t.Fatal("RecordDecision returned no companion memory id")
	}

	ev, err := s.MemoryProvenance(ctx, companion)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("the companion memory has %d evidence record(s), want 1: %v", len(ev), evidenceRowsOf(t, ev))
	}
	if ev[0].Kind != evidenceObserved {
		t.Errorf("kind = %q, want %q", ev[0].Kind, evidenceObserved)
	}
	// ghost_memory_record_decision carries no provenance of its own, so every
	// identifying field is NULL. That is the honest record: the decision tool
	// reports no agent, and a value here would be one Ghost invented.
	if ev[0].Agent != "" || ev[0].SessionID != "" || ev[0].SourceRef != "" || ev[0].Confidence != nil {
		t.Errorf("record = %+v, want every identifying field empty — the decision tool reports no provenance", ev[0])
	}
	if ev[0].CarriedFrom != "" {
		t.Errorf("carried_from = %q, want empty — a record written here observed this row directly", ev[0].CarriedFrom)
	}
	if ev[0].ObservedAt == nil {
		t.Error("the record carries no observed_at")
	}
}

// TestConsolidationCarriesEvidenceToTheRowThatSurvives: a rewrite or a merge mints
// a new id, and the foreign key takes the source rows' evidence with them. Without
// a carry, every consolidated memory reads "no recorded evidence" forever — the
// support a consolidation consolidated is exactly the support that is lost, and it
// is lost silently, on the one write path that runs unattended over the whole
// corpus.
//
// A carried row is a copy of its source's row, with carried_from naming the memory
// it came from. The copy is verbatim — same kind, same agent, same observed_at —
// because the observation really was made; what it is not is an observation of
// THIS wording, and carried_from is what says so. It is also the join back: that
// id's history survives the cascade, so the reader can ask what the source said.
func TestConsolidationCarriesEvidenceToTheRowThatSurvives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("a rewrite carries its source's evidence", func(t *testing.T) {
		source, _, _, err := s.UpsertWithProvenance(ctx, testProject, "convention",
			"the release tag is cut from the release branch", "mcp", 0.6, nil,
			Provenance{Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/release.md"})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		// A rewrite: new wording, so no reuse, and the source named as replaced.
		const rewritten = "the release tag is cut from the release branch, never from main"
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "convention", Content: rewritten, Importance: 0.6,
			ReplacesIDs: []string{source},
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		rewrittenID := onlyMemoryWithContent(t, s, rewritten)
		ev, err := s.MemoryProvenance(ctx, rewrittenID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		if got := evidenceRowsOf(t, ev); !equalStrings(got, []string{"observed:claude-code:" + source}) {
			t.Fatalf("the rewritten row's evidence = %v, want the source's observation carried onto it", got)
		}
		// The copy is verbatim, so a reader can still see who observed it and when.
		if ev[0].SessionID != "ses_a" || ev[0].SourceRef != "docs/release.md" {
			t.Errorf("carried record = %+v, want the source's own fields", ev[0])
		}
		if ev[0].ObservedAt == nil {
			t.Error("the carried record has no observed_at; the observation really was made, at a known time")
		}
	})

	t.Run("a merge carries every source's evidence", func(t *testing.T) {
		// Two unrelated facts, deliberately: a near-duplicate would FOLD into the
		// first, and the fold's second observation belongs to the survivor — the
		// merged row would then carry three records, which is right but is not what
		// this case is about.
		first, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
			"the staging cluster answers on port 8443", "mcp", 0.5, nil,
			Provenance{Agent: "claude-code", SessionID: "ses_a"})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		second, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
			"deploys go out through the bastion tunnel on port 2222", "mcp", 0.5, nil,
			Provenance{Agent: "codex", SessionID: "ses_b"})
		if err != nil {
			t.Fatalf("second Upsert: %v", err)
		}

		const merged = "the staging cluster answers health checks on port 8443"
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: merged, Importance: 0.5,
			ReplacesIDs: []string{first, second},
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		mergedID := onlyMemoryWithContent(t, s, merged)
		ev, err := s.MemoryProvenance(ctx, mergedID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		// BOTH agents, each naming the memory it was reported for. Two agents
		// saying the same thing is the strongest signal this table holds, and a
		// merge that keeps only one of them loses half of it.
		//
		// Compared as a SET, because the order within one carry is by source id and
		// not by the order the operation named them: two agents reporting one fact
		// have no first and second, and the carry is one statement for all of them.
		// What has to be deterministic — and is, by the carry's ORDER BY — is the
		// order between two runs, so two exports of an unchanged corpus stay
		// byte-identical.
		want := []string{
			"observed:claude-code:" + first,
			"observed:codex:" + second,
		}
		if got := evidenceRowsOf(t, ev); !sameSet(got, want) {
			t.Fatalf("the merged row's evidence = %v, want %v", got, want)
		}
		// And it is the same order every time, which is the part a set comparison
		// cannot see and a byte-reproducible export depends on.
		again, err := s.MemoryProvenance(ctx, mergedID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		if !equalStrings(evidenceRowsOf(t, ev), evidenceRowsOf(t, again)) {
			t.Errorf("two reads of the same rows disagree on order: %v then %v",
				evidenceRowsOf(t, ev), evidenceRowsOf(t, again))
		}
	})

	t.Run("a reuse leaves the row and its evidence alone", func(t *testing.T) {
		const kept = "the readiness probe is checked before the rollout"
		keptID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "convention", kept, "mcp", 0.6, nil,
			Provenance{Agent: "claude-code", SessionID: "ses_a"})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		before, err := s.MemoryProvenance(ctx, keptID)
		if err != nil || len(before) != 1 {
			t.Fatalf("the fixture needs one evidence record: %d %v", len(before), err)
		}

		// A byte-identical re-emission reuses the row in place. There is nothing to
		// carry: the row never changed identity, so its evidence never left it.
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "convention", Content: kept, Importance: 0.6,
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		after, err := s.MemoryProvenance(ctx, keptID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		if got := evidenceRowsOf(t, after); !equalStrings(got, evidenceRowsOf(t, before)) {
			t.Errorf("the reused row's evidence = %v, want it unchanged: %v", got, evidenceRowsOf(t, before))
		}
		if len(after) != 1 {
			t.Errorf("the reused row has %d evidence record(s), want the 1 it had", len(after))
		}
	})

	t.Run("a genuinely new memory gets no evidence", func(t *testing.T) {
		const fresh = "a consolidation surfaced this with no predecessor at all"
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: fresh, Importance: 0.5,
		}}, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		id := onlyMemoryWithContent(t, s, fresh)
		ev, err := s.MemoryProvenance(ctx, id)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		if len(ev) != 0 {
			t.Errorf("a brand-new memory has %d evidence record(s) (%v), want none — nobody observed it", len(ev), evidenceRowsOf(t, ev))
		}
	})
}

// TestRestoreBringsTheEvidenceBackWithTheMemory: a restore that reinstates a
// deleted row under its ORIGINAL id has to bring that row's evidence with it. The
// cascade took the evidence when the consolidation deleted the row, and a restore
// that put back only the text would return a memory that reads as never observed —
// the one row in the corpus whose support the database still had and threw away.
//
// The snapshot carries the evidence, which is the preferred side of the choice: a
// note recording that the evidence is gone would only report a loss, and this
// database does not have to lose it.
func TestRestoreBringsTheEvidenceBackWithTheMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const original = "the bastion accepts ssh on port 2222 only"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "convention", original, "mcp", 0.7, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a", SourceRef: "docs/bastion.md", Confidence: f64Ptr(0.9)})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE memory_provenance SET verified_at = datetime('now') WHERE memory_id = ?`, id,
	); err != nil {
		t.Fatalf("stamp a verification: %v", err)
	}
	before, err := s.MemoryProvenance(ctx, id)
	if err != nil || len(before) != 1 {
		t.Fatalf("the fixture needs one evidence record: %d %v", len(before), err)
	}

	// A consolidation rewrites it, which deletes the row the restore has to bring
	// back — and with it, its evidence.
	const rewritten = "the bastion accepts ssh on port 2222 and nothing else"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "convention", Content: rewritten, Importance: 0.7,
		ReplacesIDs: []string{id},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if ev, err := s.MemoryProvenance(ctx, id); err != nil || len(ev) != 0 {
		t.Fatalf("the deleted memory kept %d evidence record(s) (%v); the fixture does not test a restore", len(ev), err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	restored, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(restored) != 1 {
		t.Fatalf("the restore did not bring the row back under its own id: %v %v", restored, err)
	}
	if restored[0].Content != original {
		t.Errorf("restored content = %q, want the snapshot's wording", restored[0].Content)
	}
	ev, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if len(ev) != 1 {
		t.Fatalf("the restored memory has %d evidence record(s), want the 1 it had: %v", len(ev), evidenceRowsOf(t, ev))
	}
	// Verbatim, and RESTORED rather than carried: a restore puts a row back where
	// it was, so its evidence is its own again and carried_from stays empty. A
	// restore that marked the record as inherited would misreport the row's own
	// support as a successor's.
	if ev[0].Agent != before[0].Agent || ev[0].SessionID != before[0].SessionID || ev[0].SourceRef != before[0].SourceRef {
		t.Errorf("restored record = %+v, want the snapshot's own fields %+v", ev[0], before[0])
	}
	if ev[0].CarriedFrom != "" {
		t.Errorf("carried_from = %q, want empty — the row came back to itself", ev[0].CarriedFrom)
	}
	if ev[0].VerifiedAt == nil {
		t.Error("the restored record lost its verification stamp; the snapshot carried it")
	}
	if ev[0].Confidence == nil || *ev[0].Confidence != 0.9 {
		t.Errorf("restored confidence = %v, want 0.9", derefF(ev[0].Confidence))
	}
}

// TestRestoreDoesNotTouchTheEvidenceOfARowItUpdatesInPlace: the other half of the
// restore, and the one a snapshot-based fix gets wrong.
//
// A row the replace never deleted is restored by UPDATING it, so its evidence was
// never in danger — and the snapshot holds an OLDER copy of it. A restore that
// "restored" that evidence too would replace a row's own support with what it
// held at snapshot time, silently dropping every observation recorded since: a
// corroboration that arrived after the snapshot is exactly the observation a
// reader most wants and a rollback most destroys.
func TestRestoreDoesNotTouchTheEvidenceOfARowItUpdatesInPlace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A reflection-sourced row, so the replace snapshots it and keeps it in place.
	const original = "a fact the snapshot will hold for the record"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", original, "reflection", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A reflect that reuses the row verbatim: the snapshot is taken, the row is
	// kept, and the snapshot now holds exactly the evidence the row has.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: original, Importance: 0.5,
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual (the snapshot): %v", err)
	}

	// An edit after the snapshot, so the restore has a state change to revert...
	const edited = "a fact edited after the snapshot was taken"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(edited), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	// ...and a second agent's corroboration after that, so the row's evidence is
	// strictly MORE than the snapshot holds. A fold, because a fold is the way a
	// second agent reports the same fact.
	if _, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"a fact edited after the snapshot was taken, per the runbook", "mcp", 0.5, nil,
		Provenance{Agent: "codex", SessionID: "ses_b"}); err != nil {
		t.Fatalf("Upsert (the corroboration): %v", err)
	}
	got, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if want := []string{"observed:claude-code:", "observed:codex:"}; !sameSetPrefixes(evidenceRowsOf(t, got), want) {
		t.Fatalf("the row has %v, want both agents before the restore", evidenceRowsOf(t, got))
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// The text is back...
	restored, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(restored) != 1 {
		t.Fatalf("the row is gone: %v %v", restored, err)
	}
	if restored[0].Content != original {
		t.Errorf("restored content = %q, want the snapshot's wording", restored[0].Content)
	}
	// ...and so is the support recorded AFTER the snapshot, which the restore has
	// no business touching: this row was never deleted.
	after, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	if want := []string{"observed:claude-code:", "observed:codex:"}; !sameSetPrefixes(evidenceRowsOf(t, after), want) {
		t.Errorf("after the restore the row has %v, want both observations: the restore replaced support "+
			"recorded after the snapshot with the snapshot's older copy", evidenceRowsOf(t, after))
	}
}

// sameSetPrefixes reports whether got holds one record per wanted prefix — used
// where the assertion is about WHICH agents reported and not about order.
func sameSetPrefixes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.HasPrefix(g, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sameSet reports whether two renderings hold the same values, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}

// onlyMemoryWithContent returns the id of the one live memory holding content, and
// fails the test when there is not exactly one — a consolidation fixture that
// cannot name its own output is not testing anything.
func onlyMemoryWithContent(t *testing.T, s *Store, content string) string {
	t.Helper()
	ctx := context.Background()
	rows, err := s.GetAll(ctx, testProject, 1000)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var ids []string
	for _, m := range rows {
		if m.Content == content {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("%d live memories hold %q, want exactly 1: %v", len(ids), content, ids)
	}
	return ids[0]
}
