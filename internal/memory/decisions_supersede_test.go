package memory

import (
	"context"
	"testing"
	"time"
)

// decisionSupersedeFixture records a reversed decision and the one that
// replaced it, in that order, and returns both ids: the two decisions, and the
// companion memories RecordDecision wrote beside each.
func decisionSupersedeFixture(t *testing.T, s *Store, ctx context.Context) (oldDecisionID, newDecisionID, oldMemoryID, newMemoryID string) {
	t.Helper()
	var err error
	oldDecisionID, oldMemoryID, _, err = s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (old): %v", err)
	}
	newDecisionID, newMemoryID, _, err = s.RecordDecision(ctx, testProject,
		"Reverse: use Postgres for the job queue", "SKIP LOCKED on Postgres",
		"Redis lost jobs on failover", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}
	return oldDecisionID, newDecisionID, oldMemoryID, newMemoryID
}

// decisionSupersedePassiveRequest is a session-start-shaped read: one project
// bucket, decay order, a window wide enough that both companion memories are
// candidates rather than a ranking decision.
func decisionSupersedePassiveRequest() CandidateRequest {
	return CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Now:       time.Now().UTC(),
		Fetch:     Fetch{Limit: 20},
		Params:    DefaultSearchParams(),
		Condition: CondHybrid,
		Passive: []SlicePolicy{{
			Bucket:            testProject,
			Order:             OrderDecay,
			OverFetch:         50,
			TwoPass:           false,
			DemotionThreshold: 0.99,
		}},
	}
}

// TestSupersedeDecisionRetiresTheCompanionMemory covers the finding that put
// this on the board: the decisions row reported `status: superseded` while
// search and session start went on returning the reversed decision as current,
// because the companion memory nothing linked to that status stayed live.
func TestSupersedeDecisionRetiresTheCompanionMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	oldDecisionID, newDecisionID, oldMemoryID, newMemoryID := decisionSupersedeFixture(t, s, ctx)

	if err := s.SupersedeDecision(ctx, testProject, oldDecisionID, newDecisionID); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	// Active search demotes a resolved memory rather than filtering it, so a
	// query matching both decisions must rank the replacement above the
	// reversed decision.
	set, err := s.Candidates(ctx, candidateRequest("job queue", 10, now))
	if err != nil {
		t.Fatalf("search after supersede: %v", err)
	}
	pos := map[string]int{}
	for i, id := range candidateIDs(t, set) {
		pos[id] = i
	}
	if _, ok := pos[newMemoryID]; !ok {
		t.Fatalf("search does not return the replacement's companion %s", newMemoryID)
	}
	if po, ok := pos[oldMemoryID]; ok && po < pos[newMemoryID] {
		t.Errorf("search ranks the reversed decision (%d) above its replacement (%d)", po, pos[newMemoryID])
	}

	// The passive read session start runs binds resolved_at IS NULL in SQL
	// rather than by rank, so the reversed decision is absent outright.
	set, err = s.Candidates(ctx, decisionSupersedePassiveRequest())
	if err != nil {
		t.Fatalf("session-start read after supersede: %v", err)
	}
	var foundOld, foundNew bool
	for _, id := range candidateIDs(t, set) {
		switch id {
		case oldMemoryID:
			foundOld = true
		case newMemoryID:
			foundNew = true
		}
	}
	if foundOld {
		t.Errorf("session start still returns the reversed decision's companion %s", oldMemoryID)
	}
	if !foundNew {
		t.Errorf("session start no longer returns the replacement's companion %s", newMemoryID)
	}
}

// TestSupersedeDecisionRecordsTheNewDecisionAsTheReason pins the half of the
// retirement that is not a filter: the memory must say what replaced it, or a
// reader of its history can only see that it was retired and not by what. The
// reason is the replacement's companion memory — the memory-level stand-in for
// the decision that supersedes it — recorded both as the 'supersede' history
// row's related_id and as the 'supersedes' edge's source.
func TestSupersedeDecisionRecordsTheNewDecisionAsTheReason(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldDecisionID, newDecisionID, oldMemoryID, newMemoryID := decisionSupersedeFixture(t, s, ctx)

	if err := s.SupersedeDecision(ctx, testProject, oldDecisionID, newDecisionID); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	hist, err := s.MemoryHistory(ctx, oldMemoryID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(hist) < 2 {
		t.Fatalf("the companion recorded %d history row(s), want its save and its retirement", len(hist))
	}
	retired := hist[len(hist)-1]
	if retired.Phase != phaseSupersede {
		t.Errorf("retirement history row phase = %q, want %q", retired.Phase, phaseSupersede)
	}
	if retired.RelatedID != newMemoryID {
		t.Errorf("retirement history row related_id = %q, want the replacement's companion %s", retired.RelatedID, newMemoryID)
	}
	// The row records the state the memory had when it was retired, so the
	// retired text is still readable and still says what it used to say.
	if retired.Content == "" {
		t.Error("the retirement history row copied no content, so the retired claim is unreadable")
	}

	links, err := s.GetLinks(ctx, oldMemoryID)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("no link was written, so nothing records which memory replaced the retired one")
	}
	var edge *Link
	for i := range links {
		if links[i].Relation == "supersedes" {
			edge = &links[i]
			break
		}
	}
	if edge == nil {
		t.Fatalf("no 'supersedes' edge on the retired companion, got %d other link(s)", len(links))
	}
	if edge.Source != "manual" {
		t.Errorf("'supersedes' edge source = %q, want manual: no classifier judged it", edge.Source)
	}
	if edge.SourceID != newMemoryID || edge.TargetID != oldMemoryID {
		t.Errorf("'supersedes' edge runs %s -> %s, want %s -> %s",
			edge.SourceID, edge.TargetID, newMemoryID, oldMemoryID)
	}
}

// TestSupersedeDecisionTwiceIsIdempotent pins the other failure a second run
// could produce: a second history row for a retirement that already happened,
// or a resolved_at moved backwards. Both read as "something changed" to a
// reader that did not expect anything to.
func TestSupersedeDecisionTwiceIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldDecisionID, newDecisionID, oldMemoryID, _ := decisionSupersedeFixture(t, s, ctx)

	for range 2 {
		if err := s.SupersedeDecision(ctx, testProject, oldDecisionID, newDecisionID); err != nil {
			t.Fatalf("SupersedeDecision: %v", err)
		}
	}

	hist, err := s.MemoryHistory(ctx, oldMemoryID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var retirements int
	for _, e := range hist {
		if e.Phase == phaseSupersede {
			retirements++
		}
	}
	if retirements != 1 {
		t.Errorf("the companion recorded %d 'supersede' history row(s) for one retirement, want 1", retirements)
	}

	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	for _, d := range all {
		if d.ID != oldDecisionID {
			continue
		}
		if d.Status != "superseded" || d.SupersededBy != newDecisionID {
			t.Errorf("re-running the supersede moved the decisions row to status %q superseded_by %q", d.Status, d.SupersededBy)
		}
	}
}

// TestSupersedeDecisionWithoutACompanionMemorySucceeds covers the shape a
// portable restore leaves behind: ImportDecision writes a decisions row and no
// companion, so a decision taken from an artifact can be superseded without
// there being anything to retire. That is a normal outcome, not a failure.
func TestSupersedeDecisionWithoutACompanionMemorySucceeds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldDecisionID, oldMemoryID, _, err := s.RecordDecision(ctx, testProject,
		"Use SQLite for storage", "Embedded SQLite with FTS5", "Zero external deps", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	newDecisionID, _, _, err := s.RecordDecision(ctx, testProject,
		"Reverse: Postgres for storage", "Row-level locking", "SQLite hit a wall", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}
	if err := s.Delete(ctx, oldMemoryID); err != nil {
		t.Fatalf("Delete the old companion: %v", err)
	}

	if err := s.SupersedeDecision(ctx, testProject, oldDecisionID, newDecisionID); err != nil {
		t.Fatalf("SupersedeDecision with no companion to retire: %v", err)
	}

	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(all) != 2 || all[1].ID != oldDecisionID || all[1].Status != "superseded" {
		t.Errorf("the decisions row was not marked superseded without a companion: %v", all)
	}
}

func decisionHistoryPhases(t *testing.T, s *Store, ctx context.Context, memoryID string) []string {
	t.Helper()
	hist, err := s.MemoryHistory(ctx, memoryID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var out []string
	for _, e := range hist {
		out = append(out, e.Phase)
	}
	return out
}

func decisionMemoryResolved(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id).Scan(&n); err != nil {
		t.Fatalf("read resolved_at: %v", err)
	}
	return n == 1
}

// Two decisions with identical text must never be confused: superseding one
// retires only its own companion.
func TestSupersedeDecisionWithIdenticalTextRetiresOnlyItsOwnCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rec := func(title string) (string, string) {
		d, m, _, err := s.RecordDecision(ctx, testProject, title, "same decision", "same rationale", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		return d, m
	}
	decA, memA := rec("Same title")
	decB, memB := rec("Same title")
	decC, memC := rec("Replacement title")

	// Supersede the FIRST-recorded of the identical pair, so a newest-wins or
	// oldest-wins tie-break would each get one of the two orders wrong.
	if err := s.SupersedeDecision(ctx, testProject, decA, decC); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}
	if !decisionMemoryResolved(t, s, memA) {
		t.Errorf("the superseded decision's own companion %s is still live", memA)
	}
	if decisionMemoryResolved(t, s, memB) {
		t.Errorf("the identical-text decision's companion %s was retired", memB)
	}
	if decisionMemoryResolved(t, s, memC) {
		t.Errorf("the replacement's companion %s was retired", memC)
	}

	// And the other order: superseding B retires B's companion, not A's again.
	if err := s.SupersedeDecision(ctx, testProject, decB, decC); err != nil {
		t.Fatalf("SupersedeDecision (B): %v", err)
	}
	if !decisionMemoryResolved(t, s, memB) {
		t.Errorf("companion %s of the second superseded decision is still live", memB)
	}
	if got := decisionHistoryPhases(t, s, ctx, memA); len(got) != 2 {
		t.Errorf("companion A history phases = %v, want save and one retirement", got)
	}
}

// The link is the id, not the text: editing the companion's content must not
// make the decision lose it.
func TestSupersedeDecisionFindsAnEditedCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
	if _, err := s.db.Exec(`UPDATE memories SET content = 'edited by hand' WHERE id = ?`, oldMem); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}
	if !decisionMemoryResolved(t, s, oldMem) {
		t.Error("an edited companion was not retired")
	}
}

// A companion that predates the source_ref link is found by text only when the
// text is unambiguous.
func TestSupersedeDecisionLegacyCompanionMatchIsUnambiguousOnly(t *testing.T) {
	ctx := context.Background()
	unlink := func(t *testing.T, s *Store, ids ...string) {
		for _, id := range ids {
			if _, err := s.db.Exec(`UPDATE memories SET source_ref = NULL WHERE id = ?`, id); err != nil {
				t.Fatalf("unlink: %v", err)
			}
		}
	}
	t.Run("unique text is retired", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
		unlink(t, s, oldMem, newMem)
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if !decisionMemoryResolved(t, s, oldMem) {
			t.Error("unambiguous legacy companion was not retired")
		}
	})
	t.Run("duplicate text retires nothing", func(t *testing.T) {
		s := testStore(t)
		rec := func(title string) (string, string) {
			d, m, _, err := s.RecordDecision(ctx, testProject, title, "same", "same", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			return d, m
		}
		decA, memA := rec("T")
		_, memB := rec("T")
		decC, memC := rec("Other")
		unlink(t, s, memA, memB, memC)
		if err := s.SupersedeDecision(ctx, testProject, decA, decC); err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, memA) || decisionMemoryResolved(t, s, memB) {
			t.Error("an ambiguous legacy match retired a companion that may belong to another decision")
		}
	})
}

// Old and new compose to the same text and the old companion is gone: the
// replacement's own companion must not be retired or linked to itself.
func TestSupersedeDecisionNeverRetiresTheReplacementsOwnCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, _, _, err := s.RecordDecision(ctx, testProject, "T", "same", "same", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Re-record the same text; the first record's companion is then deleted.
	newDec, newMem, _, err := s.RecordDecision(ctx, testProject, "T", "same", "same", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM memories WHERE source_ref = ?`, decisionCompanionRef(oldDec)); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
		t.Fatal(err)
	}
	if decisionMemoryResolved(t, s, newMem) {
		t.Error("the replacement's own companion was retired")
	}
	var self int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_links WHERE source_id = target_id`).Scan(&self); err != nil {
		t.Fatal(err)
	}
	if self != 0 {
		t.Errorf("%d self link(s) written", self)
	}
}

// A retirement is never a state change without a history row.
func TestSupersedeDecisionWithoutAReplacementCompanionStillRecordsHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
	if err := s.Delete(ctx, newMem); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
		t.Fatal(err)
	}
	if !decisionMemoryResolved(t, s, oldMem) {
		t.Fatal("companion not retired")
	}
	phases := decisionHistoryPhases(t, s, ctx, oldMem)
	if len(phases) != 2 || phases[1] != phaseResolve {
		t.Errorf("history phases = %v, want save then %s", phases, phaseResolve)
	}
}

// When the reverse edge is already live no edge is written, and history must
// not claim a supersession the graph contradicts.
func TestSupersedeDecisionDoesNotClaimAnEdgeItRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
	if _, err := s.db.Exec(`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES (?, ?, 'supersedes', 1, 'manual')`, oldMem, newMem); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
		t.Fatal(err)
	}
	hist, err := s.MemoryHistory(ctx, oldMem, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range hist {
		if e.Phase == phaseSupersede {
			t.Errorf("history claims a supersede by %s while the reverse edge is live", e.RelatedID)
		}
	}
	if !decisionMemoryResolved(t, s, oldMem) {
		t.Error("companion not retired")
	}
}

// A pinned companion is left live, writes nothing, and is reported.
func TestSupersedeDecisionReportsAPinnedCompanionItLeftLive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
	if _, err := s.db.Exec(`UPDATE memories SET pinned = 1 WHERE id = ?`, oldMem); err != nil {
		t.Fatal(err)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Retired) != 0 || len(got.Declined) != 1 || got.Declined[0] != oldMem {
		t.Errorf("report = %+v, want the pinned companion declined", got)
	}
	if decisionMemoryResolved(t, s, oldMem) {
		t.Error("a pinned companion was retired")
	}
	if phases := decisionHistoryPhases(t, s, ctx, oldMem); len(phases) != 1 {
		t.Errorf("history phases = %v, want only the save", phases)
	}
}

// A default import re-stamps every memory's source, so the link must not depend
// on it; and a link naming two memories is ambiguous and retires neither.
func TestSupersedeDecisionLinkLookup(t *testing.T) {
	ctx := context.Background()
	t.Run("downgraded source still found", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		if _, err := s.db.Exec(`UPDATE memories SET source = 'onboarding' WHERE id = ?`, oldMem); err != nil {
			t.Fatal(err)
		}
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if !decisionMemoryResolved(t, s, oldMem) {
			t.Error("a companion with a downgraded source was not retired")
		}
	})
	t.Run("copied link retires nothing", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		other, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "an unrelated memory", Source: "manual", Importance: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, decisionCompanionRef(oldDec), other); err != nil {
			t.Fatal(err)
		}
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, other) || decisionMemoryResolved(t, s, oldMem) {
			t.Error("an ambiguous link retired a memory")
		}
	})
}
