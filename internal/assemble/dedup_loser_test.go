package assemble

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// droppedLoser is a near-duplicate loser the retriever removed: a candidate that
// is NOT in the set's Rows, with the ids it lost to.
func droppedLoser(c memory.Candidate, lostTo ...string) memory.DroppedLoser {
	return memory.DroppedLoser{Candidate: c, LostTo: lostTo}
}

func sessionStartWithLoser() (*fakeRetriever, Request) {
	set := passiveSet(globalCandidate("g1", 0.9), globalCandidate("g2", 0.5))
	set.DroppedLosers = []memory.DroppedLoser{droppedLoser(globalCandidate("gdup", 0.8), "g1")}
	req := passiveRequest()
	req.Budget.Slices = []Slice{{
		Bucket: memory.GlobalProjectID, MaxItems: 8, OverFetch: 16,
		Order: "pinned_importance_updated", DemotionThreshold: 0.85, DropDemotedLosers: true,
	}}
	return &fakeRetriever{set: set}, req
}

// TestDroppedNearDuplicateLoserIsInTheTrace (#894): a loser the retriever removed
// never enters CandidateSet.Rows, so before this nothing in the trace named it.
// Stage 6 files it as a drop with its reason and the row it lost to.
func TestDroppedNearDuplicateLoserIsInTheTrace(t *testing.T) {
	f, req := sessionStartWithLoser()
	res := run(t, f, req)

	var got *Decision
	for i, d := range res.Trace.Decisions {
		if d.ID == "gdup" {
			got = &res.Trace.Decisions[i]
		}
	}
	if got == nil {
		t.Fatalf("the removed loser is absent from Trace.Decisions: %+v", res.Trace.Decisions)
	}
	if got.Stage != stageDedup || got.Reason != reasonNearDuplicate || got.Kept || got.ProjectID != memory.GlobalProjectID {
		t.Errorf("decision = %+v, want a dropped %s/%s decision for the _global row", *got, stageDedup, reasonNearDuplicate)
	}
	if !reflect.DeepEqual(got.Against, []string{"g1"}) {
		t.Errorf("Against = %v, want [g1]", got.Against)
	}
	if got.Before != 0.8 {
		t.Errorf("Before = %v, want the loser's score 0.8", got.Before)
	}
	var st StageTrace
	for _, s := range res.Trace.Stages {
		if s.Stage == stageDedup {
			st = s
		}
	}
	if !reflect.DeepEqual(st.DroppedIDs, []string{"gdup"}) || st.In != 3 || st.Out != 2 {
		t.Errorf("dedup stage = in %d out %d dropped %v, want 3 -> 2 dropping gdup", st.In, st.Out, st.DroppedIDs)
	}
	// Which rows are shown is unchanged: the loser was never an answer row.
	if ids := itemIDs(res.Items); !reflect.DeepEqual(ids, []string{"g1", "g2"}) {
		t.Errorf("items = %v, want [g1 g2]", ids)
	}
}

// TestDroppedNearDuplicateLoserIsInTheRetrievalRecord: the record is built from
// the trace's decisions, so the removed row is counted as a dropped verdict at
// the dedup stage.
func TestDroppedNearDuplicateLoserIsInTheRetrievalRecord(t *testing.T) {
	f, req := sessionStartWithLoser()
	sink := &recordingSink{}
	req.Record = sink
	run(t, f, req)

	var found bool
	for _, v := range sink.last(t).Verdicts {
		if v.ID == "gdup" {
			found = true
			if v.Kept || v.Stage != stageDedup || v.Reason != reasonNearDuplicate {
				t.Errorf("verdict = %+v, want dropped at dedup with reason near_duplicate", v)
			}
		}
	}
	if !found {
		t.Errorf("the retrieval record never mentions the removed loser: %+v", sink.last(t).Verdicts)
	}
}

// TestDedupedTallyComesFromTheTrace: the session-start header's Deduped count is
// the trace's own stage 6 count, not a residual guessed from the store's eligible
// count. CountedAgainst must not add a second, independent count on top.
func TestDedupedTallyComesFromTheTrace(t *testing.T) {
	f, req := sessionStartWithLoser()
	res := run(t, f, req)
	tally := CountsFor(res.Trace, memory.GlobalProjectID, 2)
	if tally.Deduped != 1 || tally.Withheld != 0 || tally.RankedOut != 0 {
		t.Fatalf("tally = %+v, want exactly one Deduped and nothing withheld or ranked out", tally)
	}
	// The store holds 3 eligible rows and the window fetched all of them: the
	// trace already accounts for every one, so nothing more is attributed.
	got := tally.CountedAgainst(3, 0, 16)
	if got.Deduped != 1 || got.Total() != 3 {
		t.Errorf("after CountedAgainst: %+v, want Deduped 1 and Total 3", got)
	}
}

// queryCollapseStore seeds the REAL store the query-mode collapse tests read
// (#926): two near-duplicate rows and one distinct row, all three matching the
// query, with the pair linked the way the linker would link it.
//
// The pair is kept lexically distinct — below the save-time 0.5 Jaccard bar —
// so the single CreateLink edge below is the only near-duplicate edge in the
// store, and the distinct row is far longer than the pair, so bm25 ranks it
// below them: the limit-2 window therefore holds the PAIR, which is what makes
// the collapse observable. A fixture where the distinct row ranked into the
// window would pass with the losers never removed at all.
func queryCollapseStore(t *testing.T) (s *memory.Store, dupA, dupB, distinct string) {
	t.Helper()
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// A nil logger is honoured as silence, and this store has nothing to report.
	s = memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mk := func(content string) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	dupA = mk("database configuration for the replica refresh runs hourly")
	dupB = mk("database configuration of the standby snapshot runs nightly")
	distinct = mk("database configuration review for the staging cluster happens each quarter with the platform team, alongside the failover drill, the retention policy refresh and the backup restoration rehearsal")
	if err := s.CreateLink(ctx, dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(related): %v", err)
	}
	return s, dupA, dupB, distinct
}

// collapseRequest is baseRequest at the store's own clock, so the rows the
// fixture just created are fresh against it, with explain on: the projection is
// read alongside the answer, from the same run.
func collapseRequest() Request {
	req := withExplain(baseRequest())
	req.Now = time.Now().UTC()
	return req
}

func hasID(ids []string, id string) bool {
	for _, s := range ids {
		if s == id {
			return true
		}
	}
	return false
}

// TestQueryModeCollapsesNearDuplicatesInTheWindow (#926): at a limit of 2, a
// window holding a near-duplicate pair and nothing else must return the
// representative AND the row the loser's slot was spent on — two copies of one
// note used to fill both slots. The representative keeps its rank, and the
// loser is reported through the three surfaces stage 6 feeds from one decision:
// the answer's absence, the trace's decision, and explain's projection of it.
func TestQueryModeCollapsesNearDuplicatesInTheWindow(t *testing.T) {
	s, dupA, dupB, distinct := queryCollapseStore(t)
	res := run(t, s, collapseRequest())

	ids := itemIDs(res.Items)
	if len(ids) != 2 {
		t.Fatalf("items = %v, want exactly 2 rows at a limit of 2", ids)
	}
	if !hasID(ids, distinct) {
		t.Errorf("items = %v, want the distinct row admitted to the slot the loser freed", ids)
	}
	winner, loser := "", ""
	for _, id := range []string{dupA, dupB} {
		if hasID(ids, id) {
			winner = id
		} else {
			loser = id
		}
	}
	if winner == "" || loser == "" {
		t.Fatalf("items = %v, want exactly one member of the near-duplicate pair (%s, %s)", ids, dupA, dupB)
	}
	if ids[0] != winner {
		t.Errorf("first item = %s, want the representative %s keeping the rank it ranked at", ids[0], winner)
	}

	// The trace names the loser, the decision it was dropped by, and the row it
	// lost to — the one decision stage 6 files for every surface below.
	var got *Decision
	for i, d := range res.Trace.Decisions {
		if d.ID == loser {
			got = &res.Trace.Decisions[i]
		}
	}
	if got == nil {
		t.Fatalf("the trace never names the removed loser %s: %+v", loser, res.Trace.Decisions)
	}
	if got.Stage != stageDedup || got.Reason != reasonNearDuplicate || got.Kept {
		t.Errorf("decision = %+v, want a dropped %s/%s decision", *got, stageDedup, reasonNearDuplicate)
	}
	if !reflect.DeepEqual(got.Against, []string{winner}) {
		t.Errorf("Against = %v, want [%s] — the representative the loser was removed for", got.Against, winner)
	}
	var st StageTrace
	for _, stage := range res.Trace.Stages {
		if stage.Stage == stageDedup {
			st = stage
		}
	}
	if !hasID(st.DroppedIDs, loser) {
		t.Errorf("dedup stage dropped %v, want %s among them", st.DroppedIDs, loser)
	}

	// Explain is a projection of that same trace: the loser is a candidate that
	// was NOT included, with the stage's own reason and the ids it lost to, and
	// the representative it lost to is included.
	row := explainRowByID(t, res.Explain, loser)
	if row.Included {
		t.Error("the removed loser is marked included")
	}
	if !reflect.DeepEqual(row.NearDuplicateOf, []string{winner}) {
		t.Errorf("near_duplicate_of = %v, want [%s]", row.NearDuplicateOf, winner)
	}
	if row.Reason == "" || row.Reason == "not in the answer, and no stage recorded why" {
		t.Errorf("reason = %q, want the stage 6 verdict", row.Reason)
	}
	if !explainRowByID(t, res.Explain, winner).Included {
		t.Error("the winner is not included")
	}
}

// TestQueryModeCollapseIsInTheRetrievalRecord: the record is the trace
// projected, so the removed loser is a dropped verdict at the dedup stage and
// the representative and the distinct row are kept ones — the record must not
// report the loser as returned, or as never seen.
func TestQueryModeCollapseIsInTheRetrievalRecord(t *testing.T) {
	s, dupA, dupB, distinct := queryCollapseStore(t)
	req := baseRequest()
	req.Now = time.Now().UTC()
	sink := &recordingSink{}
	req.Record = sink
	res := run(t, s, req)

	if ids := itemIDs(res.Items); !hasID(ids, distinct) {
		t.Fatalf("items = %v, want the distinct row the fixture was built around", ids)
	}
	rec := sink.last(t)
	droppedID, winnerID, dropStage, dropReason := "", "", "", ""
	kept := map[string]bool{}
	for _, v := range rec.Verdicts {
		if v.Kept {
			kept[v.ID] = true
		}
		if v.ID == dupA || v.ID == dupB {
			if v.Kept {
				winnerID = v.ID
			} else {
				droppedID, dropStage, dropReason = v.ID, v.Stage, v.Reason
			}
		}
	}
	if droppedID == "" || winnerID == "" || droppedID == winnerID {
		t.Fatalf("the record holds one dropped and one kept verdict for the pair (%s, %s); got %+v", dupA, dupB, rec.Verdicts)
	}
	if dropStage != stageDedup || dropReason != reasonNearDuplicate {
		t.Errorf("the loser's verdict is %s/%s, want %s/%s", dropStage, dropReason, stageDedup, reasonNearDuplicate)
	}
	if !kept[winnerID] {
		t.Errorf("the representative %s is not recorded as kept: %+v", winnerID, rec.Verdicts)
	}
	if !kept[distinct] {
		t.Errorf("the row admitted to the loser's slot is not recorded as kept: %+v", rec.Verdicts)
	}
	if kept[droppedID] {
		t.Errorf("the removed loser %s is recorded as kept as well as dropped", droppedID)
	}
}

// queryDedupNote pulls stage 6's sentence out of a QUERY-mode run's notes, so
// the assertion is on that sentence rather than on a neighbour sharing a word.
// It matches on "near-duplicate" rather than on the passive sentence's own
// opening, because the query wording is a different sentence from the passive
// one and the two are what this test tells apart.
func queryDedupNote(t *testing.T, res Result) string {
	t.Helper()
	for _, n := range res.Notes {
		if strings.Contains(n, "near-duplicate") {
			return n
		}
	}
	t.Fatalf("no near-duplicate note in %v", res.Notes)
	return ""
}

// TestQueryModeDedupNoteSaysTheLosersAreRemoved (#926): stage 6's note is
// derived from the request, and for a query it used to say "no source policy
// drops losers on this surface yet" — true of the policies (a query reaches
// none) and false of the surface, whose retriever now removes the loser. The
// sentence states the MODE's behaviour rather than a per-row outcome, so the
// control below runs a window with no near-duplicate edge at all and must get
// the same sentence: otherwise this test would be measuring the window, not the
// wording.
func TestQueryModeDedupNoteSaysTheLosersAreRemoved(t *testing.T) {
	s, _, _, _ := queryCollapseStore(t)
	note := queryDedupNote(t, run(t, s, collapseRequest()))
	if !strings.Contains(note, "losers are REMOVED by the retriever") {
		t.Errorf("a query-mode note must say the retriever removes the loser: %q", note)
	}
	if !strings.Contains(note, "one row of each pair") {
		t.Errorf("the note must say what the removal leaves the window holding: %q", note)
	}
	if strings.Contains(note, "no source policy drops losers") {
		t.Errorf("the note denies removals on the surface that just performed one: %q", note)
	}

	// Control: the same request shape with a window holding no near-duplicate
	// edge says the same thing, so the sentence is the mode's and not a claim
	// about what this particular window contained. Without it the assertions
	// above could be satisfied by a note derived from the removal's outcome.
	plain := run(t, &fakeRetriever{set: setOf(candidate("c1", "proj", "fact", "x", 0.9))}, baseRequest())
	plainNote := queryDedupNote(t, plain)
	if !strings.Contains(plainNote, "losers are REMOVED by the retriever") {
		t.Errorf("the wording must not depend on whether this window held a pair: %q", plainNote)
	}

	// The passive wording is untouched: a passive request without a dropping
	// policy still denies removals, so the query sentence is not simply the one
	// every request now prints.
	passiveNote := passiveDedupNote(t, run(t, &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}, passiveRequest()))
	if strings.Contains(passiveNote, "REMOVED by the retriever") {
		t.Errorf("the passive sentence was rewritten by the query-mode change: %q", passiveNote)
	}
}
