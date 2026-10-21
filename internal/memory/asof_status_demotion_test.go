package memory

import (
	"context"
	"testing"
	"time"
)

// The status demotion the current path applies (demoteStatus) applies to a
// historical read too. A resolved row and a `_global` row in a project-scoped
// search are demoted rather than excluded, and the inputs the factor needs are
// the VERSION's: `AsOfRow` carries `ResolvedAt` and `ProjectID` from the version
// row, so a read can say what a row's status was at the instant asked about
// rather than what it is now.
//
// Without the factor a historical read ranked a high-importance resolved row (or
// a high-importance `_global` row) above a lower-importance live project row —
// the opposite of what the same search over the present does, from the same
// store, with the same query.

// asOfStatusRow saves one memory with an explicit importance, so the fixture
// controls the standing a raw ranking gives it: the higher-importance row is
// the one the demotion has to sink.
func asOfStatusRow(t *testing.T, s *Store, projectID, content string, importance float32) string {
	t.Helper()
	id, err := s.Create(context.Background(), projectID, Memory{
		Category:   "fact",
		Content:    content,
		Source:     "manual",
		Importance: importance,
	})
	if err != nil {
		t.Fatalf("Create(%s, %q): %v", projectID, content, err)
	}
	return id
}

// currentSearch runs the same project-scoped keyword search over the present:
// the same request the historical read builds, with the instant left as the
// clock rather than bound as the instant the rows are read at.
func currentSearch(t *testing.T, s *Store, projectID, query string, now time.Time) *CandidateSet {
	t.Helper()
	req := asOfRequest(projectID, query, now)
	req.AsOf = nil
	set, err := s.Candidates(context.Background(), req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	return set
}

// requireLiveFirst asserts that the live row leads the answer and that the
// demoted row is still in it: a demotion ranks a row down, never filters it
// out.
func requireLiveFirst(t *testing.T, name string, set *CandidateSet, liveID, demotedID string) {
	t.Helper()
	ids := candidateIDs(t, set)
	if len(ids) == 0 || ids[0] != liveID {
		t.Errorf("%s returns %v, want the live row %s first: a demoted row ranks below the project's live memories",
			name, ids, liveID)
	}
	for _, id := range ids {
		if id == demotedID {
			return
		}
	}
	t.Errorf("%s returns %v, want the demoted row %s in it: the demotion ranks a row down, it never filters it out",
		name, ids, demotedID)
}

// TestCandidatesAsOfDemotesResolvedBelowLive: a resolved row must not lead a
// historical read over a live project row, whatever the two rows' importance.
func TestCandidatesAsOfDemotesResolvedBelowLive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	resolvedID := asOfStatusRow(t, s, testProject, "zebra resolved checkpoint alpha", 0.9)
	liveID := asOfStatusRow(t, s, testProject, "zebra live checkpoint beta", 0.3)
	if n, err := s.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}
	// The row holds two versions — the save and the resolve — and the read
	// chooses between them by their recorded instants, so both are stamped:
	// the newest version at the instant below is the resolve, and the row reads
	// as resolved then.
	stampHistory(t, s, resolvedID, asOfStampSave, asOfStampRewrite)
	stampHistory(t, s, liveID, asOfStampSave)

	at := asOfAt(t, asOfStampFarFuture)
	requireLiveFirst(t, "the current read", currentSearch(t, s, testProject, "zebra", at), liveID, resolvedID)
	requireLiveFirst(t, "the as_of read", asOfSearch(t, s, testProject, "zebra", asOfStampFarFuture), liveID, resolvedID)
}

// TestCandidatesAsOfDemotesGlobalBelowProject: the same rule for a `_global`
// row in a project-scoped historical read. A shared row enters every project's
// legs, so without the demotion it pads a project's results ahead of the
// project's own memories.
func TestCandidatesAsOfDemotesGlobalBelowProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	globalID := asOfStatusRow(t, s, GlobalProjectID, "zebra global checkpoint alpha", 0.9)
	liveID := asOfStatusRow(t, s, testProject, "zebra project checkpoint beta", 0.3)
	stampHistory(t, s, globalID, asOfStampSave)
	stampHistory(t, s, liveID, asOfStampSave)

	at := asOfAt(t, asOfStampFarFuture)
	requireLiveFirst(t, "the current read", currentSearch(t, s, testProject, "zebra", at), liveID, globalID)
	requireLiveFirst(t, "the as_of read", asOfSearch(t, s, testProject, "zebra", asOfStampFarFuture), liveID, globalID)
}

// TestCandidatesAsOfCrossProjectReadDoesNotDemoteGlobalRows: the `_global`
// factor fires when a specific project is searched, where the shared row has
// the project's own memories to lose to. A cross-project read has no
// "elsewhere", so a `_global` row keeps its raw standing there — the same rule
// the current path follows.
func TestCandidatesAsOfCrossProjectReadDoesNotDemoteGlobalRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	globalID := asOfStatusRow(t, s, GlobalProjectID, "zebra global checkpoint alpha", 0.9)
	asOfStatusRow(t, s, testProject, "zebra project checkpoint beta", 0.3)
	stampHistory(t, s, globalID, asOfStampSave)

	req := asOfRequest("", "zebra", asOfAt(t, asOfStampFarFuture))
	req.Mode = AllProjects
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := candidateIDs(t, set)
	if len(ids) == 0 || ids[0] != globalID {
		t.Errorf("the cross-project as_of read returns %v, want the _global row %s first: the project-scoped "+
			"factor must not fire when no project is being searched", ids, globalID)
	}
}

// TestCandidatesAsOfDemotionOwnsTheWindowCut: the factor decides membership as
// well as order. A live project row the demoted row was keeping out of the
// window has to take that slot — the same rule the current path's cut follows,
// where the demotion runs before the cut rather than after it.
func TestCandidatesAsOfDemotionOwnsTheWindowCut(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	resolvedID := asOfStatusRow(t, s, testProject, "zebra resolved checkpoint alpha", 0.9)
	liveID := asOfStatusRow(t, s, testProject, "zebra live checkpoint beta", 0.3)
	if n, err := s.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}
	stampHistory(t, s, resolvedID, asOfStampSave, asOfStampRewrite)
	stampHistory(t, s, liveID, asOfStampSave)

	// A window of one: only the row the demotion leaves standing can take it.
	req := asOfRequest(testProject, "zebra", asOfAt(t, asOfStampFarFuture))
	req.Fetch.Limit = 1
	set, err := s.Candidates(context.Background(), req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := candidateIDs(t, set)
	if len(ids) == 0 || ids[0] != liveID {
		t.Errorf("the as_of window of one holds %v, want the live row %s: the demotion owns the cut, "+
			"not just the order of what survived it", ids, liveID)
	}
}
