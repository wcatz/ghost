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

	// Active search (query-based) demotes resolved memories but doesn't filter
	// them entirely — a query matching only the retired companion will still
	// return it, just with a halved score. The passive session-start read,
	// however, binds resolved_at IS NULL in SQL and filters it completely.
	set, err := s.Candidates(ctx, candidateRequest("Redis lists as the queue backend", 10, now))
	if err != nil {
		t.Fatalf("search after supersede: %v", err)
	}
	// The query only matches the old memory, so it's returned (demoted but
	// still the only hit). The passive path below is the one that withholds it.
	if got := candidateIDs(t, set); len(got) == 0 {
		t.Errorf("active search returned nothing for a query matching only the retired memory; expected it (demoted)")
	}

	// And the same is true of the passive read session start runs, which binds
	// resolved_at IS NULL in SQL rather than by rank.
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
