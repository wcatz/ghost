package memory

import (
	"context"
	"database/sql"
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

// A merge must not compose a window out of two rows' halves.
//
// inheritedClaims filled valid_from, valid_until and verifiedAt independently, each
// from the first source that had it — so merging A (valid_from 2030) with B
// (valid_until 2020) produced a row born expired, with both sources' evidence
// carried onto it as though the pair were one claim. That is the contradiction
// every other writer refuses: UpsertWithOptions' fold and
// UpdateMemoryWithOptions both run CheckWindowOrder over the pair they compose,
// and the tool boundary refuses it at the argument. This path had no check.
//
// The assertion is not "some error" but "no successor row holds a window that ends
// before it starts", because there are two legitimate outcomes — inherit a
// consistent pair, or inherit none — and the test must not pin which.
func TestReplaceNonManualNeverComposesAWindowThatEndsBeforeItStarts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A states a start far in the future and no end.
	a, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the lane switchover is scheduled for 2030", Source: "mcp", Importance: 0.6,
		ValidFrom: stampPtr("2030-01-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	// B states an end in the past and no start.
	b, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the legacy lane was retired in 2020", Source: "mcp", Importance: 0.6,
		ValidUntil: stampPtr("2020-06-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}

	const merged = "the lane migration is complete and the legacy lane is gone"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: merged, Importance: 0.7,
		ReplacesIDs: []string{a, b},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	row := successorRow(t, s, merged)
	if row.ValidFrom != nil || row.ValidUntil != nil {
		// Either half on its own is defensible, so the real assertion is the ORDER.
		if err := CheckWindowOrder(Validity{ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil}, Validity{}); err != nil {
			t.Errorf("the merged row holds a window that ends before it starts: %v (from=%v until=%v)",
				err, row.ValidFrom, row.ValidUntil)
		}
	}
}

// The same class from the other side: one source row that ITSELF holds an
// out-of-order window, which is reachable because Store.Create, ImportMemory and
// RestoreSnapshot write the triple with no order check. Inheriting a pair as a
// unit copies the contradiction, so the check has to run on the inherited pair and
// not only on a composed one.
func TestReplaceNonManualDoesNotInheritAnOutOfOrderWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create does not check order, so this row is born contradictory — and a
	// pre-guard store, a hand edit or an import makes exactly this reachable.
	a, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the cutover window is recorded backwards here", Source: "mcp", Importance: 0.6,
		ValidFrom:  stampPtr("2030-01-01 00:00:00"),
		ValidUntil: stampPtr("2020-06-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const rewritten = "the cutover window has been recorded correctly since"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: rewritten, Importance: 0.6,
		ReplacesIDs: []string{a},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	row := successorRow(t, s, rewritten)
	if err := CheckWindowOrder(Validity{ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil}, Validity{}); err != nil {
		t.Errorf("the rewrite inherited a window that ends before it starts: %v (from=%v until=%v)",
			err, row.ValidFrom, row.ValidUntil)
	}
}

// The mirror: a CONSISTENT pair must still be inherited whole, or the check above
// is satisfied by dropping every window and the fix is just data loss.
func TestReplaceNonManualStillInheritsAConsistentWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the retention window is ninety days by policy", Source: "mcp", Importance: 0.6,
		ValidFrom:  stampPtr("2026-01-01 00:00:00"),
		ValidUntil: stampPtr("2027-12-31 23:59:59"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const rewritten = "retention is ninety days, and the policy is reviewed annually"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: rewritten, Importance: 0.6,
		ReplacesIDs: []string{a},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	row := successorRow(t, s, rewritten)
	if row.ValidFrom == nil || *row.ValidFrom != "2026-01-01 00:00:00" {
		t.Errorf("successor valid_from = %v, want the source's 2026-01-01 00:00:00", row.ValidFrom)
	}
	if row.ValidUntil == nil || *row.ValidUntil != "2027-12-31 23:59:59" {
		t.Errorf("successor valid_until = %v, want the source's 2027-12-31 23:59:59", row.ValidUntil)
	}
}

// successorRow finds the row a rewrite produced, failing if it is not there.
func successorRow(t *testing.T, s *Store, content string) Memory {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
	).Scan(&id); err != nil {
		t.Fatalf("find the successor row %q: %v", content, err)
	}
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(successor): %v (n=%d)", err, len(rows))
	}
	return rows[0]
}

// A complete pair anywhere in the source set wins, in EITHER id order.
//
// The single-pass version took the first source holding EITHER boundary as the
// whole unit, so a source holding only valid_from locked the window and every
// later source's valid_until was discarded — and which half survived depended on
// the order the consolidator happened to name ReplacesIDs in. A row that inherited
// a start and lost its end never retires, which is the outcome valid_until exists
// to prevent, on a path that runs unattended over the whole corpus.
//
// Both orders are asserted because the bug WAS order-dependence: a fix that only
// happened to be right for [A, B] would pass one of these.
func TestReplaceNonManualPrefersACompleteWindowWhicheverSourceIsNamedFirst(t *testing.T) {
	for _, order := range []struct {
		name string
		// halfFirst names which of the two ids leads.
		halfFirst string
	}{
		{"half named first", "half"},
		{"complete pair named first", "whole"},
	} {
		t.Run(order.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			// A complete, consistent window.
			whole, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the support window is open through next year", Source: "mcp", Importance: 0.6,
				ValidFrom: stampPtr("2026-01-01 00:00:00"), ValidUntil: stampPtr("2027-03-01 00:00:00"),
			})
			if err != nil {
				t.Fatalf("Create (whole): %v", err)
			}
			// And a row holding one boundary only, which is what used to win the
			// unit and close it.
			half, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the rollback window has a start but no end", Source: "mcp", Importance: 0.6,
				ValidFrom: stampPtr("2028-05-01 00:00:00"),
			})
			if err != nil {
				t.Fatalf("Create (half): %v", err)
			}

			ids := []string{whole, half}
			if order.halfFirst == "half" {
				ids = []string{half, whole}
			}
			content := "the support window is open through next year and the rollback is bounded"
			if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: content, Importance: 0.6, ReplacesIDs: ids,
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}

			row := successorRow(t, s, content)
			if row.ValidFrom == nil || *row.ValidFrom != "2026-01-01 00:00:00" {
				t.Errorf("valid_from = %v, want the complete pair's 2026-01-01 00:00:00 — a single-boundary source won the unit", row.ValidFrom)
			}
			if row.ValidUntil == nil || *row.ValidUntil != "2027-03-01 00:00:00" {
				t.Errorf("valid_until = %v, want the complete pair's 2027-03-01 00:00:00 — the complementary end was discarded", row.ValidUntil)
			}
		})
	}
}

// And when NO source supplies a complete pair, the result must never be a composed
// one. Half from one row and half from another is the pair no row asserted, so at
// most one boundary may survive — which boundary is the first id's, and that
// order dependence is a stated cost rather than a silent one.
func TestReplaceNonManualNeverComposesHalvesWhenNoSourceHasBoth(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	from, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the migration has a scheduled start", Source: "mcp", Importance: 0.6,
		ValidFrom: stampPtr("2026-01-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create (from): %v", err)
	}
	until, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the migration has a scheduled end", Source: "mcp", Importance: 0.6,
		ValidUntil: stampPtr("2027-03-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create (until): %v", err)
	}

	const content = "the migration is scheduled for a window"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.6,
		ReplacesIDs: []string{from, until},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	row := successorRow(t, s, content)
	if row.ValidFrom != nil && row.ValidUntil != nil {
		t.Errorf("the merged row holds a window composed across two rows (from=%v until=%v); "+
			"neither row asserted that pair", *row.ValidFrom, *row.ValidUntil)
	}
	// One boundary is a real half of a claim and is worth keeping — the fallback
	// exists so the fix is not satisfied by dropping every window.
	if row.ValidFrom == nil && row.ValidUntil == nil {
		t.Error("no source supplied a complete pair, yet the merge inherited no boundary at all — the fallback dropped a real half")
	}
}

// A window Ghost cannot READ is not a window, and it must not outrank a real one.
//
// Rule 1 is position-independent by design — a complete consistent pair wins
// whatever id the consolidator named first — and that is what makes an unreadable
// pair dangerous. nullStringPtr maps only SQL NULL to nil, so a stored empty
// string, or any text no layout parses, is a non-nil boundary. CheckWindowOrder
// deliberately returns nil for a pair it cannot read ("a half no layout reads is
// not a claim"), so the very check meant to qualify the candidate records it as a
// COMPLETE consistent pair, and it then wins and discards a real window named by
// another source — the mirror image of the loss rule 1 exists to remove.
//
// Such a row is reachable: Store.Create stores a stamp verbatim (nullIfEmptyPtr
// maps only the empty string), and ImportMemory and RestoreSnapshot write the
// column with no check at all. So this is a pre-existing row in a pre-existing
// database, not a hypothetical one.
//
// Both id orders, because position-independence is the property under test.
func TestReplaceNonManualAnUnreadableWindowNeverOutranksARealOne(t *testing.T) {
	for _, order := range []struct {
		name            string
		unreadableFirst bool
	}{
		{"unreadable named first", true},
		{"unreadable named last", false},
	} {
		t.Run(order.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()

			// A real, complete, consistent window.
			real, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the support window runs to the end of next year", Source: "mcp", Importance: 0.6,
				ValidFrom: stampPtr("2026-01-01 00:00:00"), ValidUntil: stampPtr("2027-03-01 00:00:00"),
			})
			if err != nil {
				t.Fatalf("Create (real): %v", err)
			}
			// A row whose window is text no layout reads. Create stores a stamp
			// verbatim, which is exactly how such a row gets into a real database.
			junk, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the migration window was recorded in prose", Source: "mcp", Importance: 0.6,
				ValidFrom: stampPtr("sometime last spring"), ValidUntil: stampPtr("when the dust settles"),
			})
			if err != nil {
				t.Fatalf("Create (unreadable): %v", err)
			}
			// Prove the premise: the junk row really does hold unreadable text.
			junkRow := successorRowByID(t, s, junk)
			if junkRow.ValidFrom == nil || *junkRow.ValidFrom != "sometime last spring" {
				t.Fatalf("premise broken: the source row holds %v, not the unreadable text", junkRow.ValidFrom)
			}

			ids := []string{real, junk}
			if order.unreadableFirst {
				ids = []string{junk, real}
			}
			content := "the support window runs to the end of next year, and the prose note is retired"
			if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
				Category: "fact", Content: content, Importance: 0.6, ReplacesIDs: ids,
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}

			row := successorRow(t, s, content)
			if row.ValidFrom == nil || *row.ValidFrom != "2026-01-01 00:00:00" {
				t.Errorf("valid_from = %v, want the readable window's 2026-01-01 00:00:00 — an unreadable boundary was treated as a stated claim", row.ValidFrom)
			}
			if row.ValidUntil == nil || *row.ValidUntil != "2027-03-01 00:00:00" {
				t.Errorf("valid_until = %v, want the readable window's 2027-03-01 00:00:00", row.ValidUntil)
			}
		})
	}
}

// And the unreadable value is not propagated onto the successor either. Without
// this the row would read validity_unparseable from then on, which stage 2
// surfaces on every answer that touches it with nothing to explain why — the
// permanently-confusing row nullIfEmptyPtr's own doc says no writer should leave
// behind. Asserted on the raw column, because a reader that cannot parse the value
// is exactly the thing being asserted about.
func TestReplaceNonManualDoesNotWriteAnUnreadableStampOntoTheSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	junk, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the rollback was scheduled in prose once", Source: "mcp", Importance: 0.6,
		ValidFrom: stampPtr("whenever the deploy lands"), ValidUntil: stampPtr("soon after"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const content = "the rollback is scheduled, in a form the reader can use"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.6, ReplacesIDs: []string{junk},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var rawFrom, rawUntil sql.NullString
	if err := s.db.QueryRow(
		`SELECT valid_from, valid_until FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
	).Scan(&rawFrom, &rawUntil); err != nil {
		t.Fatalf("read the successor's raw window: %v", err)
	}
	if rawFrom.Valid || rawUntil.Valid {
		t.Errorf("the successor carries a window no layout can read (from=%q until=%q); "+
			"it will report validity_unparseable forever with no way to tell why",
			rawFrom.String, rawUntil.String)
	}
}

// The same rule at the INSERT, from the other side. Every other writer binds the
// triple through nullIfEmptyPtr, and this one bound it raw — so an emission that
// stated the empty moment wrote "" into the column, which reads as a claim with no
// readable value. The row is the successor of a real rewrite, so it is a row a
// search will meet.
func TestReplaceNonManualRecordsTheEmptyMomentAsNoClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the freeze happens before the release", Source: "mcp", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const content = "the freeze happens before the release, and lasts a day"
	empty := ""
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.6,
		ValidFrom:   &empty, // the empty moment, which is not a claim
		ReplacesIDs: []string{a},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var rawFrom sql.NullString
	if err := s.db.QueryRow(
		`SELECT valid_from FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
	).Scan(&rawFrom); err != nil {
		t.Fatalf("read the successor's raw valid_from: %v", err)
	}
	if rawFrom.Valid {
		t.Errorf("the successor stored valid_from = %q; the empty moment is no claim, and every other "+
			"writer maps it to NULL through nullIfEmptyPtr", rawFrom.String)
	}
}

// verified_at is the THIRD value of the same triple, and it gets the same rule.
//
// The readability fix covered the two window boundaries, and this diff binds
// verified_at through nullIfEmptyPtr — which maps only the empty string. So a
// source whose verified_at holds text no layout reads was still copied onto the
// successor verbatim, and readValidity puts it in `unparseable`, so stage 2 emits
// validity_unparseable for a row the store believes carries a readable claim. The
// same defect, one column over, in the statement this change edited.
func TestReplaceNonManualDoesNotWriteAnUnreadableVerifiedAtOntoTheSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	junk, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the check was recorded as prose once", Source: "mcp", Importance: 0.6,
		VerifiedAt: stampPtr("last Tuesday, give or take"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row := successorRowByID(t, s, junk); row.VerifiedAt == nil || *row.VerifiedAt != "last Tuesday, give or take" {
		t.Fatalf("premise broken: the source holds verified_at %v, not the prose", row.VerifiedAt)
	}

	const content = "the check is recorded in a form the reader can use"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.6, ReplacesIDs: []string{junk},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	var raw sql.NullString
	if err := s.db.QueryRow(
		`SELECT verified_at FROM memories WHERE content = ? AND resolved_at IS NULL`, content,
	).Scan(&raw); err != nil {
		t.Fatalf("read the successor's raw verified_at: %v", err)
	}
	if raw.Valid {
		t.Errorf("the successor carries verified_at = %q, which no layout reads; "+
			"readValidity reports it unparseable and stage 2 emits validity_unparseable forever", raw.String)
	}
}

// "Readable" has to mean readable by the READER the rule exists to satisfy.
//
// ParseStamp accepts the zero instant — time.Parse("0001-01-01 00:00:00") succeeds
// and yields the zero time — but the reader that consumes the column disagrees on
// exactly that value: assemble.parseStampPtr maps t.IsZero() to nil, so readValidity
// records it in `unparseable` and stage 2 emits validity_unparseable for a row the
// store believes carries a readable claim. Narrow in reach (a hand-edited artifact
// or a restored snapshot), but a value the reader cannot read is not a stated claim
// however cleanly it parses.
func TestReplaceNonManualDoesNotTreatTheZeroInstantAsAStatedWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A real window to lose, so the test is not satisfied by dropping everything.
	real, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the support window is open through next year", Source: "mcp", Importance: 0.6,
		ValidFrom: stampPtr("2026-01-01 00:00:00"), ValidUntil: stampPtr("2027-03-01 00:00:00"),
	})
	if err != nil {
		t.Fatalf("Create (real): %v", err)
	}
	// A row whose window is the zero instant, which parses cleanly and reads as
	// nothing at all.
	zero, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the window was recorded as the zero instant", Source: "mcp", Importance: 0.6,
		ValidFrom: stampPtr("0001-01-01 00:00:00"), ValidUntil: stampPtr("0001-01-01 00:00:01"),
	})
	if err != nil {
		t.Fatalf("Create (zero): %v", err)
	}
	if at, ok := ParseStamp("0001-01-01 00:00:00"); !ok || !at.IsZero() {
		t.Fatalf("premise broken: ParseStamp no longer reports the zero instant as ok-and-zero (ok=%v at=%v)", ok, at)
	}

	const content = "the support window is open through next year, and the zero-instant note is retired"
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: content, Importance: 0.6, ReplacesIDs: []string{zero, real},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	row := successorRow(t, s, content)
	if row.ValidFrom == nil || *row.ValidFrom != "2026-01-01 00:00:00" {
		t.Errorf("valid_from = %v, want the real window's 2026-01-01 00:00:00 — the zero instant was treated as a stated claim", row.ValidFrom)
	}
	if row.ValidUntil == nil || *row.ValidUntil != "2027-03-01 00:00:00" {
		t.Errorf("valid_until = %v, want the real window's 2027-03-01 00:00:00", row.ValidUntil)
	}
}

// successorRowByID is successorRow for a row named by id rather than by content,
// so a test can inspect the SOURCE's window as a premise.
func successorRowByID(t *testing.T, s *Store, id string) Memory {
	t.Helper()
	rows, err := s.GetByIDs(context.Background(), []string{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(rows))
	}
	return rows[0]
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
