package memory

import (
	"context"
	"testing"
)

// The round trip is the whole shape of the bug: an export carries the memory's
// evidence records, the import writes each of them AND appends an arrival record
// for the store's own arrival. When the artifact already holds a stamped record —
// which it does the moment the origin store's own live writers put one there —
// the arrival re-states a verification that has already been counted, and a second
// export/import hop re-states it again. So a corpus that is exported and imported
// N times reports N+1 verifications of one real check, and the count a reader
// trusts drifts upward every time the corpus moves between machines.
//
// Exactly 1 is the assertion, not "at least 1": the number is the point, and a
// floor is what let this through in the first place.
func TestImportDoesNotReinflateAVerificationTheArtifactAlreadyCarries(t *testing.T) {
	// One store records the observation and the check the way a live save does.
	origin := testStore(t)
	ctx := context.Background()
	confidence := 0.8
	const content = "the mirror job is idempotent and re-runs from its own cursor"

	if err := origin.EnsureProject(ctx, testProject, "/tmp/origin-round-trip", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	originID, err := origin.Create(ctx, testProject, Memory{
		Category: "fact", Content: content, Source: "mcp", Importance: 0.7,
		Agent: "opencode", SourceRef: "docs/mirror.md", Confidence: &confidence,
		VerifiedAt: stampPtr("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create on the origin store: %v", err)
	}

	// Hop one: export what the origin holds and import it elsewhere.
	hop1 := importOnce(t, origin, testProject, originID, "hop1")
	if got := verifiedCount(t, hop1, originID); got != 1 {
		t.Fatalf("after one hop Verified = %d, want 1 — the artifact's own stamped record plus the arrival is two for one check", got)
	}

	// Hop two: export the FIRST store and import it into a third. The artifact
	// now carries the arrival record as well, so a guard that only looked at the
	// MEMORY's column would re-inflate here; one that looks at any carried record
	// does not. The kind is not consulted, which is the point — the origin's stamp
	// may be `verified` or the import's own `imported`, and either way the check
	// already happened.
	hop2 := importOnce(t, hop1, testProject, originID, "hop2")
	if got := verifiedCount(t, hop2, originID); got != 1 {
		t.Errorf("after two hops Verified = %d, want 1 — a second hop re-inflated a check that was already counted", got)
	}
}

// The kind-agnostic half, and the case the two-hop test above cannot reach.
//
// That test's artifact already carries a `verified` record, so a guard that looked
// only for that kind would pass it. This one does not: the first hop's stamp is the
// ARRIVAL, whose kind is `imported`, because the artifact it came from recorded the
// check only in the memory's own column. So the artifact for the second hop holds
// exactly one stamped record and it is an `imported` one — and a kind-specific guard
// finds nothing, stamps the second arrival, and reports two verifications of one
// check. This is the B→C hop, and it is the only place the two shapes differ.
func TestSecondHopDoesNotReinflateWhenOnlyTheArrivalIsStamped(t *testing.T) {
	verified := "2026-09-20 00:00:00"
	const id = "arrivalonly0000000000000000AA"

	// Hop one: the artifact's records are all unstamped, so the arrival is the
	// verification — which is what the mirror test above pins as correct.
	hop1 := testStore(t)
	if _, _, _, err := hop1.ImportMemory(context.Background(), PortableMemory{
		ID: id, ProjectID: testProject, Category: "fact",
		Content: "a fact whose check lives only in the column",
		Source:  "onboarding", VerifiedAt: &verified,
		Evidence: []PortableEvidence{
			{Kind: "observed", Agent: "claude-code", ObservedAt: &verified},
		},
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("hop 1 ImportMemory: %v", err)
	}
	if got := verifiedCount(t, hop1, id); got != 1 {
		t.Fatalf("after hop 1 Verified = %d, want 1", got)
	}

	// Confirm the single stamped record really is the arrival, so this test cannot
	// accidentally be the `verified`-kind case the two-hop test already covers.
	records, err := hop1.MemoryProvenance(context.Background(), id)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	var stampedKinds []string
	for _, r := range records {
		if r.VerifiedAt != nil {
			stampedKinds = append(stampedKinds, r.Kind)
		}
	}
	if len(stampedKinds) != 1 || stampedKinds[0] != evidenceImported {
		t.Fatalf("stamped record kinds = %v, want exactly one and it must be %q", stampedKinds, evidenceImported)
	}

	// Hop two.
	hop2 := importOnce(t, hop1, testProject, id, "hop2")
	if got := verifiedCount(t, hop2, id); got != 1 {
		t.Errorf("after hop 2 Verified = %d, want 1 — a guard that keys on the carried record's KIND re-inflated the arrival's own stamp", got)
	}
}

// The mirror of the above, and the reason the guard is not "the artifact had any
// evidence": a memory whose artifact holds records but NONE of them stamped must
// still get the arrival record's stamp from the memory's own verified_at column. A
// guard that keyed on len(Evidence) > 0 would silently drop a real verification
// whenever the artifact carried observations, which is the common case.
func TestImportStillStampsTheArrivalWhenNoCarriedRecordIsVerified(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const id = "arrivalstamp0000000000000000AA"
	verified := "2026-09-20 00:00:00"
	_, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: id, ProjectID: testProject, Category: "fact",
		Content: "an artifact whose observations are all unstamped",
		Source:  "onboarding", VerifiedAt: &verified,
		Evidence: []PortableEvidence{
			{Kind: "observed", Agent: "claude-code", ObservedAt: &verified},
			{Kind: "observed", Agent: "codex", ObservedAt: &verified},
		},
	}, ImportOptions{Apply: true})
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	// The two carried observations, plus the arrival — and the arrival is the one
	// stamped, because the column says somebody checked this and nothing in the
	// artifact says so.
	if counts.Verified != 1 {
		t.Errorf("Verified = %d, want 1: the memory's own verified_at has no stamped record beside it, so the arrival must be it", counts.Verified)
	}
	if counts.Observations != 3 {
		t.Errorf("Observations = %d, want 3 (two carried plus the arrival)", counts.Observations)
	}
}

// A carried record whose verified_at is the empty string is not a verification.
// The artifact form is hand-edited and `omitempty` drops a nil rather than a
// pointer to "", so `"verified_at": ""` is reachable, and the memory row treats ""
// as no claim. Keying the guard on a non-empty value is what keeps that artifact
// from suppressing a real arrival stamp.
func TestImportStampsTheArrivalWhenCarriedStampsAreEmptyStrings(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const id = "emptystamp00000000000000000AA"
	verified := "2026-09-20 00:00:00"
	empty := ""
	_, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID: id, ProjectID: testProject, Category: "fact",
		Content: "an artifact whose carried stamp is the empty string",
		Source:  "onboarding", VerifiedAt: &verified,
		Evidence: []PortableEvidence{
			{Kind: "observed", Agent: "claude-code", ObservedAt: &verified, VerifiedAt: &empty},
		},
	}, ImportOptions{Apply: true})
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}

	counts, err := s.MemoryEvidenceCounts(ctx, id)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Errorf("Verified = %d, want 1 — an empty string is not a verification, so the arrival must still be stamped", counts.Verified)
	}
}

// The precedence, and the half of the rule the inheritance tests cannot see: when
// the EMISSION states a value it WINS, and the source is consulted only where the
// emission is silent. Both halves are the same rule — a value the caller supplied
// overrides, silence inherits — and the other tests here only ever observe the
// second half, so a resolver that ignored the emission entirely would pass all of
// them.
//
// The emission reaching a consolidator's output carries nothing today, so this is
// also the test that keeps the emission's columns load-bearing: it is the only
// path by which a caller of ReplaceNonManual can restate a claim rather than
// inherit it.
func TestReplaceNonManualTheEmissionsClaimBeatsTheSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the read replica lags by one commit under load"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		Agent: "claude-code", SourceRef: "docs/old.md",
		ValidFrom:  stampPtr("2026-01-01 00:00:00"),
		VerifiedAt: stampPtr("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const rewritten = "the read replica lags the primary by one commit under load"
	restated := "2027-01-01 00:00:00"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "architecture", Content: rewritten, Importance: 0.7,
		ReplacesIDs: []string{id},
		// The emission restates the window and speaks for itself where the
		// source row did not.
		ValidFrom:  &restated,
		VerifiedAt: stampPtr("2026-11-11 00:00:00"),
		SourceRef:  "docs/new.md",
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var newID string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, rewritten,
	).Scan(&newID); err != nil {
		t.Fatalf("find the successor row: %v", err)
	}
	rows, err := s.GetByIDs(ctx, []string{newID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(rows))
	}
	row := rows[0]
	if row.ValidFrom == nil || *row.ValidFrom != restated {
		t.Errorf("successor valid_from = %v, want the emission's %q", row.ValidFrom, restated)
	}
	if row.VerifiedAt == nil || *row.VerifiedAt != "2026-11-11 00:00:00" {
		t.Errorf("successor verified_at = %v, want the emission's 2026-11-11 00:00:00", row.VerifiedAt)
	}
	if row.SourceRef != "docs/new.md" {
		t.Errorf("successor source_ref = %q, want the emission's docs/new.md", row.SourceRef)
	}
	// And where the emission said nothing, the source still supplies it — that is
	// the other half of the same rule, in the same row.
	if row.Agent != "claude-code" {
		t.Errorf("successor agent = %q, want the source row's claude-code, since the emission stated none", row.Agent)
	}
}

// importOnce exports a memory from one store and imports it into a fresh one,
// returning the destination.
func importOnce(t *testing.T, from *Store, projectID, memoryID, destProject string) *Store {
	t.Helper()
	ctx := context.Background()
	rows, err := from.PortableMemories(ctx, []string{projectID})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	var carried PortableMemory
	found := false
	for _, r := range rows {
		if r.ID == memoryID {
			carried, found = r, true
		}
	}
	if !found {
		t.Fatalf("PortableMemories returned no row for %s", memoryID)
	}
	if len(carried.Evidence) == 0 {
		t.Fatalf("the artifact carries no evidence records; the round trip is not exercising anything")
	}

	dest := testStore(t)
	if _, _, _, err := dest.ImportMemory(ctx, carried, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory into %s: %v", destProject, err)
	}
	return dest
}

// verifiedCount reads the number of stamped records straight from the table, so
// the assertion is on the records and not on the row's own verified_at column —
// the column is copied through and would read 1 in both the broken and the fixed
// build.
func verifiedCount(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_provenance WHERE memory_id = ? AND verified_at IS NOT NULL AND verified_at <> ''`,
		memoryID,
	).Scan(&n); err != nil {
		t.Fatalf("count stamped evidence rows: %v", err)
	}
	return n
}

// A merge that carries evidence forward must carry the ROW's claims with it, and
// the two disagreeing is the failure: carryEvidenceTx copies the sources' records
// — verified stamps included — onto a row that ReplaceNonManual inserts without
// valid_from, valid_until, verified_at, confidence or source_ref. So a rewritten
// memory reads verified_at NULL while its own evidence says somebody checked it,
// and a reader comparing the row against the support summary finds the table
// contradicting the memory it describes.
func TestReplaceNonManualFreshRowKeepsTheColumnsItsCarriedEvidenceClaims(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	confidence := 0.7

	const content = "the writer pins MaxOpenConns(1) because a pool query inside a write transaction deadlocks"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "architecture", Content: content, Source: "mcp", Importance: 0.8,
		Agent: "claude-code", SourceRef: "internal/memory/store.go", Confidence: &confidence,
		ValidFrom:  stampPtr("2026-01-15 00:00:00"),
		ValidUntil: stampPtr("2027-12-31 23:59:59"),
		VerifiedAt: stampPtr("2026-09-20 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The consolidation that supersedes it: a fresh row carrying the evidence
	// forward, which is the path ReplaceNonManual's INSERT takes.
	const rewritten = "the writer pins MaxOpenConns(1) so a pool query inside an open write transaction cannot deadlock"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "architecture", Content: rewritten, Importance: 0.8,
		ReplacesIDs: []string{id},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// The source row is DELETED by the replace, so the successor is found by its
	// own text; that it exists at all is the carry's precondition, checked below.
	var newID string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, rewritten,
	).Scan(&newID); err != nil {
		t.Fatalf("find the successor row: %v", err)
	}

	// The evidence arrived — that is what the existing carry test pins — so the
	// row's own columns have to agree with it.
	counts, err := s.MemoryEvidenceCounts(ctx, newID)
	if err != nil {
		t.Fatalf("MemoryEvidenceCounts: %v", err)
	}
	if counts.Verified != 1 {
		t.Fatalf("the successor carries %d stamped record(s); the carry did not happen, so this test is not exercising the columns", counts.Verified)
	}

	got, err := s.GetByIDs(ctx, []string{newID})
	if err != nil || len(got) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(got))
	}
	row := got[0]
	if row.VerifiedAt == nil {
		t.Errorf("successor verified_at is NULL while its own evidence carries a verification — the row and the table contradict each other")
	}
	for _, tc := range []struct {
		name string
		got  *string
		want string
	}{
		{"valid_from", row.ValidFrom, "2026-01-15 00:00:00"},
		{"valid_until", row.ValidUntil, "2027-12-31 23:59:59"},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("successor %s = %v, want %q", tc.name, tc.got, tc.want)
		}
	}
	if row.Confidence == nil || *row.Confidence != confidence {
		t.Errorf("successor confidence = %v, want %v", row.Confidence, confidence)
	}
	if row.SourceRef != "internal/memory/store.go" {
		t.Errorf("successor source_ref = %q, want the source row's reference", row.SourceRef)
	}
	if row.Agent != "claude-code" {
		t.Errorf("successor agent = %q, want claude-code — the source row's author, since the emission reports none", row.Agent)
	}
}

// A rewrite of something that recorded NO claim must not invent one. The gate is
// the source row's own columns, not a default, so the successor of an ordinary
// memory is ordinary.
func TestReplaceNonManualFreshRowInventsNoClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "the mirror runs on a timer and skips a cycle it cannot start"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: content, Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const rewritten = "the mirror runs on a timer and skips any cycle it fails to start"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: rewritten, Importance: 0.6,
		ReplacesIDs: []string{id},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var newID string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, rewritten,
	).Scan(&newID); err != nil {
		t.Fatalf("find the successor row: %v", err)
	}
	rows, err := s.GetByIDs(ctx, []string{newID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(rows))
	}
	row := rows[0]
	if row.VerifiedAt != nil || row.ValidFrom != nil || row.ValidUntil != nil ||
		row.Confidence != nil || row.SourceRef != "" || row.Agent != "" {
		t.Errorf("successor of an unclaimed row carries a claim: verified_at=%v valid_from=%v valid_until=%v confidence=%v source_ref=%q agent=%q",
			row.VerifiedAt, row.ValidFrom, row.ValidUntil, row.Confidence, row.SourceRef, row.Agent)
	}
}

// The trust filters must not start reporting these columns. A merge that
// re-attributed a row to whichever emission happened to rewrite it would be a
// provenance change nobody asked for, and this is the field the reader quotes.
func TestReplaceNonManualFreshRowKeepsTheSourcesTrustNotTheEmissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	confidence := 0.3

	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "the FTS leg truncates at ten terms", Source: "manual", Importance: 0.5,
		Agent: "codex", SourceRef: "PR-1", Confidence: &confidence,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const rewritten = "the FTS leg truncates its query terms at ten"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "gotcha", Content: rewritten, Importance: 0.5,
		ReplacesIDs: []string{id},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var newID string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, rewritten,
	).Scan(&newID); err != nil {
		t.Fatalf("find the successor row: %v", err)
	}
	rows, err := s.GetByIDs(ctx, []string{newID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(rows))
	}
	// A manual row's successor is a reflection row, and that is the one trust
	// change the path is allowed: consolidation's whole job is to re-attribute.
	if rows[0].Source != "reflection" {
		t.Errorf("source = %q, want reflection — that IS the change consolidation makes", rows[0].Source)
	}
	// The author and the reference are the source row's, because the emission
	// reports none and inventing one would be a fabrication of exactly the kind
	// these columns exist to prevent.
	if rows[0].Agent != "codex" || rows[0].SourceRef != "PR-1" {
		t.Errorf("successor provenance = (%q, %q), want (codex, PR-1) from the source row", rows[0].Agent, rows[0].SourceRef)
	}
}
