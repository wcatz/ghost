package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

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

// A reflect merge can absorb a decision companion into an ordinary agent save
// (#1283): the emission's text byte-matches the SAVE, so takeReusableRow reuses
func TestSupersedeDecisionRetiresACompanionAbsorbedIntoAnAgentSave(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)

	var companionText string
	if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, oldMem).Scan(&companionText); err != nil {
		t.Fatalf("read the companion's text: %v", err)
	}
	// The agent save: the same text under its own category and source, and fields
	// a verbatim re-emission reproduces, so the merge takes the branch that writes
	// nothing (#727) — the strictest path, and the one the leak is on.
	save, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", companionText, "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("save the agent copy: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: companionText, Importance: 0.5,
		ReplacesIDs: []string{save, oldMem},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if !rowExists(t, s, save) || rowExists(t, s, oldMem) {
		t.Fatalf("the merge did not reuse the save and take the companion (save live=%v, companion live=%v)",
			rowExists(t, s, save), rowExists(t, s, oldMem))
	}
	var source, ref string
	if err := s.db.QueryRow(`SELECT source, COALESCE(source_ref, '') FROM memories WHERE id = ?`, save).Scan(&source, &ref); err != nil {
		t.Fatalf("read the reused row: %v", err)
	}
	if !sourceRefNamesDecision(ref, oldDec) {
		t.Fatalf("the reused row %s carries source_ref %q, which does not name decision %s: the fixture did not reproduce the merge",
			save, ref, oldDec)
	}

	got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 1 || got.Retired[0] != save {
		t.Errorf("report = %+v, want the reused row %s (source %q) retired", got, save, source)
	}
	if !decisionMemoryResolved(t, s, save) {
		t.Error("the reused row is still live: search and session start keep answering with the reversed decision")
	}
}

// The read side must hold on its own (#1283): even when the write side did not
func TestSupersedeDecisionReportsALinkedRowTheTrustArmsReject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldDec, newDec, oldMem, _ := decisionSupersedeFixture(t, s, ctx)
	if err := s.Delete(ctx, oldMem); err != nil {
		t.Fatalf("delete the companion: %v", err)
	}
	// The shape the write side used to leave behind: an ordinary agent save
	// holding the companion's link with neither the decision category nor a
	// trusted source.
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "the queue backend, restated by an agent", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("create the linked row: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, decisionCompanionRef(oldDec), id); err != nil {
		t.Fatalf("link the row: %v", err)
	}

	got, err := s.SupersedeDecisionReport(ctx, testProject, oldDec, newDec)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 0 {
		t.Errorf("a row whose link the trust arms reject was retired: %+v", got)
	}
	if len(got.Declined) != 1 || got.Declined[0] != id {
		t.Errorf("report = %+v, want the linked row %s declined so the supersede is not answered clean", got, id)
	}
	if decisionMemoryResolved(t, s, id) {
		t.Error("the linked row was retired even though its link is not trusted")
	}
}

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

func TestTheReservedRefRefusalBoundsAnOversizedValue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	short := "decision:someid123"
	_, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "short reserved", "manual", 0.5, nil,
		UpsertOptions{Provenance: Provenance{SourceRef: short}})
	if err == nil {
		t.Fatal("UpsertWithOptions accepted a short reserved source_ref")
	}
	if !strings.Contains(err.Error(), `"`+short+`"`) {
		t.Errorf("a short reserved source_ref refusal lost the value: %v", err)
	}

	big := "decision:abc" + strings.Repeat("x", 5000)
	_, _, _, err = s.UpsertWithOptions(ctx, testProject, "fact", "oversized reserved", "manual", 0.5, nil,
		UpsertOptions{Provenance: Provenance{SourceRef: big}})
	if err == nil {
		t.Fatal("UpsertWithOptions accepted an oversized reserved source_ref")
	}
	if len(err.Error()) > 1024 || strings.Contains(err.Error(), strings.Repeat("x", 100)) || !strings.Contains(err.Error(), "source_ref withheld") {
		t.Errorf("the oversized reserved refusal repeated the value: len %d, %.200s", len(err.Error()), err)
	}

	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "plain", Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{Provenance: Provenance{SourceRef: big}}); err == nil {
		t.Error("UpdateMemoryWithOptions accepted an oversized reserved source_ref")
	} else if len(err.Error()) > 1024 || strings.Contains(err.Error(), strings.Repeat("x", 100)) || !strings.Contains(err.Error(), "source_ref withheld") {
		t.Errorf("the update refusal repeated the value: len %d, %.200s", len(err.Error()), err)
	}
}

func TestTheReservedRefRefusalSaysTheLinkIsGhostsToManage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "plain", Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	err = s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{
		Provenance: Provenance{SourceRef: "docs/runbook.md,decision:abc"},
	})
	if err == nil {
		t.Fatal("UpdateMemoryWithOptions accepted a decision link behind an ordinary path")
	}
	for _, want := range []string{"docs/runbook.md,decision:abc", "omit source_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// TestSupersedeDecisionRefusesToDropALiveSuccessor is #1144 at the store: A was
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
	if _, _, err := s.ImportDecision(ctx, bad, true); err == nil {
		t.Errorf("ImportDecision imported id %q, which splits its own companion link", bad.ID)
	}
	good := bad
	good.ID = "abcd"
	if _, _, err := s.ImportDecision(ctx, good, true); err != nil {
		t.Errorf("ImportDecision refused an ordinary id: %v", err)
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
		if dropped.Overflow != 1 {
			t.Errorf("dropped.Overflow = %d, want the oversized merge dropped", dropped.Overflow)
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

// TestAnOverflowingMergeIsReportedAsItsOwnDropReason is #1228 at the store: an
func TestAnOverflowingMergeIsReportedAsItsOwnDropReason(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var mems []string
	for i := 0; i < 13; i++ {
		_, mem, _, err := s.RecordDecision(ctx, testProject, fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		mems = append(mems, mem)
	}
	// A row the corpus guard would refuse, dropped beside the overflowing merge,
	// so the two reasons cannot be conflated by a count that happens to be 1.
	other, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "an untouched note", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	setAgesDaysAgo(t, s, other, 2)

	fed := corpusRows(t, s, append(append([]string{}, mems...), other)...)
	emitted := []Memory{
		{Category: "decision", Content: "Thirteen decisions in one", Importance: 0.9, ReplacesIDs: mems},
		{Category: "fact", Content: "an untouched note", Importance: 0.5},
	}
	if err := s.Delete(ctx, other); err != nil {
		t.Fatal(err)
	}

	preserved, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, emitted, "", fed)
	if err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Overflow != 1 {
		t.Errorf("Overflow = %d, want 1: the merge whose links do not fit is its own reason", dropped.Overflow)
	}
	if dropped.Changed != 1 {
		t.Errorf("Changed = %d, want 1: the keep claiming the deleted row is still the corpus guard's", dropped.Changed)
	}
	if dropped.Total() != 2 {
		t.Errorf("Total = %d, want 2 emissions refused", dropped.Total())
	}
	if dropped.OverflowLinks != 13 {
		t.Errorf("OverflowLinks = %d, want the 13 decision links the merge stood for", dropped.OverflowLinks)
	}
	// The thirteen kept alive by the overflow drop are not reported as
	// survivors: a survivor is a row a later run still has to merge, and this
	// merge cannot be written at all. Reporting them is what held the project
	// open, re-proposing and dropping the same merge on every round.
	for _, mem := range mems {
		for _, id := range preserved {
			if id == mem {
				t.Errorf("companion %s is reported as preserved, so the caller will not record a skip fingerprint for it", mem)
			}
		}
	}
	for _, mem := range mems {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, mem).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("companion %s was not kept live by the overflow drop", mem)
		}
	}
}

// guardReaddFixture is the drop guard's shape at the store.
func guardReaddFixture(t *testing.T, s *Store, ctx context.Context) (decID, comp, fact, successor string) {
	t.Helper()
	var err error
	decID, comp, _, err = s.RecordDecision(ctx, testProject, "queue", "we use nats for the event queue", "it was cheapest", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	fact, err = s.Create(ctx, testProject, Memory{Category: "fact", Source: "mcp", Importance: 0.5,
		Content: "the event queue consumer restarts in place"})
	if err != nil {
		t.Fatalf("create the fact: %v", err)
	}
	var compText string
	if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, comp).Scan(&compText); err != nil {
		t.Fatalf("read the companion's text: %v", err)
	}
	const mergeText = "event queue consumer restarts in place; queue choice recorded"
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "decision", Content: mergeText, Importance: 0.6, Source: "reflection", ReplacesIDs: []string{comp, fact}},
		{Category: "decision", Content: compText, Importance: 0.5, Source: "reflection"},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if err := s.db.QueryRow(`SELECT id FROM memories WHERE content = ?`, mergeText).Scan(&successor); err != nil {
		t.Fatalf("read the merge successor: %v", err)
	}
	return decID, comp, fact, successor
}

// liveRowsLinking returns every live row whose source_ref names the decision.
func liveRowsLinking(t *testing.T, s *Store, decisionID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, COALESCE(source_ref, '') FROM memories WHERE resolved_at IS NULL ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read linked rows: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var id, ref string
		if err := rows.Scan(&id, &ref); err != nil {
			t.Fatalf("scan linked row: %v", err)
		}
		if sourceRefNamesDecision(ref, decisionID) {
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read linked rows: %v", err)
	}
	return out
}

func TestSupersedeDecisionReportsAGuardReaddedCompanionAndItsMergeAsAmbiguous(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	decID, comp, fact, successor := guardReaddFixture(t, s, ctx)

	if !rowExists(t, s, comp) {
		t.Fatal("the re-added companion did not keep its row: the fixture no longer reproduces the guard's reuse")
	}
	if rowExists(t, s, fact) {
		t.Fatal("the merged fact is still a row: the fixture no longer folds it")
	}
	if got := liveRowsLinking(t, s, decID); len(got) != 2 || got[0] != comp || got[1] != successor {
		t.Fatalf("live rows carrying the decision link = %v, want the companion %s and the merge %s", got, comp, successor)
	}

	newDec, _, _, err := s.RecordDecision(ctx, testProject, "queue v2", "we use kafka for the event queue", "scale", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}
	got, err := s.SupersedeDecisionReport(ctx, testProject, decID, newDec)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(got.Retired) != 0 || len(got.Ambiguous) != 2 {
		t.Errorf("report = %+v, want nothing retired and both carriers (%s, %s) ambiguous", got, comp, successor)
	}
	if decisionMemoryResolved(t, s, comp) || decisionMemoryResolved(t, s, successor) {
		t.Error("an ambiguous link retired a memory")
	}
}

func TestRepointCompanionInvalidationReportsRowsAffectedError(t *testing.T) {
	src, err := os.ReadFile("decisions.go")
	if err != nil {
		t.Fatalf("read decisions.go: %v", err)
	}
	text := string(src)
	i := strings.Index(text, "func repointRetiredCompanionsTx(")
	if i < 0 {
		t.Fatalf("func repointRetiredCompanionsTx not found")
	}
	j := strings.Index(text[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("end of func repointRetiredCompanionsTx not found")
	}
	body := blankLineComments(text[i : i+j])

	if strings.Contains(body, ", _ := res.RowsAffected") || strings.Contains(body, ", _ = res.RowsAffected") {
		t.Errorf("stale-edge invalidation ignores the RowsAffected error: it must be returned so an invalidation is never left without its history row")
	}
	if !strings.Contains(body, "n, err := res.RowsAffected()") {
		t.Errorf("stale-edge invalidation does not read the affected count into err")
	}
	if !strings.Contains(body, "return fmt.Errorf(\"invalidate stale supersedes link rows:") {
		t.Errorf("stale-edge invalidation does not return the RowsAffected error from the transaction")
	}
}
