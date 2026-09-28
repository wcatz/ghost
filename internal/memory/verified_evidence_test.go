package memory

import (
	"context"
	"strings"
	"testing"
)

// A `verified` evidence record is what a reader counts in
// EvidenceCounts.Verified, and it is the only account of "somebody checked this"
// that survives a second save. The memories column answers a different question
// — the LATEST state — so a store that writes verified_at and never appends a
// record reports "1 observation, 0 verified" for a fact somebody just checked,
// and a reader has no way to tell that from a fact nobody checked.
//
// #673 left this to the validity writers on purpose (evidence.go's
// evidenceVerified comment reserves the kind for them), so these tests are the
// half of the contract that #673 could not write alone.

// verifiedStamp is a stored-layout verified_at as a caller states it.
func verifiedStamp(v string) *string { return &v }

// recordsByKind counts a memory's evidence records per kind.
//
// The counts alone cannot answer what these tests ask. EvidenceCounts counts
// EVERY record as an observation "whatever its kind", so a verified record is
// counted twice over and a change in Observations cannot tell a new report from a
// new check. The kind is the thing worth asserting, so this reads the records.
func recordsByKind(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	records, err := s.MemoryProvenance(t.Context(), id)
	if err != nil {
		t.Fatalf("MemoryProvenance(%s): %v", id, err)
	}
	got := map[string]int{}
	for _, r := range records {
		got[r.Kind]++
	}
	return got
}

// The first and simplest: a save that verified the fact leaves both the column
// and the record.
func TestUpsertVerifiedSaveRecordsAVerification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the reconcile job holds the advisory lock for the whole window",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}
	if dup != "" {
		t.Fatalf("the first save folded into %q; a fresh memory is not a fold target, so the fresh-insert branch is not what this test reached", dup)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d, want 1 — the save verified the fact and left no record of the check", counts.Verified)
	}
	// A verification is an ADDITIONAL record, not a replacement for the
	// observation — and EvidenceCounts.Observations counts it as one too (every
	// record is an observation "whatever its kind"), so the kinds are what say so.
	if kinds := recordsByKind(t, s, id); kinds[evidenceObserved] != 1 || kinds[evidenceVerified] != 1 {
		t.Errorf("records = %v, want one observed and one verified", kinds)
	}
	if got := counts.Label(); !strings.Contains(got, "1 verified") {
		t.Errorf("Label() = %q, want it to report the verification", got)
	}
}

// insertMemory is shared by Create and CreateFromCorpus, and neither is reached
// by the Upsert tests above, so this is the only coverage of that branch — which
// a mutation confirmed: dropping the append from insertMemory left the whole file
// green until this test existed.
func TestCreateVerifiedRecordsAVerification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "the build host is not the laptop",
		Source:     "mcp",
		VerifiedAt: verifiedStamp("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d after a verifying Create, want 1", counts.Verified)
	}
	if kinds := recordsByKind(t, s, id); kinds[evidenceObserved] != 1 || kinds[evidenceVerified] != 1 {
		t.Errorf("records = %v, want one observed and one verified", kinds)
	}
}

// And the same branch with nothing stated, because a store whose Create path
// appended unconditionally would report every seeded row as checked.
func TestCreateWithoutVerifiedRecordsNoVerification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact",
		Content:  "the mirror job is idempotent",
		Source:   "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 0 {
		t.Errorf("Verified = %d after a Create that verified nothing, want 0", counts.Verified)
	}
}

// A corpus row's verified_at is DATA ABOUT THE CORPUS, not a check Ghost
// observed, and those are different claims with different stamps. The record's
// stamp is the store's own clock, so appending one for a corpus row asserts "this
// fact was checked at <now>" for a dataset that may be asserting a check from
// years earlier — the inversion the whole store-clock rule exists to prevent,
// reached through a path the rule was never stated on.
//
// CreateFromCorpus opts out. Its own contract already says the row is never
// injected, mirrored or quoted, so nothing can read the fabricated record; the
// point is that the invariant is stated and enforced rather than held by the
// accident that no corpus sets the field today.
func TestCreateFromCorpusRecordsNoVerification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Far enough in the past that a store clock could not have produced it, which
	// is what makes a fabricated record distinguishable from a real one.
	const claimed = "2019-03-04 00:00:00"
	id, err := s.CreateFromCorpus(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "a corpus row that asserts a check from long ago",
		Source:     "mcp",
		VerifiedAt: verifiedStamp(claimed),
	})
	if err != nil {
		t.Fatalf("CreateFromCorpus: %v", err)
	}

	// The COLUMN is the corpus's own claim and is stored verbatim: refusing it
	// would be losing the dataset's data, which is the opposite of the defect.
	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if mems[0].VerifiedAt == nil || *mems[0].VerifiedAt != claimed {
		t.Errorf("memories.verified_at = %v, want the corpus's own claim %q preserved", mems[0].VerifiedAt, claimed)
	}

	// And no EVENT was recorded.
	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 0 {
		t.Errorf("Verified = %d for a corpus row, want 0: the table records events, and ingesting a dataset is not a check", counts.Verified)
	}
	if kinds := recordsByKind(t, s, id); kinds[evidenceVerified] != 0 {
		t.Errorf("records = %v, want no verified record on a corpus row", kinds)
	}
	// The observation still happens: a corpus row was ingested, and that much
	// Ghost did do. Opting out of the verification is not opting out of the
	// evidence table.
	if kinds := recordsByKind(t, s, id); kinds[evidenceObserved] != 1 {
		t.Errorf("records = %v, want the ingestion's one observed record", kinds)
	}
}

// The other side, so the opt-out cannot be implemented by dropping the append
// wholesale: Create writes the same Memory and MUST record the verification.
func TestCreateStillRecordsAVerificationBesideTheCorpusOptOut(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "a written fact whose author checked it",
		Source:     "mcp",
		VerifiedAt: verifiedStamp("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d after a verifying Create, want 1 — the corpus opt-out leaked into the harness path", counts.Verified)
	}
}

// The gate, and the reason the update path needs one at all: a row verified by
// an EARLIER save keeps its verified_at through COALESCE, so an edit that never
// mentions a verification still finds the column populated. Appending on that
// would record a check that did not happen — in the one table whose whole claim
// is that a row means an event occurred. This is the case the memories column
// cannot be read for, which is the reason the test asserts the COUNT and not the
// column.
func TestUpdateWithoutVerifiedStatedAddsNoVerificationRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the queue depth alert threshold is eighty",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}

	// A content-only correction. The stored verified_at survives by COALESCE, so
	// the column is still populated going into the edit.
	corrected := "the queue depth alert threshold is eighty-five"
	if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{
		Content: &corrected,
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}

	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if mems[0].VerifiedAt == nil {
		t.Fatalf("the edit cleared verified_at; COALESCE was supposed to keep it and the gate's premise is gone")
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d after an edit that verified nothing, want 1: the column kept a verification this edit never made", counts.Verified)
	}
}

// The other side of the same gate, so the test above cannot pass by an append
// that never fires at all: an edit that DOES state a verification records one.
func TestUpdateWithVerifiedStatedRecordsAVerification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the chart of record for latency is the p99 dashboard",
		"mcp", 0.6, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}

	before, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts (before): %v", err)
	}
	if before.Verified != 0 {
		t.Fatalf("the save recorded %d verifications without stating one", before.Verified)
	}

	if err := s.UpdateMemoryWithOptions(ctx, testProject, id, UpdateOptions{
		Validity: Validity{VerifiedAt: verifiedStamp("2026-09-21 00:00:00")},
	}); err != nil {
		t.Fatalf("UpdateMemoryWithOptions: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d, want 1: the edit asserted a check and recorded none", counts.Verified)
	}
	// And the edit is a CHECK of the fact, not a new REPORT of it: #673's writers
	// append an observed record to Create and to every Upsert branch but not here,
	// and widening that from inside this PR would inflate the report count on every
	// content edit. So the exact record set is one verified and no new observed.
	if kinds := recordsByKind(t, s, id); kinds[evidenceVerified] != 1 || kinds[evidenceObserved] != 1 {
		t.Errorf("records = %v, want the save's one observed plus this edit's one verified", kinds)
	}
}

// The fold is the case the whole mechanism is for, and the one this PR's
// validity merge made reachable: the row a fold leaves in the corpus is the
// target, not the copy the response names, so a verification that reached only
// the copy would leave the row search returns with no account of the check.
func TestUpsertFoldRecordsTheVerificationOnTheSurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the vector identity is compared as an opaque string, never parsed"
	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		text, "mcp", 0.6, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	// FoldOnly so the fold is decided by foldOnlyEquivalent rather than a Jaccard
	// bar this PR does not own, and so no copy competes for the count.
	_, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"The vector identity is compared as an opaque string, never parsed",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
			FoldOnly: true,
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	if dup != target {
		t.Fatalf("fold returned duplicateOf %q, want the target %q — the fold branch was not reached", dup, target)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, target)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified on the fold target = %d, want 1: the check was recorded somewhere other than the row the corpus keeps", counts.Verified)
	}
	// The first save's report, the fold's second report, and the check. Exactly
	// three records, so the fold's second report is not double-counted by the
	// verification landing next to it.
	if kinds := recordsByKind(t, s, target); kinds[evidenceObserved] != 2 || kinds[evidenceVerified] != 1 {
		t.Errorf("records = %v, want two observed (first save, fold's report) and one verified", kinds)
	}
}

// The default fold writes two rows, and BOTH carry the check — the survivor
// because it is the row the corpus keeps, and the copy because it is a row of its
// own whose own verified_at column says the fact was checked. A reader who
// retrieved the copy must not find it unverified while its column claims
// otherwise.
func TestUpsertDefaultFoldRecordsTheVerificationOnBothRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, _, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the bench seed carries its category in the row, not the filename",
		"mcp", 0.6, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("UpsertWithOptions (target): %v", err)
	}

	folded, dup, _, err := s.UpsertWithOptions(ctx, testProject, "gotcha",
		"the bench seed carries its category in the row rather than the filename",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions (fold): %v", err)
	}
	// Fails rather than skips, because the default fold IS the branch under test
	// here and a skip would turn a threshold change into a green test asserting
	// nothing. The wording clears upsertMergeThreshold=0.5 at 9/12 = 0.75, a 50%
	// margin, and the FoldOnly sibling above is the threshold-independent coverage
	// of the survivor's append.
	if dup != target {
		t.Fatalf("the wording did not fold onto %s (dup=%q): this test needs the DEFAULT fold, whose 0.5 Jaccard bar the wording clears at 0.75", target, dup)
	}

	for name, id := range map[string]string{"survivor": target, "copy": folded} {
		counts, err := s.MemoryEvidenceCounts(ctx, id)
		if err != nil {
			t.Fatalf("MemoryEvidenceCounts(%s): %v", name, err)
		}
		if counts.Verified != 1 {
			t.Errorf("%s Verified = %d, want 1", name, counts.Verified)
		}
	}
}

// The verified record is stamped with the STORE's clock, never the caller's.
// A verifier that could date its own check could date it before the thing it
// checked, which is why AppendVerifiedEvidenceTx takes a bool and splices a
// literal rather than accepting a string. This pins that the two stamps stay
// separate facts: the column carries what the caller claimed, the record carries
// when Ghost heard about it.
func TestVerificationRecordCarriesTheStoreClockNotTheCallers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A stamp far enough in the past that a store clock could not have produced it.
	const claimed = "2020-01-01 00:00:00"
	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
		"the migration ran on a host with a frozen clock",
		"mcp", 0.6, nil, UpsertOptions{
			Validity: Validity{VerifiedAt: verifiedStamp(claimed)},
		})
	if err != nil {
		t.Fatalf("UpsertWithOptions: %v", err)
	}

	records, err := s.MemoryProvenance(ctx, id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	var verified *Evidence
	for i := range records {
		if records[i].Kind == evidenceVerified {
			verified = &records[i]
		}
	}
	if verified == nil {
		t.Fatal("no verified record was written")
	}
	if verified.VerifiedAt == nil {
		t.Fatal("a verified record carries no stamp, so a reader cannot tell when the check was recorded")
	}
	if *verified.VerifiedAt == claimed {
		t.Errorf("the record's verified_at is the caller's claim %q; a verifier must not be able to date its own check", claimed)
	}
	// The column keeps the caller's value, because that is a different question.
	mems, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if mems[0].VerifiedAt == nil || *mems[0].VerifiedAt != claimed {
		t.Errorf("memories.verified_at = %v, want the caller's claim %q", mems[0].VerifiedAt, claimed)
	}
}

// A save that verifies a fact, and a LATER save of the same fact that verifies it
// again, are two checks. The table is append-only for exactly this reason, and
// collapsing them would make "3 observations, 1 verified" indistinguishable from
// "3 observations, 3 verified" — the difference between a fact one agent checked
// and a fact three agents checked.
func TestRepeatedVerificationAccumulates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the retry budget is three attempts, not five"
	for i := range 3 {
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			text, "mcp", 0.6, nil, UpsertOptions{
				Validity: Validity{VerifiedAt: verifiedStamp("2026-09-20 00:00:00")},
				FoldOnly: true,
			}); err != nil {
			t.Fatalf("UpsertWithOptions (save %d): %v", i+1, err)
		}
	}

	all, err := s.GetAll(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("GetAll returned %d rows, want 1 — FoldOnly stores no copy of its own", len(all))
	}
	counts, err := s.MemoryEvidenceCounts(ctx, all[0].ID)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 3 {
		t.Errorf("Verified = %d after three verifying saves, want 3", counts.Verified)
	}
	if kinds := recordsByKind(t, s, all[0].ID); kinds[evidenceObserved] != 3 || kinds[evidenceVerified] != 3 {
		t.Errorf("records = %v, want three of each — one report and one check per save", kinds)
	}
}
