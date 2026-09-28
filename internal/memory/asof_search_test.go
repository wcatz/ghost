package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// asOfSearch runs one historical retrieval: the request the assembler builds for
// an as_of read, with the clock the read is about. Now is set to the same
// instant on purpose — the assembler binds both — and a test that set only AsOf
// would be testing a request shape no caller produces.
func asOfSearch(t *testing.T, s *Store, projectID, query, stamp string) *CandidateSet {
	t.Helper()
	at := asOfAt(t, stamp)
	set, err := s.Candidates(context.Background(), asOfRequest(projectID, query, at))
	if err != nil {
		t.Fatalf("Candidates as_of %s: %v", stamp, err)
	}
	return set
}

// asOfRequest is the same request with the project, condition and clock filled
// in, so the two callers below differ only in the vector leg they ask for.
func asOfRequest(projectID, query string, at time.Time) CandidateRequest {
	return CandidateRequest{
		ProjectID: projectID,
		Mode:      ProjectScoped,
		Query:     query,
		Condition: CondHybrid,
		Params:    DefaultSearchParams(),
		Now:       at,
		AsOf:      &at,
		Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
	}
}

// TestCandidatesAsOfMatchesTheTextTheVersionHeld: the point of the whole feature.
// An embedding and an FTS index both describe what a memory says NOW, so a
// search for wording that has since been edited away returns nothing — and a
// benchmark replaying a past session cannot reproduce what that session was
// given. The historical read matches the version's own text, so the same query
// finds the memory before the edit and misses it after.
func TestCandidatesAsOfMatchesTheTextTheVersionHeld(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The two versions share no content term, and the queries name one term each
	// that the other version does not contain. Terms are OR'd, so a query with a
	// word in both versions would match both and the test would measure nothing.
	const before = "the vacuum job holds an exclusive lock while it rewrites the db file"
	const after = "the checkpoint interval bounds growth between two backups"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", before, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(after), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite)

	// The old wording, asked at an instant when it was what the memory said.
	old := asOfSearch(t, s, testProject, "vacuum exclusive", asOfStampSave)
	if !containsID(old, id) {
		t.Errorf("a search for the pre-edit wording at %s returns %v, want it to find %s", asOfStampSave, candidateIDs(t, old), id)
	}
	// ...and the same question after the edit, where that text exists nowhere in
	// the store except the version row the read must not match.
	oldLater := asOfSearch(t, s, testProject, "vacuum exclusive", asOfStampRewrite)
	if containsID(oldLater, id) {
		t.Errorf("a search for the pre-edit wording at %s returns %s, want no match: the memory said something else then", asOfStampRewrite, id)
	}
	// The new wording, which is what a current search finds.
	current := asOfSearch(t, s, testProject, "checkpoint bounds", asOfStampRewrite)
	if !containsID(current, id) {
		t.Errorf("a search for the post-edit wording at %s returns %v, want it to find %s", asOfStampRewrite, candidateIDs(t, current), id)
	}
	earlier := asOfSearch(t, s, testProject, "checkpoint bounds", asOfStampSave)
	if containsID(earlier, id) {
		t.Errorf("a search for the post-edit wording at %s returns %s, want no match: the memory had not been written yet", asOfStampSave, id)
	}
}

// TestCandidatesAsOfDoesNotDependOnMemoryProvenance: schema v18 added
// memory_provenance — the append-only EVIDENCE table, several rows per memory,
// answering "who or what supports this memory". A historical read must neither
// read it nor fail without it, and the only way to be sure of both is to remove
// it: every statement in the as_of path has to work against a database that has
// no such table, which is exactly what a store that predates v18 looks like to
// this read.
//
// It is also the reason the candidate carries no evidence counts. A count taken
// now describes the present — evidence records are not versioned, and none of
// them can be dated back to an instant — so a historical read that filled it in
// would be reporting present-day support for a past memory. The counts stay zero
// and the surface says so, because a zero renders as "no recorded evidence",
// which is a claim.
func TestCandidatesAsOfDoesNotDependOnMemoryProvenance(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "the evidence table records observations, not versions", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// The evidence row has to exist before the table goes, or the test would be
	// proving that a read works against a store that never had one — which is a
	// weaker claim.
	var observed int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = ?`, id).Scan(&observed); err != nil {
		t.Fatalf("count evidence rows: %v", err)
	}
	if observed == 0 {
		t.Fatal("the save recorded no evidence row, so this fixture would not prove non-dependence")
	}
	if _, err := s.db.Exec(`DROP TABLE memory_provenance`); err != nil {
		t.Fatalf("drop memory_provenance: %v", err)
	}

	// The set read, and the retrieval, both still work.
	set, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, asOfStampFarFuture))
	if err != nil {
		t.Fatalf("MemoriesAsOf against a store with no evidence table: %v", err)
	}
	if _, ok := asOfContentByID(t, set)[id]; !ok {
		t.Errorf("the set read lost the memory against a store with no evidence table: %v", asOfContentByID(t, set))
	}
	candidates := asOfSearch(t, s, testProject, "evidence observations", asOfStampFarFuture)
	if !containsID(candidates, id) {
		t.Errorf("the historical search lost the memory against a store with no evidence table: %v", candidateIDs(t, candidates))
	}
	// And the counts are absent rather than zero-because-they-were-read, which is
	// what the surface's disclosure is about.
	for _, c := range candidates.Rows {
		if c.Evidence != (EvidenceCounts{}) {
			t.Errorf("row %s carries evidence counts %+v, want none: a historical read cannot date an observation", c.ID, c.Evidence)
		}
	}
}

// TestCandidatesAsOfDropsADeletedMemoryAndItsTombstone: a delete removes the row
// and leaves a tombstone carrying the text. A historical read that ran against
// the live tables would return nothing for that memory at every instant; a
// historical read that forgot the tombstone would return it at every instant
// after the delete. Both are wrong, and the window between them is the whole
// question.
func TestCandidatesAsOfDropsADeletedMemoryAndItsTombstone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "the snapshot retention window is fourteen days", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite)

	live := asOfSearch(t, s, testProject, "snapshot retention", asOfStampSave)
	if !containsID(live, id) {
		t.Error("a search at the instant before the delete does not find the memory, want it live")
	}
	gone := asOfSearch(t, s, testProject, "snapshot retention", asOfStampRewrite)
	if containsID(gone, id) {
		t.Error("a search after the delete still finds the memory, want the tombstone to have removed it")
	}
}

// TestCandidatesAsOfReportsTheVectorLegAsNotApplicable: an embedding describes
// the text a memory holds now, so a historical content set cannot be searched
// by vector at all — the vectors for the versions the read is choosing between
// do not exist. The leg has to say it did not run rather than report a silent
// zero, because "the vector leg ran and matched nothing" and "no vector leg was
// attempted" are different answers to a caller deciding whether an empty result
// means anything.
func TestCandidatesAsOfReportsTheVectorLegAsNotApplicable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "an as_of search has no vector leg", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	stampHistory(t, s, id, asOfStampFarFuture)

	at := asOfAt(t, asOfStampFarFuture)
	set, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "vector leg",
		QueryVec:  []float32{0.1, 0.2, 0.3}, // a caller may still carry one
		Condition: CondHybrid,
		Params:    DefaultSearchParams(),
		Now:       at,
		AsOf:      &at,
		Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if !containsID(set, id) {
		t.Fatalf("the keyword leg did not find the memory: %v", candidateIDs(t, set))
	}
	vec := set.Legs["vector"]
	if vec.Applicable {
		t.Error("the vector leg reports itself applicable on a historical read, want it not applicable: there is no vector for a past wording")
	}
	if vec.Attempted || vec.Available {
		t.Errorf("the vector leg reports attempted=%v available=%v, want neither: nothing ran", vec.Attempted, vec.Available)
	}
	for _, c := range set.Rows {
		if c.VectorRank != -1 {
			t.Errorf("row %s carries vector rank %d, want -1: no vector leg retrieved it", c.ID, c.VectorRank)
		}
	}
	// The keyword leg is the leg that ran, so it must be reported as having run.
	fts := set.Legs["fts"]
	if !fts.Applicable || !fts.Attempted || !fts.Available {
		t.Errorf("the keyword leg reports applicable=%v attempted=%v available=%v, want all three", fts.Applicable, fts.Attempted, fts.Available)
	}
}

// TestCandidatesAsOfRefusesAVectorOnlyCondition: vector-only retrieval on a
// historical content set is not a degraded answer, it is an impossible one — and
// an impossible request has to be refused rather than answered with the keyword
// leg, because a caller that asked for the vector leg and got keyword results
// cannot tell which leg produced them from the rows alone.
func TestCandidatesAsOfRefusesAVectorOnlyCondition(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := asOfAt(t, asOfStampFarFuture)
	_, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "anything",
		QueryVec:  []float32{0.1, 0.2, 0.3},
		Condition: CondVectorOnly,
		Params:    DefaultSearchParams(),
		Now:       at,
		AsOf:      &at,
		Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
	})
	if err == nil {
		t.Fatal("a vector-only historical search was answered, want it refused")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("the refusal is %q, want it to name the vector leg it cannot run", err)
	}
}

// TestCandidatesAsOfDoesNotBorrowTheCurrentLinkGraph: memory_links has no
// history — an edge records when it was invalidated, not what the graph looked
// like at T — so the current supersede and near-duplicate demotions would import
// the present into a past answer. The historical demotion comes from the
// supersede/unsupersede sequence instead, and the edge read is reported as not
// applicable so no conflict claim is made in either direction.
func TestCandidatesAsOfDoesNotBorrowTheCurrentLinkGraph(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	replaced, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "the WAL checkpoint interval defaults to one thousand pages", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the replaced memory: %v", err)
	}
	replacement, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "the WAL checkpoint interval is configured per connection", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the replacement: %v", err)
	}
	if err := s.CreateLink(ctx, replacement, replaced, "supersedes", 1.0, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	stampHistory(t, s, replaced, asOfStampSave, asOfStampRewrite)
	stampHistory(t, s, replacement, asOfStampSave)

	// At the rewrite the claim is live, and a claim about a memory that is in the
	// answer demotes it — the same rule the current path applies.
	atRewrite := asOfSearch(t, s, testProject, "WAL checkpoint interval", asOfStampRewrite)
	ids := candidateIDs(t, atRewrite)
	if !containsID(atRewrite, replaced) || !containsID(atRewrite, replacement) {
		t.Fatalf("the historical search returned %v, want both memories: the claim is about one of them, not a reason to hide it", ids)
	}
	if pos(ids, replaced) < pos(ids, replacement) {
		t.Errorf("the superseded memory %s ranks first at %s, want the claim recorded at that instant to demote it", replaced, asOfStampRewrite)
	}
	if atRewrite.EdgesStatus.Status != edgesNotApplicable {
		t.Errorf("the edge status is %q, want %q: no historical read of the link graph ran", atRewrite.EdgesStatus.Status, edgesNotApplicable)
	}

	// Before the claim, neither is demoted. The two memories are otherwise
	// identical in kind, so the composite cannot separate them on its own: with
	// the claim recorded, the replacement outranks the memory it replaced, and
	// with no claim recorded, nothing says it should.
	before := asOfSearch(t, s, testProject, "WAL checkpoint interval", asOfStampSave)
	if pos(candidateIDs(t, before), replaced) < pos(candidateIDs(t, before), replacement) {
		t.Errorf("the memory ranks above its replacement at %s, want the recorded claim at %s to be the only thing that demotes it",
			asOfStampSave, asOfStampRewrite)
	}
}

// TestCandidatesAsOfReportsAPreV17MemoryAsUnrecorded: a memory with no recorded
// version cannot be matched, because the store does not know what it said at T —
// and it cannot be reported as a near-miss either. The read counts it, and the
// count is what a surface turns into the sentence telling a reader that the set
// it is looking at has a gap in it.
func TestCandidatesAsOfReportsAPreV17MemoryAsUnrecorded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	known, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a memory this build wrote about snapshots", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	legacy, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a pre-history memory that also mentions snapshots", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the legacy memory: %v", err)
	}
	stampHistory(t, s, known, asOfStampSave)
	dropHistory(t, s, legacy)

	set := asOfSearch(t, s, testProject, "snapshots", asOfStampSave)
	if !containsID(set, known) {
		t.Errorf("the recorded memory is missing from %v, want it matched", candidateIDs(t, set))
	}
	if containsID(set, legacy) {
		t.Error("a memory with no recorded version was matched, want it left out: the store cannot say what it held at that instant")
	}
	if set.Unrecorded != 1 {
		t.Errorf("Unrecorded = %d, want 1: the pre-v17 memory is the one gap in this read", set.Unrecorded)
	}
	if note := AsOfUnknownNote(set.Unrecorded); note == "" {
		t.Error("AsOfUnknownNote(1) is empty, want the sentence that names the gap")
	}
	// A read with no gap has no sentence, so a surface cannot print a disclosure
	// that has stopped being true.
	if note := AsOfUnknownNote(0); note != "" {
		t.Errorf("AsOfUnknownNote(0) = %q, want empty", note)
	}
}

// TestCandidatesAsOfInTheFutureEqualsTheCurrentSet: the strongest check that the
// read REPLAYS rather than reconstructs. Past every recorded write, the version
// row a memory contributes is its newest one, so a historical candidate set and a
// current one must agree on membership, content and category. A divergence means
// something in the historical path is inventing a state the store is not in.
func TestCandidatesAsOfInTheFutureEqualsTheCurrentSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", "a memory whose wording is edited twice", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	kept, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "the maintenance sweep reclaims scratch older than an hour", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the second memory: %v", err)
	}
	category := "architecture"
	if err := s.UpdateMemory(ctx, testProject, first, strPtr("a memory whose wording is edited once"), &category, nil, nil); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, first, strPtr("a memory whose wording is edited twice"), nil, nil, nil); err != nil {
		t.Fatalf("second update: %v", err)
	}
	stampHistory(t, s, first, asOfStampSave, asOfStampRewrite, asOfStampLate)
	stampHistory(t, s, kept, asOfStampSave)

	future := asOfSearch(t, s, testProject, "wording edited", asOfStampFarFuture)
	if !containsID(future, first) {
		t.Fatalf("the edited memory is missing from %v", candidateIDs(t, future))
	}
	row := candidateByID(t, future, first)
	if row.Content != "a memory whose wording is edited twice" {
		t.Errorf("content = %q, want the newest recorded wording", row.Content)
	}
	if row.Category != "architecture" {
		t.Errorf("category = %q, want architecture: the relabelling is part of the version, and a past read has to carry it", row.Category)
	}
	// The same question without as_of, which is the current read: identical rows.
	now := asOfAt(t, asOfStampFarFuture)
	current, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "wording edited",
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
	})
	if err != nil {
		t.Fatalf("Candidates (current): %v", err)
	}
	if len(current.Rows) != len(future.Rows) {
		t.Errorf("the current read returned %d rows and the historical read %d, want the same set", len(current.Rows), len(future.Rows))
	}
	for i := range future.Rows {
		if future.Rows[i].ID != current.Rows[i].ID {
			t.Errorf("row %d is %s historically and %s currently, want the same order", i, future.Rows[i].ID, current.Rows[i].ID)
		}
	}
}

func candidateByID(t *testing.T, set *CandidateSet, id string) Candidate {
	t.Helper()
	for _, c := range set.Rows {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("candidate %s is not in the set %v", id, candidateIDs(t, set))
	return Candidate{}
}

func pos(list []string, id string) int {
	for i, got := range list {
		if got == id {
			return i
		}
	}
	return -1
}
