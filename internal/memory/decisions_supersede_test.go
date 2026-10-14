package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// A companion promoted to _global before the supersede is still this decision's
// companion. Promotion moves memories.project_id only and leaves the
// source_ref link naming the decision alone, so the retirement that scoped
// every read to the project found nothing and left the reversed decision live
// in the one project every other project reads its session start from.
func TestSupersedeDecisionRetiresAPromotedCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldDecisionID, newDecisionID, oldMemoryID, newMemoryID := decisionSupersedeFixture(t, s, ctx)
	if err := s.PromoteToGlobal(ctx, testProject, oldMemoryID); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	got, err := s.SupersedeDecisionReport(ctx, testProject, oldDecisionID, newDecisionID)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != oldMemoryID {
		t.Fatalf("retirement report = %+v, want the promoted companion retired", got)
	}
	if !decisionMemoryResolved(t, s, oldMemoryID) {
		t.Error("the promoted companion was left live")
	}
	// The retirement withholds the row; it does not move it. A promoted memory
	// stays in _global, because that is where the caller put it.
	projectID := memoryProject(t, s, oldMemoryID)
	if projectID != GlobalProjectID {
		t.Errorf("the promoted companion now reads as project %q, want %q", projectID, GlobalProjectID)
	}
	// The reason survives, and it names the replacement's companion.
	if phases := decisionHistoryPhases(t, s, ctx, oldMemoryID); len(phases) < 2 || phases[len(phases)-1] != phaseSupersede {
		t.Errorf("history phases = %v, want a final %q naming the replacement", phases, phaseSupersede)
	}
	links, err := s.GetLinks(ctx, oldMemoryID)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	var edge bool
	for _, l := range links {
		if l.Relation == "supersedes" && l.SourceID == newMemoryID && l.TargetID == oldMemoryID {
			edge = true
		}
	}
	if !edge {
		t.Errorf("no supersedes edge %s -> %s, so nothing records what replaced the promoted companion", newMemoryID, oldMemoryID)
	}
}

// A replacement whose own companion was promoted names the reason from just as
// little as one that was not: the retirement reaches _global on both sides of
// the edge, so the two promoted rows are joined by the edge that claims it.
func TestSupersedeDecisionNamesAPromotedReplacementAsTheReason(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldDecisionID, newDecisionID, oldMemoryID, newMemoryID := decisionSupersedeFixture(t, s, ctx)
	if err := s.PromoteToGlobal(ctx, testProject, newMemoryID); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	got, err := s.SupersedeDecisionReport(ctx, testProject, oldDecisionID, newDecisionID)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != oldMemoryID {
		t.Fatalf("retirement report = %+v, want the companion retired", got)
	}
	links, err := s.GetLinks(ctx, oldMemoryID)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	var edge bool
	for _, l := range links {
		if l.Relation != "supersedes" {
			continue
		}
		edge = true
		if l.SourceID != newMemoryID || l.TargetID != oldMemoryID {
			t.Errorf("the edge runs %s -> %s, want it from the promoted replacement %s", l.SourceID, l.TargetID, newMemoryID)
		}
	}
	if !edge {
		t.Errorf("no supersedes edge %s -> %s, so nothing records what replaced the companion", newMemoryID, oldMemoryID)
	}
}

// memoryProject reads the project a memory row currently names, which is the
// one thing the supersede must not change.
func memoryProject(t *testing.T, s *Store, id string) string {
	t.Helper()
	var projectID string
	if err := s.db.QueryRow(`SELECT project_id FROM memories WHERE id = ?`, id).Scan(&projectID); err != nil {
		t.Fatalf("read project of %s: %v", id, err)
	}
	return projectID
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
		got, err := s.SupersedeDecisionReport(ctx, testProject, decA, decC)
		if err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, memA) || decisionMemoryResolved(t, s, memB) {
			t.Error("an ambiguous legacy match retired a companion that may belong to another decision")
		}
		if len(got.Ambiguous) != 2 {
			t.Errorf("ambiguous = %v, want both same-text companions reported", got.Ambiguous)
		}
	})
}

// A companion recategorised by ghost_memory_update keeps its decision_log
// source, which no writer tool can set and a default import downgrades, so it is
// found whatever its category is now — by the link, and by the composed text when
// it predates the link — and the resolve guard then decides: an architecture
// companion is retired, a convention companion is left live and reported in
// Declined.
func TestSupersedeDecisionFindsARecategorisedCompanion(t *testing.T) {
	ctx := context.Background()
	for _, legacy := range []bool{false, true} {
		for _, tc := range []struct {
			category string
			retired  bool
		}{
			{category: "architecture", retired: true},
			{category: "convention", retired: false},
		} {
			name := tc.category
			if legacy {
				name = "legacy " + name
			}
			t.Run(name, func(t *testing.T) {
				s := testStore(t)
				oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
				if legacy {
					if _, err := s.db.Exec(`UPDATE memories SET source_ref = NULL WHERE id IN (?, ?)`, oldMem, newMem); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.db.Exec(`UPDATE memories SET category = ? WHERE id = ?`, tc.category, oldMem); err != nil {
					t.Fatal(err)
				}
				got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
				if err != nil {
					t.Fatal(err)
				}
				if tc.retired {
					if !decisionMemoryResolved(t, s, oldMem) || len(got.Retired) != 1 || got.Retired[0] != oldMem {
						t.Errorf("a recategorised %s companion was not retired: %+v", tc.category, got)
					}
					return
				}
				if decisionMemoryResolved(t, s, oldMem) {
					t.Errorf("the resolve guard was bypassed for a %s companion", tc.category)
				}
				if len(got.Declined) != 1 || got.Declined[0] != oldMem {
					t.Errorf("a %s companion left live is not reported in Declined: %+v", tc.category, got)
				}
			})
		}
	}
}

// A reflect successor that inherited the decision link is the companion now, and
// the link arm has to find it. Both routes to such a row leave it holding the
// link and a category that is not 'decision': an LLM merge that names a
// non-decision row first inserts a fresh row with the link carried onto it, and
// the SQLite tier's cross-category clustering rewrites the companion in place
// onto the cluster head's category. Both write source 'reflection', which no
// writer tool can set — the only writers are ReplaceNonManual itself, whose link
// is inherited from the row it stands in for, and an import told to trust the
// artifact's own provenance, which is a restore of the same shape. So the row is
// found, the resolve guard then retires it or reports it in Declined, and the
// supersede answer names it rather than coming back clean.
func TestSupersedeDecisionFindsAReflectSuccessorThatInheritedTheLink(t *testing.T) {
	ctx := context.Background()
	// The routes, as the store produces them: insert mints the merged row the
	// link is carried onto, reuse rewrites the companion in place onto the head's
	// category. Both keep the link.
	routes := []struct {
		name   string
		insert bool
	}{
		{name: "merge successor", insert: true},
		{name: "reused companion", insert: false},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			s := testStore(t)
			oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
			oldText, _ := decisionCompanionContent(
				"Use Redis for the job queue", "Redis lists as the queue backend", "Already deployed for caching")
			newText, _ := decisionCompanionContent(
				"Reverse: use Postgres for the job queue", "SKIP LOCKED on Postgres", "Redis lost jobs on failover")
			// The old companion's successor: category fact (the head's), source
			// reflection, link intact. The route decides whether that is a fresh
			// row or the companion rewritten in place.
			oldContent := oldText
			if route.insert {
				oldContent = "Use Postgres for the job queue: SKIP LOCKED on Postgres, replacing the Redis lists approach"
			}
			if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
				{Category: "decision", Content: newText, Importance: 0.9, ReplacesIDs: []string{newMem}},
				{Category: "fact", Content: oldContent, Importance: 0.9, ReplacesIDs: []string{oldMem}},
			}, "", nil); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			successor := oldMem
			if route.insert {
				successor = memoryBySourceRef(t, s, oldDec)
				if successor == "" {
					t.Fatal("the merged successor did not inherit the decision link")
				}
			}
			var category, source string
			if err := s.db.QueryRow(
				`SELECT category, source FROM memories WHERE id = ?`, successor).Scan(&category, &source); err != nil {
				t.Fatalf("read successor: %v", err)
			}
			if category != "fact" || source != "reflection" {
				t.Fatalf("the successor is %s/%s, want fact/reflection — the route no longer reproduces the finding", category, source)
			}

			got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
			if err != nil {
				t.Fatalf("SupersedeDecisionReport: %v", err)
			}
			if len(got.Retired) != 1 || got.Retired[0] != successor {
				t.Errorf("a reflect successor holding the link was not retired: %+v", got)
			}
			if !decisionMemoryResolved(t, s, successor) {
				t.Error("the successor is still live: search and session start keep answering with the reversed decision")
			}
			if len(got.Retired) == 0 && len(got.Declined) == 0 && len(got.Ambiguous) == 0 {
				t.Error("a supersede that left the successor live was answered as a clean success")
			}
		})
	}
}

// memoryBySourceRef returns the live memory carrying the decision link, or "".
func memoryBySourceRef(t *testing.T, s *Store, decisionID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE source_ref = ?`, decisionCompanionRef(decisionID)).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		t.Fatalf("read linked memory: %v", err)
	}
	return id
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
	copyOf := func(t *testing.T, s *Store, decisionID, source string) string {
		id, err := s.Create(ctx, testProject, Memory{Category: "decision", Content: "a copy", Source: source, Importance: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, decisionCompanionRef(decisionID), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Run("an import-shaped copy leaves the real companion retired", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		cp := copyOf(t, s, oldDec, "onboarding")
		got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
		if err != nil {
			t.Fatal(err)
		}
		if !decisionMemoryResolved(t, s, oldMem) {
			t.Error("the real companion was not retired")
		}
		if decisionMemoryResolved(t, s, cp) || len(got.Retired) != 1 || len(got.Ambiguous) != 0 {
			t.Errorf("the copy was touched or the report is wrong: %+v", got)
		}
	})
	t.Run("two decision_log rows retire nothing and are reported", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		cp := copyOf(t, s, oldDec, "decision_log")
		got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
		if err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, cp) || decisionMemoryResolved(t, s, oldMem) {
			t.Error("an ambiguous link retired a memory")
		}
		if len(got.Ambiguous) != 2 || len(got.Retired) != 0 {
			t.Errorf("report = %+v, want both claimants ambiguous", got)
		}
	})
	t.Run("a default import of a second claimant keeps the real companion retired", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		imp, _, downgraded, err := s.ImportMemory(ctx, PortableMemory{
			ID: "IMPORTED1", ProjectID: testProject, Category: "decision", Content: "carried by an artifact",
			Source: "decision_log", SourceRef: decisionCompanionRef(oldDec),
		}, ImportOptions{Apply: true})
		if err != nil || !imp || !downgraded {
			t.Fatalf("ImportMemory created=%v downgraded=%v err=%v", imp, downgraded, err)
		}
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if !decisionMemoryResolved(t, s, oldMem) {
			t.Error("an imported claimant left the real companion live")
		}
		if decisionMemoryResolved(t, s, "IMPORTED1") {
			t.Error("the imported claimant was retired")
		}
	})
}

// A row forged with the decision link but not the decision category is not the
// companion: not for a legacy decision (real companion unlinked) and not for a
// decision whose companion was deleted.
func TestSupersedeDecisionIgnoresAForgedLinkOnANonDecisionMemory(t *testing.T) {
	ctx := context.Background()
	forge := func(t *testing.T, s *Store, decisionID string) string {
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "an unrelated memory", Source: "manual", Importance: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, decisionCompanionRef(decisionID), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Run("legacy decision", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, newMem := decisionSupersedeFixture(t, s, ctx)
		if _, err := s.db.Exec(`UPDATE memories SET source_ref = NULL WHERE id IN (?, ?)`, oldMem, newMem); err != nil {
			t.Fatal(err)
		}
		forged := forge(t, s, oldDec)
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, forged) {
			t.Error("a forged non-decision memory was retired")
		}
		if !decisionMemoryResolved(t, s, oldMem) {
			t.Error("the real legacy companion was not retired")
		}
	})
	t.Run("deleted companion", func(t *testing.T) {
		s := testStore(t)
		oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
		if err := s.Delete(ctx, oldMem); err != nil {
			t.Fatal(err)
		}
		forged := forge(t, s, oldDec)
		if err := s.SupersedeDecision(ctx, testProject, oldDec, newDec); err != nil {
			t.Fatal(err)
		}
		if decisionMemoryResolved(t, s, forged) {
			t.Error("a forged non-decision memory was retired")
		}
	})
}

func TestWriterPathsRefuseTheReservedDecisionRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "forged link", "manual", 0.5, nil,
		UpsertOptions{Provenance: Provenance{SourceRef: "decision:abc"}})
	if err == nil {
		t.Error("UpsertWithOptions accepted a reserved source_ref")
	}
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "plain", Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{Provenance: Provenance{SourceRef: "decision:abc"}}); err == nil {
		t.Error("UpdateMemoryWithOptions accepted a reserved source_ref")
	}
}

// TestSupersedeDecisionRefusesToDropALiveSuccessor is #1144 at the store: A was
// replaced by B and B is still live, so a supersede of A by C would overwrite
// the only record that B replaced A. It is refused inside the transaction, so a
// second writer that passed the handler's read-only check cannot slip it through,
// and A keeps pointing at B. The same pair again stays a clean no-op
// (TestSupersedeDecisionTwiceIsIdempotent).
func TestSupersedeDecisionRefusesToDropALiveSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, b, _, _ := decisionSupersedeFixture(t, s, ctx)
	if err := s.SupersedeDecision(ctx, testProject, a, b); err != nil {
		t.Fatalf("SupersedeDecision(A, B): %v", err)
	}
	c, _, _, err := s.RecordDecision(ctx, testProject, "Use SQS", "SQS", "managed", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (C): %v", err)
	}

	err = s.SupersedeDecision(ctx, testProject, a, c)
	if err == nil {
		t.Fatal("SupersedeDecision(A, C) succeeded while A's successor B is live; B's link was dropped")
	}
	if !strings.Contains(err.Error(), b) {
		t.Errorf("the refusal does not name the live successor %s: %v", b, err)
	}

	succ, err := s.LiveDecisionSuccessor(ctx, testProject, a)
	if err != nil {
		t.Fatalf("LiveDecisionSuccessor: %v", err)
	}
	if succ != b {
		t.Errorf("LiveDecisionSuccessor(A) = %q, want %s", succ, b)
	}
	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	for _, d := range all {
		switch d.ID {
		case a:
			if d.Status != "superseded" || d.SupersededBy != b {
				t.Errorf("A is %s superseded_by %q, want superseded by %s", d.Status, d.SupersededBy, b)
			}
		case b, c:
			if d.Status != "active" {
				t.Errorf("%s is %s, want active", d.ID, d.Status)
			}
		}
	}

	// Once B is itself replaced, A has no live successor and repoints.
	if err := s.SupersedeDecision(ctx, testProject, b, c); err != nil {
		t.Fatalf("SupersedeDecision(B, C): %v", err)
	}
	if succ, err := s.LiveDecisionSuccessor(ctx, testProject, a); err != nil || succ != "" {
		t.Errorf("LiveDecisionSuccessor(A) after B was replaced = %q, %v; want none", succ, err)
	}
	if err := s.SupersedeDecision(ctx, testProject, a, c); err != nil {
		t.Errorf("SupersedeDecision(A, C) after B was replaced: %v", err)
	}
}

// TestSupersedeDecisionDeclinesASuccessorThatStandsForTwoDecisions covers the
// finding that a consolidation merge folding two decision companions into one
// row kept only the first decision's link. Superseding the other decision was
// then answered as a clean success while its reversed text stayed live, and
// superseding the first retired a row that still stated the other decision.
// The successor records every decision it stands for; a successor standing for
// more than one is reported rather than retired (which would withhold a current
// decision along with the reversed one) or ignored (which would leave the
// reversed one ranking).
func TestSupersedeDecisionDeclinesASuccessorThatStandsForTwoDecisions(t *testing.T) {
	ctx := context.Background()
	const merged = "Queue: use Postgres (SKIP LOCKED); retries max 5 attempts because of the upstream rate limit"

	// build records three decisions and folds the first two companions into one
	// successor, naming first first in the merge so both link orders are
	// exercised; the third companion is kept verbatim and stands in as the
	// replacement. It returns the store, the two folded decisions, the
	// replacement decision, and the successor row.
	build := func(t *testing.T, first string) (s *Store, firstDec, secondDec, replacementDec, successor string) {
		t.Helper()
		s = testStore(t)
		record := func(title, decision, rationale string) (string, string) {
			dec, mem, _, err := s.RecordDecision(ctx, testProject, title, decision, rationale, nil, nil)
			if err != nil {
				t.Fatalf("RecordDecision(%q): %v", title, err)
			}
			return dec, mem
		}
		aDec, aMem := record("Queue backend", "use Postgres", "already deployed for the cache")
		cDec, cMem := record("Queue retries", "max 5 attempts", "the upstream rate limit")
		dDec, dMem := record("Queue retries v2", "max 3 attempts", "the upstream limit is lower")
		var heldContent string
		if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, dMem).Scan(&heldContent); err != nil {
			t.Fatalf("read held companion content: %v", err)
		}
		replaces := []string{aMem, cMem}
		if first == "c" {
			replaces = []string{cMem, aMem}
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "decision", Content: merged, Importance: 0.9, ReplacesIDs: replaces},
			{Category: "decision", Content: heldContent, Importance: 0.9},
		}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		successor = onlyMemoryWithContent(t, s, merged)
		if first == "a" {
			return s, aDec, cDec, dDec, successor
		}
		return s, cDec, aDec, dDec, successor
	}

	for _, first := range []string{"a", "c"} {
		t.Run("merge names "+first+" first", func(t *testing.T) {
			s, firstDec, secondDec, replacementDec, successor := build(t, first)

			// The successor has to carry BOTH links whichever source was named
			// first: one link is the defect, and neither decision may be the one
			// silently dropped.
			var ref string
			if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, successor).Scan(&ref); err != nil {
				t.Fatalf("read successor link: %v", err)
			}
			if !sourceRefNamesDecision(ref, firstDec) || !sourceRefNamesDecision(ref, secondDec) {
				t.Fatalf("the merge successor's link %q does not name both decisions", ref)
			}

			// Superseding either folded decision must report the successor
			// rather than retire it or answer clean.
			for _, oldDec := range []string{firstDec, secondDec} {
				got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, replacementDec)
				if err != nil {
					t.Fatalf("SupersedeDecisionReport(%s): %v", oldDec, err)
				}
				if len(got.Retired) != 0 {
					t.Errorf("superseding %s retired %v; the successor still states a live decision", oldDec, got.Retired)
				}
				if len(got.Shared) != 1 || got.Shared[0] != successor {
					t.Errorf("superseding %s shared = %v, want the multi-decision successor %s", oldDec, got.Shared, successor)
				}
				// It is not a competing claimant: the ambiguity channel's remedy is to
				// delete the extras, and this row is the only memory of a live decision.
				if len(got.Ambiguous) != 0 {
					t.Errorf("superseding %s reported the shared successor as ambiguous %v", oldDec, got.Ambiguous)
				}
				if decisionMemoryResolved(t, s, successor) {
					t.Errorf("superseding %s withheld the successor; the other decision's text went with it", oldDec)
				}
			}
		})
	}
}

// TestWriterPathsRefuseADecisionTokenAnywhereInTheSourceRef: the decision lookup
// reads a source_ref as a comma-separated list and takes any token carrying the
// decision prefix as a link, so the writer guard refuses that token wherever the
// reader would accept one — not only as a leading prefix. Otherwise a caller
// saving "runbook.md,decision:<id>" in the decision category becomes a claimed
// companion of that decision, retired when it is superseded.
func TestWriterPathsRefuseADecisionTokenAnywhereInTheSourceRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, ref := range []string{"runbook.md,decision:abc", "a,b,decision:abc", "docs/x.md,decision:"} {
		_, _, _, err := s.UpsertWithOptions(ctx, testProject, "decision", "forged link "+ref, "manual", 0.5, nil,
			UpsertOptions{Provenance: Provenance{SourceRef: ref}})
		if err == nil {
			t.Errorf("UpsertWithOptions accepted %q, a source_ref the decision lookup reads as a link", ref)
		}
		id, err := s.Create(ctx, testProject, Memory{Category: "decision", Content: "plain " + ref, Source: "manual", Importance: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{Provenance: Provenance{SourceRef: ref}}); err == nil {
			t.Errorf("UpdateMemoryWithOptions accepted %q, a source_ref the decision lookup reads as a link", ref)
		}
	}
	// A reference that only MENTIONS the word, in a token the reader does not take
	// as a link, is an ordinary reference and stays accepted.
	for _, ref := range []string{"docs/decision:abc.md", "runbook.md, decision:abc"} {
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "ordinary ref "+ref, "manual", 0.5, nil,
			UpsertOptions{Provenance: Provenance{SourceRef: ref}}); err != nil {
			t.Errorf("UpsertWithOptions refused %q, which the decision lookup does not read as a link: %v", ref, err)
		}
	}
}

// TestImportDecisionRefusesTheLinkSeparatorInItsID: a decision id is the tail of
// its companion's `decision:<id>` link, and the lookup splits a source_ref on
// decisionRefSeparator. RecordDecision mints hex ids, but an import writes the
// artifact's id verbatim, so an id holding the separator would split its own link
// into tokens that name no decision: the companion is never found, and a
// supersede answers clean while the reversed decision keeps ranking. The id is
// refused at the import check, so the premise holds on every writer.
func TestImportDecisionRefusesTheLinkSeparatorInItsID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/sep", "sep"); err != nil {
		t.Fatal(err)
	}
	bad := Decision{ID: "ab" + decisionRefSeparator + "cd", ProjectID: testProject, Title: "t", Decision: "d", Rationale: "r", Status: "active"}
	if err := CheckImportedDecision(bad); err == nil {
		t.Errorf("CheckImportedDecision accepted id %q, which splits its own companion link", bad.ID)
	}
	if _, err := s.ImportDecision(ctx, bad, true); err == nil {
		t.Errorf("ImportDecision imported id %q, which splits its own companion link", bad.ID)
	}
	good := bad
	good.ID = "abcd"
	if _, err := s.ImportDecision(ctx, good, true); err != nil {
		t.Errorf("ImportDecision refused an ordinary id: %v", err)
	}
}

// TestSupersedeDecisionRetiresTheSoleCompanionBesideASharedSuccessor: a row
// standing for more than one decision is reported as shared and left live, and
// it does not turn the decision's own single-link companion into an ambiguous
// claimant — that companion states only the reversed decision, so it is retired
// as it would be with no shared row beside it.
func TestSupersedeDecisionRetiresTheSoleCompanionBesideASharedSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	aDec, aMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "use Postgres", "already deployed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bDec, _, _, err := s.RecordDecision(ctx, testProject, "Queue retries", "max 5", "rate limit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dDec, _, _, err := s.RecordDecision(ctx, testProject, "Queue backend v2", "use SQS", "managed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var shared string
	if err := s.db.QueryRow(`INSERT INTO memories (project_id, category, content, source, importance, source_ref)
		VALUES (?, 'decision', 'Postgres queue, 5 retries', 'reflection', 0.9, ?) RETURNING id`,
		testProject, decisionCompanionRef(aDec)+decisionRefSeparator+decisionCompanionRef(bDec)).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, aDec, dDec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != aMem {
		t.Errorf("retired = %v, want the decision's own companion %s", got.Retired, aMem)
	}
	if len(got.Shared) != 1 || got.Shared[0] != shared {
		t.Errorf("shared = %v, want %s", got.Shared, shared)
	}
	if len(got.Ambiguous) != 0 {
		t.Errorf("ambiguous = %v, want none", got.Ambiguous)
	}
	if decisionMemoryResolved(t, s, shared) {
		t.Error("the shared successor was withheld; it still states the other decision")
	}
}

// successorSourceRef reads the stored source_ref of the one memory holding content.
func successorSourceRef(t *testing.T, s *Store, content string) (id, ref string) {
	t.Helper()
	id = onlyMemoryWithContent(t, s, content)
	if err := s.db.QueryRow(`SELECT COALESCE(source_ref, '') FROM memories WHERE id = ?`, id).Scan(&ref); err != nil {
		t.Fatalf("read successor source_ref: %v", err)
	}
	return id, ref
}

// TestMergeKeepsALoneDecisionLinkBehindANonDecisionSource: a merge whose FIRST
// source carries an ordinary reference and whose second is a decision companion
// stands in for that decision. First-source-wins kept only the path, so the
// decision lost its companion: superseding it found nothing and answered clean
// while the merged text kept stating the reversed decision. The link EXTENDS the
// inherited reference rather than replacing it, so both survive.
func TestMergeKeepsALoneDecisionLinkBehindANonDecisionSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	plain, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "the queue runbook lives in runbook.md", "mcp", 0.5, nil,
		UpsertOptions{Provenance: Provenance{SourceRef: "runbook.md"}})
	if err != nil {
		t.Fatal(err)
	}
	aDec, aMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "use Postgres", "already deployed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dDec, dMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend v2", "use SQS", "managed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var held string
	if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, dMem).Scan(&held); err != nil {
		t.Fatal(err)
	}
	const merged = "Queue: Postgres, runbook in runbook.md"
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: merged, Importance: 0.8, ReplacesIDs: []string{plain, aMem}},
		{Category: "decision", Content: held, Importance: 0.9},
	}, "", nil); err != nil {
		t.Fatal(err)
	}
	successor, ref := successorSourceRef(t, s, merged)
	if !sourceRefNamesDecision(ref, aDec) {
		t.Fatalf("the merge successor's source_ref %q dropped the decision link", ref)
	}
	if !strings.HasPrefix(ref, "runbook.md") {
		t.Errorf("the merge successor's source_ref %q dropped the first source's reference", ref)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, aDec, dDec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != successor {
		t.Errorf("retired = %v, want the successor %s that stands for the decision", got.Retired, successor)
	}
}

// TestMergeDecisionLinksExtendTheEmissionsSourceRef: a value the emission states
// wins over the sources', and the decision links a merge inherits are appended
// to it rather than written over it.
func TestMergeDecisionLinksExtendTheEmissionsSourceRef(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	aDec, aMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "use Postgres", "already deployed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bDec, bMem, _, err := s.RecordDecision(ctx, testProject, "Queue retries", "max 5", "rate limit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const merged = "Queue: Postgres, 5 retries"
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "decision", Content: merged, Importance: 0.9, SourceRef: "docs/new.md", ReplacesIDs: []string{aMem, bMem}},
	}, "", nil); err != nil {
		t.Fatal(err)
	}
	_, ref := successorSourceRef(t, s, merged)
	want := "docs/new.md" + decisionRefSeparator + decisionCompanionRef(aDec) + decisionRefSeparator + decisionCompanionRef(bDec)
	if ref != want {
		t.Errorf("source_ref = %q, want %q (the emission's value, extended by both links)", ref, want)
	}
}

// TestMergeDecisionLinksStayWithinTheSourceRefBound: the joined value is written
// by no caller, so the bound is enforced here. A base reference that no longer
// fits beside the links gives way to them — a lost link is the silent supersede
// miss — and a merge whose links ALONE would not fit is not written at all: its
// sources stay live, each still the companion of its decision.
func TestMergeDecisionLinksStayWithinTheSourceRefBound(t *testing.T) {
	ctx := context.Background()
	t.Run("a long base gives way to the links", func(t *testing.T) {
		s := testStore(t)
		long := "docs/" + strings.Repeat("x", MaxSourceRefLen-len("docs/")-10)
		plain, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "a long-referenced note", "mcp", 0.5, nil,
			UpsertOptions{Provenance: Provenance{SourceRef: long}})
		if err != nil {
			t.Fatal(err)
		}
		aDec, aMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "use Postgres", "already deployed", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		const merged = "Queue: Postgres, see the long note"
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: merged, Importance: 0.8, ReplacesIDs: []string{plain, aMem}},
		}, "", nil); err != nil {
			t.Fatal(err)
		}
		_, ref := successorSourceRef(t, s, merged)
		if len(ref) > MaxSourceRefLen {
			t.Errorf("source_ref is %d bytes, over the %d-byte bound", len(ref), MaxSourceRefLen)
		}
		if !sourceRefNamesDecision(ref, aDec) {
			t.Errorf("source_ref %q dropped the decision link to stay in bounds", ref)
		}
	})
	t.Run("links that cannot fit keep the sources", func(t *testing.T) {
		s := testStore(t)
		var mems []string
		decs := map[string]string{}
		for i := 0; i < 13; i++ {
			dec, mem, _, err := s.RecordDecision(ctx, testProject, fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			mems = append(mems, mem)
			decs[mem] = dec
		}
		// 13 links of 41 bytes joined by commas are 545 bytes, over the bound.
		_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "decision", Content: "Thirteen decisions in one", Importance: 0.9, ReplacesIDs: mems},
		}, "", nil)
		if err != nil {
			t.Fatalf("ReplaceNonManual failed the whole pass over one oversized merge: %v", err)
		}
		if dropped != 1 {
			t.Errorf("dropped = %d, want the oversized merge dropped", dropped)
		}
		for _, mem := range mems {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NULL AND source_ref = ?`,
				mem, decisionCompanionRef(decs[mem])).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("companion %s was not kept live with its link", mem)
			}
		}
	})
}

// TestMergeReusingADecisionCompanionKeepsTheOtherDecisionsLink: when the merge's
// text is byte-identical to one source, that row is reused in place and the
// others are deleted into it. The reused row's own reference wins, but a decision
// link the absorbed sources carried must be added to it — otherwise the absorbed
// decision has no companion left, and superseding it answers clean while the
// merged row keeps stating it.
func TestMergeReusingADecisionCompanionKeepsTheOtherDecisionsLink(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	aDec, aMem, _, err := s.RecordDecision(ctx, testProject, "Queue backend", "use Postgres", "already deployed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bDec, bMem, _, err := s.RecordDecision(ctx, testProject, "Queue retries", "max 5", "rate limit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dDec, dMem, _, err := s.RecordDecision(ctx, testProject, "Queue retries v2", "max 3", "lower limit", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := func(id string) string {
		var c string
		if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	aText, dText := content(aMem), content(dMem)
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "decision", Content: aText, Importance: 0.9, ReplacesIDs: []string{aMem, bMem}},
		{Category: "decision", Content: dText, Importance: 0.9},
	}, "", nil); err != nil {
		t.Fatal(err)
	}
	reused, ref := successorSourceRef(t, s, aText)
	if reused != aMem {
		t.Fatalf("the merge did not reuse the identical row: got %s, want %s", reused, aMem)
	}
	if !sourceRefNamesDecision(ref, aDec) || !sourceRefNamesDecision(ref, bDec) {
		t.Fatalf("the reused row's source_ref %q does not name both decisions", ref)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, bDec, dDec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shared) != 1 || got.Shared[0] != reused {
		t.Errorf("superseding the absorbed decision: shared = %v, want the reused row %s", got.Shared, reused)
	}
}
