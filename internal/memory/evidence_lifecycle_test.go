package memory

import (
	"context"
	"fmt"
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
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "convention", Content: rewritten, Importance: 0.6,
			ReplacesIDs: []string{source},
		}}, "", nil); err != nil {
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
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: merged, Importance: 0.5,
			ReplacesIDs: []string{first, second},
		}}, "", nil); err != nil {
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
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "convention", Content: kept, Importance: 0.6,
		}}, "", nil); err != nil {
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
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: fresh, Importance: 0.5,
		}}, "", nil); err != nil {
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

// TestReuseMergeCarriesTheAbsorbedSourcesEvidenceAndClaims: a merge whose text is
// byte-identical to one of its sources takes ReplaceNonManual's REUSE branch. That
// row is updated in place and the other sources are deleted into it, so their
// evidence goes with the cascade and their claims with the row — unless the reuse
// branch carries them, as the fresh INSERT and FoldRows do. The reused row's own
// columns win; the absorbed sources only fill what it left unset, and the window
// is still inherited as a unit, never completed half from another row.
func TestReuseMergeCarriesTheAbsorbedSourcesEvidenceAndClaims(t *testing.T) {
	ctx := context.Background()
	confidence := 0.7

	// absorbed writes a source row stating every claim, and returns its id.
	absorbed := func(t *testing.T, s *Store, content string) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: content, Source: "mcp", Importance: 0.5,
			Agent: "codex", SessionID: "ses_b", SourceRef: "docs/queue.md", Confidence: &confidence,
			ValidFrom:  stampPtr("2026-01-15 00:00:00"),
			ValidUntil: stampPtr("2027-12-31 23:59:59"),
			VerifiedAt: stampPtr("2026-09-20 00:00:00"),
		})
		if err != nil {
			t.Fatalf("Create absorbed source: %v", err)
		}
		return id
	}
	reuseMerge := func(t *testing.T, s *Store, kept string, ids ...string) {
		t.Helper()
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: kept, Importance: 0.5, ReplacesIDs: ids,
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
	}
	readRow := func(t *testing.T, s *Store, id string) Memory {
		t.Helper()
		got, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(got) != 1 {
			t.Fatalf("GetByIDs(%s): %v (n=%d) — the reused row must survive the merge", id, err, len(got))
		}
		return got[0]
	}

	t.Run("an unclaimed reused row takes the absorbed source's evidence and claims", func(t *testing.T) {
		s := testStore(t)
		const kept = "the job queue backend is Redis lists"
		keptID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: kept, Source: "mcp", Importance: 0.5, Agent: "claude-code",
		})
		if err != nil {
			t.Fatalf("Create kept: %v", err)
		}
		other := absorbed(t, s, "the queue worker reads Redis lists with BLPOP")

		reuseMerge(t, s, kept, keptID, other)

		if got := onlyMemoryWithContent(t, s, kept); got != keptID {
			t.Fatalf("the merge minted %s; this test needs the reuse branch to keep %s", got, keptID)
		}
		ev, err := s.MemoryProvenance(ctx, keptID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		// The absorbed source stated a verification, so it holds a verified record
		// beside its observation, and both are carried.
		want := []string{"observed:claude-code:", "observed:codex:" + other, "verified:codex:" + other}
		if got := evidenceRowsOf(t, ev); !sameSet(got, want) {
			t.Errorf("the reused row's evidence = %v, want its own record plus the absorbed source's carried ones %v", got, want)
		}

		row := readRow(t, s, keptID)
		if row.Agent != "claude-code" {
			t.Errorf("agent = %q, want the reused row's own claude-code", row.Agent)
		}
		if row.SourceRef != "docs/queue.md" {
			t.Errorf("source_ref = %q, want the absorbed source's docs/queue.md", row.SourceRef)
		}
		if row.SessionID != "ses_b" {
			t.Errorf("session_id = %q, want the absorbed source's ses_b", row.SessionID)
		}
		if row.Confidence == nil || *row.Confidence != confidence {
			t.Errorf("confidence = %s, want %v", floatOrNil(row.Confidence), confidence)
		}
		for _, tc := range []struct {
			name string
			got  *string
			want string
		}{
			{"valid_from", row.ValidFrom, "2026-01-15 00:00:00"},
			{"valid_until", row.ValidUntil, "2027-12-31 23:59:59"},
			{"verified_at", row.VerifiedAt, "2026-09-20 00:00:00"},
		} {
			if tc.got == nil || *tc.got != tc.want {
				t.Errorf("%s = %s, want the absorbed source's %q", tc.name, strOrNil(tc.got), tc.want)
			}
		}
	})

	t.Run("the reused row's own claims win and a half window is not completed", func(t *testing.T) {
		s := testStore(t)
		const kept = "the queue retries a job at most five times"
		keptID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: kept, Source: "mcp", Importance: 0.5,
			Agent: "claude-code", SourceRef: "docs/retries.md",
			ValidFrom: stampPtr("2026-03-01 00:00:00"),
		})
		if err != nil {
			t.Fatalf("Create kept: %v", err)
		}
		other := absorbed(t, s, "retries back off exponentially from one second")

		reuseMerge(t, s, kept, keptID, other)

		row := readRow(t, s, keptID)
		if row.Agent != "claude-code" || row.SourceRef != "docs/retries.md" {
			t.Errorf("agent/source_ref = %q/%q, want the reused row's own claude-code/docs/retries.md", row.Agent, row.SourceRef)
		}
		if row.ValidFrom == nil || *row.ValidFrom != "2026-03-01 00:00:00" {
			t.Errorf("valid_from = %s, want the reused row's own 2026-03-01", strOrNil(row.ValidFrom))
		}
		if row.ValidUntil != nil {
			t.Errorf("valid_until = %q, want NULL — completing the reused row's half window from another row composes a pair no row asserted", *row.ValidUntil)
		}
		if row.VerifiedAt == nil || *row.VerifiedAt != "2026-09-20 00:00:00" {
			t.Errorf("verified_at = %s, want the absorbed source's, since the reused row stated none", strOrNil(row.VerifiedAt))
		}
	})

	t.Run("the reused row's own confidence and complete window win", func(t *testing.T) {
		s := testStore(t)
		own := 0.3
		const kept = "the queue drains in under a minute after a deploy"
		keptID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: kept, Source: "mcp", Importance: 0.5, Confidence: &own,
			ValidFrom:  stampPtr("2025-06-01 00:00:00"),
			ValidUntil: stampPtr("2026-06-01 00:00:00"),
		})
		if err != nil {
			t.Fatalf("Create kept: %v", err)
		}
		other := absorbed(t, s, "a deploy pauses the queue consumers first")

		reuseMerge(t, s, kept, keptID, other)

		row := readRow(t, s, keptID)
		if row.Confidence == nil || *row.Confidence != own {
			t.Errorf("confidence = %s, want the reused row's own %v", floatOrNil(row.Confidence), own)
		}
		if row.ValidFrom == nil || *row.ValidFrom != "2025-06-01 00:00:00" ||
			row.ValidUntil == nil || *row.ValidUntil != "2026-06-01 00:00:00" {
			t.Errorf("window = %s..%s, want the reused row's own 2025-06-01..2026-06-01", strOrNil(row.ValidFrom), strOrNil(row.ValidUntil))
		}
	})

	t.Run("a decision companion merged into a reused fact keeps its link and evidence", func(t *testing.T) {
		s := testStore(t)
		_, companion, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "Redis lists", "already deployed for caching", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		var link string
		if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, companion).Scan(&link); err != nil || link == "" {
			t.Fatalf("the companion's link = %q (%v); the fixture needs one", link, err)
		}
		const kept = "the job queue backend is Redis lists, chosen because it was already deployed"
		keptID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", kept, "mcp", 0.5, nil,
			Provenance{Agent: "claude-code", SessionID: "ses_a"})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		reuseMerge(t, s, kept, keptID, companion)

		ev, err := s.MemoryProvenance(ctx, keptID)
		if err != nil {
			t.Fatalf("MemoryProvenance: %v", err)
		}
		want := []string{"observed:claude-code:", "observed::" + companion}
		if got := evidenceRowsOf(t, ev); !sameSet(got, want) {
			t.Errorf("the reused row's evidence = %v, want %v", got, want)
		}
		if row := readRow(t, s, keptID); row.SourceRef != link {
			t.Errorf("source_ref = %q, want the absorbed companion's decision link %q", row.SourceRef, link)
		}
	})
}

// claimsOf renders the claim columns a merge can fill, for a before/after
// comparison.
func claimsOf(m Memory) string {
	return fmt.Sprintf("agent=%q session=%q ref=%q conf=%s from=%s until=%s verified=%s",
		m.Agent, m.SessionID, m.SourceRef, floatOrNil(m.Confidence),
		strOrNil(m.ValidFrom), strOrNil(m.ValidUntil), strOrNil(m.VerifiedAt))
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func floatOrNil(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// TestRestoreUndoesTheEvidenceAMergeCarriedOntoARowItKeeps: a merge that keeps
// one row in place — ReplaceNonManual's reuse branch, and FoldRows' survivor —
// carries the absorbed rows' evidence onto it. A restore brings the absorbed rows
// back with their own evidence from the snapshot, and restores the kept row in
// place, so unless it also takes the carried copies off the kept row, every
// absorbed record exists twice and the kept row's evidence claims a verification
// its restored verified_at no longer states.
func TestRestoreUndoesTheEvidenceAMergeCarriedOntoARowItKeeps(t *testing.T) {
	ctx := context.Background()
	confidence := 0.7
	snapshotState := func(t *testing.T, s *Store, ids ...string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, id := range ids {
			ev, err := s.MemoryProvenance(ctx, id)
			if err != nil {
				t.Fatalf("MemoryProvenance(%s): %v", id, err)
			}
			got, err := s.GetByIDs(ctx, []string{id})
			if err != nil || len(got) != 1 {
				t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(got))
			}
			out[id] = strings.Join(evidenceRowsOf(t, ev), ",") + " | " + claimsOf(got[0])
		}
		return out
	}
	absorbed := func(t *testing.T, s *Store, source, content string) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: content, Source: source, Importance: 0.5,
			Agent: "codex", SessionID: "ses_b", SourceRef: "docs/queue.md", Confidence: &confidence,
			ValidFrom:  stampPtr("2026-01-15 00:00:00"),
			ValidUntil: stampPtr("2027-12-31 23:59:59"),
			VerifiedAt: stampPtr("2026-09-20 00:00:00"),
		})
		if err != nil {
			t.Fatalf("Create absorbed: %v", err)
		}
		return id
	}
	check := func(t *testing.T, s *Store, before map[string]string, keptID, otherID string) {
		t.Helper()
		if merged := snapshotState(t, s, keptID)[keptID]; merged == before[keptID] {
			t.Fatalf("the merge carried nothing onto %s, so the restore has nothing to undo: %s", keptID, merged)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := snapshotState(t, s, keptID, otherID)
		for _, id := range []string{keptID, otherID} {
			if after[id] != before[id] {
				t.Errorf("after the restore %s is\n  %s\nwant its pre-merge state\n  %s", id, after[id], before[id])
			}
		}
		// A second restore is a no-op: the snapshot is kept and restore repeatable.
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("second RestoreSnapshot: %v", err)
		}
		if again := snapshotState(t, s, keptID, otherID); again[keptID] != before[keptID] || again[otherID] != before[otherID] {
			t.Errorf("a second restore moved the state: %v, want %v", again, before)
		}
	}

	t.Run("ReplaceNonManual reuse merge", func(t *testing.T) {
		s := testStore(t)
		const kept = "the job queue backend is Redis lists"
		keptID, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: kept, Source: "mcp", Importance: 0.5, Agent: "claude-code",
		})
		if err != nil {
			t.Fatalf("Create kept: %v", err)
		}
		other := absorbed(t, s, "mcp", "the queue worker reads Redis lists with BLPOP")
		before := snapshotState(t, s, keptID, other)

		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: kept, Importance: 0.5, ReplacesIDs: []string{keptID, other},
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		check(t, s, before, keptID, other)
	})

	// A kept row the restore leaves alone — pinned, resolved or persistent after
	// the merge — keeps everything the merge gave it: the restore does not revert
	// its columns, so taking its carried records would leave it claiming the
	// merged verification with none of the evidence behind it.
	for _, guard := range []struct {
		name, sql string
	}{
		{"a kept row pinned after the merge is left alone", `UPDATE memories SET pinned = 1 WHERE id = ?`},
		{"a kept row resolved after the merge is left alone", `UPDATE memories SET resolved_at = datetime('now') WHERE id = ?`},
		{"a kept row made persistent after the merge is left alone", `UPDATE memories SET retention = '` + RetentionPersistent + `', expires_at = NULL WHERE id = ?`},
	} {
		t.Run(guard.name, func(t *testing.T) {
			s := testStore(t)
			const kept = "the job queue backend is Redis lists"
			keptID, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: kept, Source: "mcp", Importance: 0.5, Agent: "claude-code",
			})
			if err != nil {
				t.Fatalf("Create kept: %v", err)
			}
			other := absorbed(t, s, "mcp", "the queue worker reads Redis lists with BLPOP")
			before := snapshotState(t, s, keptID)

			if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: kept, Importance: 0.5, ReplacesIDs: []string{keptID, other},
			}}, "", nil); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, guard.sql, keptID); err != nil {
				t.Fatalf("guard the kept row: %v", err)
			}
			merged := snapshotState(t, s, keptID)
			if merged[keptID] == before[keptID] {
				t.Fatalf("the merge carried nothing onto %s, so this test proves nothing: %s", keptID, merged[keptID])
			}

			if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
				t.Fatalf("RestoreSnapshot: %v", err)
			}
			if after := snapshotState(t, s, keptID); after[keptID] != merged[keptID] {
				t.Errorf("the restore changed a row it leaves alone:\n  %s\nwant its merged state\n  %s", after[keptID], merged[keptID])
			}
		})
	}

	t.Run("FoldRows survivor", func(t *testing.T) {
		s := testStore(t)
		survivor, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "the job queue backend is Redis lists", Source: "reflection",
			Importance: 0.5, Agent: "claude-code",
		})
		if err != nil {
			t.Fatalf("Create survivor: %v", err)
		}
		folded := absorbed(t, s, "reflection", "the job queue backend is Redis lists, per the runbook")
		before := snapshotState(t, s, survivor, folded)
		rows, err := s.GetByIDs(ctx, []string{survivor, folded})
		if err != nil || len(rows) != 2 {
			t.Fatalf("GetByIDs: %v (n=%d)", err, len(rows))
		}
		plan := map[string]FoldRow{}
		for _, m := range rows {
			plan[m.ID] = FoldRow{ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt}
		}
		res, err := s.FoldRows(ctx, testProject, []FoldCluster{{Survivor: plan[survivor], Folded: []FoldRow{plan[folded]}}}, "")
		if err != nil {
			t.Fatalf("FoldRows: %v", err)
		}
		if len(res.Folded) != 1 {
			t.Fatalf("FoldRows folded %v (skipped %+v), want the one row", res.Folded, res.Skipped)
		}
		check(t, s, before, survivor, folded)
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
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "convention", Content: rewritten, Importance: 0.7,
		ReplacesIDs: []string{id},
	}}, "", nil); err != nil {
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
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: original, Importance: 0.5,
	}}, "", nil); err != nil {
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
