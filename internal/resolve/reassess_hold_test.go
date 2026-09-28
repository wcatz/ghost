package resolve

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// #712: the repair counted the rows it left resolved ("N still asserted by a
// link or correction") and never said which ones they were or what asserted
// each one, so an operator repairing a store could not tell a live edge (which
// `ghost supersede --withdraw` can undo) from a correction pairing (which it
// cannot), and could not check whether the pairing was itself wrong.

// TestReassessNamesWhatHoldsEachRow: every held row is listed with the thing
// that holds it — the live supersedes edge by its SOURCE, the correction by its
// own id. A held row with no name is a count an operator can only act on by
// re-deriving the whole graph, which is the work the pass just did.
func TestReassessNamesWhatHoldsEachRow(t *testing.T) {
	superseded := memory.Memory{ID: "superseded", Category: "gotcha", UpdatedAt: "2026-09-01 00:00:00",
		Content: "Cost estimate from May: $148/mo projected; actuals have since replaced it."}
	newer := memory.Memory{ID: "newer", Category: "gotcha", UpdatedAt: "2026-09-02 00:00:00",
		Content: "superseded the May cost estimate; the actuals document it now"}
	paired := memory.Memory{ID: "paired", Category: "gotcha", UpdatedAt: "2026-09-19 20:00:00",
		Content: "root cause: ledgerstate/imported_reward_inputs.go never sets CalculationVersion on imported reward_snapshot rows; unusable (closed)"}
	correction := memory.Memory{ID: "correction", Category: "gotcha", UpdatedAt: "2026-09-19 21:00:00",
		Content: "CORRECTION/RESOLUTION to the imported reward_snapshot P0: the bug IS ALREADY FIXED ON MAIN. Commit d646e680 adds CalculationVersion to ledgerstate/imported_reward_inputs.go. NO PR IS NEEDED FROM US."}
	store := &fakeStore{
		alreadyResolved: []memory.Memory{superseded, paired},
		candidates:      []memory.Memory{newer, correction},
		links: []memory.Link{{
			SourceID: newer.ID, TargetID: superseded.ID, Relation: "supersedes", Source: "llm",
		}},
	}
	// The classifier would KEEP both; neither is asked about, because the
	// up-front floor settles them for free.
	cls := &fakeClassifier{drop: map[string]bool{}}

	res, _, err := Reassess(context.Background(), store, cls, "proj", false, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 — a held row is never asked about", cls.calls)
	}
	if res.Demoted != 2 || len(res.Held) != 2 {
		t.Fatalf("Demoted = %d, Held = %v, want 2 and the two rows that list must agree with the count", res.Demoted, res.Held)
	}
	// Pool order, the same order every other listing on the report uses.
	if res.Held[0].Memory.ID != superseded.ID || res.Held[1].Memory.ID != paired.ID {
		t.Fatalf("Held = %v, want [superseded paired] in pool order", res.Held)
	}
	if got, want := res.Held[0].Reason(), "held by supersedes newer"; got != want {
		t.Errorf("the edge-held row's reason = %q, want %q", got, want)
	}
	if got, want := res.Held[1].Reason(), "held by correction correction"; got != want {
		t.Errorf("the correction-held row's reason = %q, want %q", got, want)
	}
}

// TestHeldMemoryReasonNamesEveryHolder: a note two newer notes both supersede
// is held by BOTH, and withdrawing one of them releases nothing — so naming only
// one would send the operator to withdraw an edge whose withdrawal changes
// nothing. The corrections are the reverse case: a pairing an operator cannot
// withdraw, so the id is the thing to go and read. Every holder is named, the
// supersedes ones first because they are the ones an action exists for.
func TestHeldMemoryReasonNamesEveryHolder(t *testing.T) {
	h := HeldMemory{
		Memory: memory.Memory{ID: "target"},
		Holds: []Hold{
			{Kind: HoldCorrection, Holder: "corr-b"},
			{Kind: HoldSupersedes, Holder: "edge-b"},
			{Kind: HoldCorrection, Holder: "corr-a"},
			{Kind: HoldSupersedes, Holder: "edge-a"},
		},
	}
	want := "held by supersedes edge-a, supersedes edge-b, correction corr-a, correction corr-b"
	if got := h.Reason(); got != want {
		t.Errorf("Reason() = %q, want %q", got, want)
	}
	// Order-independent, so a caller that assembles the list another way cannot
	// change what the report says run to run.
	reordered := HeldMemory{Memory: h.Memory, Holds: []Hold{h.Holds[3], h.Holds[1], h.Holds[2], h.Holds[0]}}
	if got := reordered.Reason(); got != want {
		t.Errorf("Reason() on a reordered Holds = %q, want %q", got, want)
	}
	// A row with no holder has no reason, and must not read as held by nothing
	// in particular — the report never renders one, and this pins that.
	if got := (HeldMemory{Memory: h.Memory}).Reason(); got != "" {
		t.Errorf("Reason() with no holds = %q, want empty", got)
	}
}

// fixedPointFixture is the subject-token bookkeeping the fixed-point tests
// share. Every boundary is counted by hand, because the whole defect is a
// document-frequency one: a pairing exists only while the tokens it shares are
// rare, and "rare" is counted over the pool the NEXT ORDINARY PASS will read.
//
//	palpha, pbravo   4 rows each (Y, B1, C2, B2) — rare in every pool below
//	pcharl           2 rows (Y, C2) — rare in every pool below
//	zephyrone        4 live fillers + A + Y + B1 — never rare in a post-repair
//	                 pool, because A and Y are cleared by this very run. A
//	                 re-check that leaves them out of the count finds it rare and
//	                 holds B1 for a pairing the next pass will not make — the
//	                 row a second, identical pass used to have to release
//	quxdelta         2 live fillers + B2 + Y + C2 — rare only once C2 is held
//	                 back, so holding C2 back CREATES the pairing that holds B2
//	                 and only a second round can see it
//
// The two boundaries point in opposite directions, which is the point. A re-check
// that counted frequency over the live pool (what the pass did before #712) sees
// zephyrone as rare and holds B1. A re-check that counts it over the post-repair
// pool but stops after one round sees quxdelta as common and frees B2, which
// round two then has to hold back again. Only the iterated, post-repair-pool
// answer leaves exactly C2 and B2 held.
type fixedPointFixture struct {
	live        []memory.Memory // the unresolved pool: four fillers
	loaded      []memory.Memory // the resolved pool: A, Y, B1, C2, B2
	liveAfter   []memory.Memory // the unresolved pool this run leaves behind (A, Y and B1 cleared)
	afterLoaded []memory.Memory // the resolved pool it leaves behind (C2 and B2, both held)
	a, y        memory.Memory
	b1, c2, b2  memory.Memory
}

func newFixedPointFixture() fixedPointFixture {
	f1 := memory.Memory{ID: "f1", UpdatedAt: "2026-09-01 00:00:00",
		Content: "nightshift ledgerentry fillerone zephyrone quxdelta"}
	f2 := memory.Memory{ID: "f2", UpdatedAt: "2026-09-02 00:00:00",
		Content: "nightshift ledgerentry fillertwo zephyrone quxdelta"}
	f3 := memory.Memory{ID: "f3", UpdatedAt: "2026-09-03 00:00:00",
		Content: "nightshift ledgerentry fillerthree zephyrone"}
	f4 := memory.Memory{ID: "f4", UpdatedAt: "2026-09-04 00:00:00",
		Content: "nightshift ledgerentry fillerfour zephyrone"}
	// A distinct resolution keyword per row, so no two rows in the repair set
	// share a subject token except the ones the fixture is counting.
	a := memory.Memory{ID: "row-a", Category: "changelog", UpdatedAt: "2026-09-10 00:00:00",
		Content: "postmortem: registry zephyrone rollback sweep"}
	y := memory.Memory{ID: "row-y", Category: "gotcha", UpdatedAt: "2026-09-20 00:00:00",
		Content: "CORRECTION/RESOLUTION: investigation note palpha pbravo pcharl quxdelta zephyrone"}
	b1 := memory.Memory{ID: "row-b1", Category: "changelog", UpdatedAt: "2026-09-11 00:00:00",
		Content: "cost estimate: ledgerstate palpha pbravo zephyrone"}
	c2 := memory.Memory{ID: "row-c2", Category: "changelog", UpdatedAt: "2026-09-12 00:00:00",
		Content: "reference only: palpha pbravo pcharl quxdelta"}
	b2 := memory.Memory{ID: "row-b2", Category: "changelog", UpdatedAt: "2026-09-13 00:00:00",
		Content: "history only: palpha pbravo quxdelta"}
	return fixedPointFixture{
		live:        []memory.Memory{f1, f2, f3, f4},
		loaded:      []memory.Memory{b1, a, b2, c2, y},
		liveAfter:   []memory.Memory{f1, f2, f3, f4, a, b1, y},
		afterLoaded: []memory.Memory{c2, b2},
		a:           a, y: y, b1: b1, c2: c2, b2: b2,
	}
}

// TestReassessHoldBackReachesAFixedPointInOneRun is #712's second observation.
// The hold-back re-check reads the pool the next ordinary pass will see, which
// is the live pool PLUS the rows this run clears, so it cannot be answered once:
// a row held back in round one leaves that pool, which can only make more tokens
// rare, which can reveal a pairing that was invisible a round earlier. One run
// has to iterate to a stop, or a second identical pass clears rows the first one
// left resolved.
//
// The fixture's B1 is the other half of the same defect and needs no iteration:
// before the fix the re-check counted frequency over the live pool alone, where
// zephyrone is rare, so it held B1 for a pairing the next pass will not find.
// A and Y are cleared in this very run, so the pairings that end here is exactly
// what the second pass used to find — and the run after them, asserted below,
// finds nothing more.
func TestReassessHoldBackReachesAFixedPointInOneRun(t *testing.T) {
	f := newFixedPointFixture()
	store := &fakeStore{alreadyResolved: f.loaded, candidates: f.live}
	cls := &fakeClassifier{drop: map[string]bool{}} // KEEP everything

	res, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	// The iteration is over the hold-back decision alone, on verdicts this run
	// already holds: a re-check that re-asked the harness would bill the operator
	// once per round for an answer it already has.
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 — a re-check round must not re-ask the harness", cls.calls)
	}
	if res.Rounds < 2 {
		t.Errorf("res.Rounds = %d, want the re-check to have iterated: round one drops C2 and that is what reveals B2's pairing", res.Rounds)
	}
	if res.BoundHit {
		t.Error("res.BoundHit is true, but this fixture settles well inside the bound")
	}
	held := map[string]string{}
	for _, h := range res.Held {
		held[h.Memory.ID] = h.Reason()
	}
	if len(held) != 2 {
		t.Fatalf("Held = %v, want exactly row-c2 and row-b2", res.Held)
	}
	if got, want := held[f.c2.ID], "held by correction "+f.y.ID; got != want {
		t.Errorf("row-c2's reason = %q, want %q", got, want)
	}
	if got, want := held[f.b2.ID], "held by correction "+f.y.ID; got != want {
		t.Errorf("row-b2's reason = %q, want %q", got, want)
	}
	cleared := map[string]bool{}
	for _, id := range store.cleared {
		cleared[id] = true
	}
	want := map[string]bool{f.a.ID: true, f.y.ID: true, f.b1.ID: true}
	if len(cleared) != len(want) {
		t.Fatalf("cleared = %v, want A, Y and B1", store.cleared)
	}
	for id := range want {
		if !cleared[id] {
			t.Errorf("cleared = %v, want it to include %q", store.cleared, id)
		}
	}
	// A held row is not repaired, so a KEEP cache entry on it is state no
	// decision produced.
	for id := range held {
		if _, ok := store.kept[id]; ok {
			t.Errorf("held row %q was KEEP-cached: %v", id, store.kept)
		}
	}
	// The next pass, on the store this run leaves behind: the fixed point. Its
	// own up-front floor finds C2 and B2 held by Y — Y is live, and the pool it
	// now counts over is the one this run settled — so it asks the harness about
	// nothing and clears nothing. Asserting THAT, rather than a second pass
	// finding what this one left, is what makes "one run" a measured claim: a
	// second identical pass clearing more is the defect, so a test that asserted
	// the second pass finding more would pass on the broken behaviour and pin
	// nothing.
	after := &fakeStore{alreadyResolved: f.afterLoaded, candidates: f.liveAfter}
	afterRes, afterKept, err := Reassess(context.Background(), after, &fakeClassifier{drop: map[string]bool{}}, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess second pass: %v", err)
	}
	if len(after.cleared) != 0 || len(afterKept) != 0 {
		t.Errorf("the pass after the fixed point cleared %v and would clear %v, want nothing — this run already reached it",
			after.cleared, ids(afterKept))
	}
	stillHeld := map[string]string{}
	for _, h := range afterRes.Held {
		stillHeld[h.Memory.ID] = h.Reason()
	}
	if len(stillHeld) != 2 || stillHeld[f.c2.ID] != "held by correction "+f.y.ID || stillHeld[f.b2.ID] != "held by correction "+f.y.ID {
		t.Errorf("the pass after the fixed point held %v, want C2 and B2 still held by the correction", stillHeld)
	}
}

// TestHoldBackReachesItsBoundAndSaysSo drives the bound at its PRODUCTION value,
// because the other bound test proves the signal by handing holdBack a bound of 1
// — which shows the signal exists, not that a real pass can reach it.
//
// The fixture is a chain of rows, each held only after the row before it left
// the pool, so a chain longer than the bound cannot settle in one run. Every
// pairing token is carried by exactly two repair rows — the row that will be
// held and the row it holds — so their document frequency crosses from common
// to rare exactly when the earlier of the two leaves the pool.
//
// The assertion that matters is the PAIR of them: the bound reached AND held
// rows fewer than the chain is deep. A bound that reported a clean stop would be
// indistinguishable from convergence, and the operator would read a repair as
// finished that is not.
func TestHoldBackReachesItsBoundAndSaysSo(t *testing.T) {
	// Deep enough that the unwind needs one round MORE than the bound allows: a
	// round settles about two holds here, so the production bound of 8 stops
	// part way down and leaves rows standing. The assertion at the end is what
	// makes the fixture's depth a proof rather than a guess — the same decision
	// with one more round of headroom converges.
	const depth = 16
	f := newHoldChainFixture(depth)
	store := &fakeStore{alreadyResolved: f.loaded, candidates: f.live}
	cls := &fakeClassifier{drop: map[string]bool{}} // KEEP everything

	res, reKept, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if !res.BoundHit {
		t.Fatalf("BoundHit is false on a %d-deep chain: rounds=%d held=%d", depth, res.Rounds, len(res.Held))
	}
	if res.Rounds != maxHoldBackRounds {
		t.Errorf("Rounds = %d, want the bound itself (%d)", res.Rounds, maxHoldBackRounds)
	}
	// A bound that reported a clean stop would be indistinguishable from
	// convergence, so the round count and the flag are asserted together and
	// the rounds are pinned to the bound itself.
	if len(res.Held) == 0 {
		t.Error("no row was held, so the bound was reached for some other reason")
	}
	// A held row is not repaired, so it must not be KEEP-cached either — the
	// cache records a decision the pass did not act on, and the next pass would
	// read it as one it could act on.
	for _, h := range res.Held {
		if _, ok := store.kept[h.Memory.ID]; ok {
			t.Errorf("held row %q was KEEP-cached: %v", h.Memory.ID, store.kept)
		}
	}
	// Every held row names the correction that held it, including the last one
	// the bound did reach: a bound does not degrade the reasons.
	for _, h := range res.Held {
		if got, want := h.Reason(), "held by correction "+f.c.ID; got != want {
			t.Errorf("held row %q's reason = %q, want %q", h.Memory.ID, got, want)
		}
	}
	// The rows the bound never reached are CLEARED, not held: the loop exits
	// with them in the repair set, and holding them instead would report rows
	// nothing asserts. A further pass finds them again if the next ordinary pass
	// really does assert them, which is what "a further pass may clear more"
	// tells the operator to check.
	if len(reKept) == 0 {
		t.Errorf("the repaired set is empty: the bound must leave the rows it never reached in it, not hold them")
	}
	// A dry run has to reach the same bound as the run that applies it, or the
	// preview is not a preview of this repair.
	dry, dryKept, err := Reassess(context.Background(),
		&fakeStore{alreadyResolved: f.loaded, candidates: f.live}, &fakeClassifier{}, "proj", false, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess dry: %v", err)
	}
	if !dry.BoundHit || dry.Rounds != res.Rounds {
		t.Errorf("the dry run hit the bound at round %d (hit %v), --apply at %d (hit %v)",
			dry.Rounds, dry.BoundHit, res.Rounds, res.BoundHit)
	}
	if !eqStrings(ids(dryKept), ids(reKept)) {
		t.Errorf("the dry run would clear %v, --apply cleared %v", ids(dryKept), ids(reKept))
	}
	// The bound is what stopped the unwind, and the way to show that is to give
	// the SAME decision, over the same pools, one round more of headroom: it
	// converges, and on the SAME holds. Without that, a re-check that silently
	// mis-settled the deep end of the chain and stopped there would look exactly
	// like one that ran out of rounds, and the report's "a further pass may clear
	// more" would be the only thing standing between the operator and a wrong
	// repair.
	roomier := holdBack(f.loaded, f.live, maxHoldBackRounds+1)
	if roomier.boundHit {
		t.Fatal("one round more of headroom still hit the bound, so the fixture is not deep enough to prove the bound bit")
	}
	if len(roomier.holds) != len(res.Held) {
		t.Errorf("with one round more of headroom the same decision holds %d row(s), the bound held %d: the two disagree, so the bound changed the answer rather than deferring a round",
			len(roomier.holds), len(res.Held))
	}
}

// holdChainFixture is the chain above. Each row carries its own three subject
// tokens and its successor's, so holding row i removes a carrier of row i+1's
// tokens as well: a round settles more than one hold, and the chain is only
// partly unwound before the bound stops the pass. That is deliberate — the
// question this fixture asks is whether the bound is REACHED and reported, not
// how many rows a round of a given depth settles.
type holdChainFixture struct {
	live   []memory.Memory
	loaded []memory.Memory
	c      memory.Memory // the one correction, and the only assertor
}

// holdChainTokens names the three subject tokens row i shares with the
// correction. Each row carries its own and its successor's, which is what makes
// the chain: holding row i removes a carrier of row i+1's tokens too.
func holdChainTokens(i int) []string {
	return []string{fmt.Sprintf("chana%02d", i), fmt.Sprintf("chanb%02d", i), fmt.Sprintf("chanc%02d", i)}
}

func newHoldChainFixture(n int) holdChainFixture {
	var tokens []string
	for i := 1; i <= n+1; i++ {
		tokens = append(tokens, holdChainTokens(i)...)
	}
	// Two live fillers and no more. A token shared by the correction and TWO
	// repair rows therefore sits at frequency 4, which is the ceiling, so it is
	// rare — and holding either repair row drops it to 3, keeping it rare. The
	// chain's depth comes from how many rows share a token set, not from
	// frequency arithmetic: each held row is the only thing standing between its
	// successor and the next hold, so the re-check has n-1 holds to settle and
	// the bound at maxHoldBackRounds cannot reach the bottom of a 20-deep one.
	live := []memory.Memory{
		{ID: "chain-f1", Category: "changelog", UpdatedAt: "2026-08-01 00:00:00",
			Content: "nightshift ledgerentry chainfillerone " + strings.Join(tokens, " ")},
		{ID: "chain-f2", Category: "changelog", UpdatedAt: "2026-08-02 00:00:00",
			Content: "nightshift ledgerentry chainfillertwo " + strings.Join(tokens, " ")},
	}
	loaded := []memory.Memory{{
		ID: "chain-correction", Category: "gotcha", UpdatedAt: "2026-10-01 00:00:00",
		Content: "CORRECTION/RESOLUTION: investigation note " + strings.Join(tokens, " "),
	}}
	for i := 1; i <= n; i++ {
		// "postmortem" is a resolution keyword, so every row is a pairing
		// TARGET, and it is shared by all of them, so it counts for nothing: at
		// nine rows it is far past the rare-token ceiling throughout.
		own := strings.Join(holdChainTokens(i), " ")
		loaded = append(loaded, memory.Memory{
			ID:        fmt.Sprintf("chain-e%02d", i),
			Category:  "changelog",
			UpdatedAt: fmt.Sprintf("2026-09-%02d 00:00:00", i),
			Content:   "postmortem: " + own + " " + strings.Join(holdChainTokens(i+1), " "),
		})
	}
	return holdChainFixture{live: live, loaded: loaded, c: loaded[0]}
}

// TestHoldBackBoundIsReportedWhenHit: the re-check is bounded, and a bound that
// stops it with a row still changing is not a quiet truncation — the repair is
// short of its fixed point and a further pass may clear more. That is a sentence
// the report has to be able to make, so the signal is computed here, on the same
// fixture, one round short of the answer.
func TestHoldBackBoundIsReportedWhenHit(t *testing.T) {
	f := newFixedPointFixture()

	full := holdBack(f.loaded, f.live, maxHoldBackRounds)
	if full.boundHit {
		t.Error("the fixture settles inside the bound, so boundHit must be false")
	}
	if full.rounds != 3 {
		t.Errorf("rounds = %d, want 3: two rounds that drop a row and the round that finds nothing", full.rounds)
	}
	if len(full.holds) != 2 {
		t.Fatalf("holds = %v, want row-c2 and row-b2", full.holds)
	}
	if sortedHeldIDs(full.kept) != "row-a,row-b1,row-y" {
		t.Errorf("kept = %v, want [row-a row-b1 row-y]", full.kept)
	}

	// One round is the mutation this test exists for: it cannot see the pairing
	// that dropping row-c2 reveals, so it frees a row the fixed point holds.
	one := holdBack(f.loaded, f.live, 1)
	if !one.boundHit || one.rounds != 1 {
		t.Errorf("one round = %d round(s), boundHit %v, want 1 and true — the round still dropped a row", one.rounds, one.boundHit)
	}
	if contains(full.kept, "row-c2") || contains(full.kept, "row-b2") {
		t.Errorf("kept = %v, want the fixed point to hold both rows back", full.kept)
	}
	if len(one.holds) != 1 || one.holds["row-c2"][0].Holder != f.y.ID {
		t.Errorf("one round's holds = %v, want only row-c2, held by the correction", one.holds)
	}
	if !contains(one.kept, "row-b2") {
		t.Errorf("one round kept %v, want the row the second round holds back to be freed", one.kept)
	}
}

// TestReassessDryRunMatchesApplyAndWritesNothing: the fixed point is a property
// of the DECISION, not of whether anything was written, so the preview has to
// name the same held rows and spend the same rounds as the run that applies it.
// A dry run that reported a smaller repair than --apply would reach is a
// preview an operator cannot plan from, and one that wrote anything to find out
// is not a preview.
func TestReassessDryRunMatchesApplyAndWritesNothing(t *testing.T) {
	f := newFixedPointFixture()
	dryStore := &fakeStore{alreadyResolved: f.loaded, candidates: f.live}
	dry, dryKept, err := Reassess(context.Background(), dryStore, &fakeClassifier{}, "proj", false, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess dry: %v", err)
	}
	if len(dryStore.cleared) != 0 || len(dryStore.kept) != 0 || len(dryStore.markedKept) != 0 {
		t.Errorf("the dry run wrote: cleared %v, cache %v, marked %v", dryStore.cleared, dryStore.kept, dryStore.markedKept)
	}

	applyStore := &fakeStore{alreadyResolved: f.loaded, candidates: f.live}
	applied, appliedKept, err := Reassess(context.Background(), applyStore, &fakeClassifier{}, "proj", true, Scope{}, nil)
	if err != nil {
		t.Fatalf("Reassess apply: %v", err)
	}
	if dry.Rounds != applied.Rounds || dry.BoundHit != applied.BoundHit {
		t.Errorf("dry re-checked %d round(s) (boundHit %v) and --apply re-checked %d (boundHit %v), want the same fixed point",
			dry.Rounds, dry.BoundHit, applied.Rounds, applied.BoundHit)
	}
	if !eqStrings(ids(dryKept), ids(appliedKept)) {
		t.Errorf("dry would clear %v, --apply cleared %v; the preview must report the repair the run reaches", ids(dryKept), ids(appliedKept))
	}
	if dry.ReKept != applied.ReKept || dry.Demoted != applied.Demoted {
		t.Errorf("dry reported %d re-KEEP / %d held, --apply %d / %d", dry.ReKept, dry.Demoted, applied.ReKept, applied.Demoted)
	}
	if len(dry.Held) != len(applied.Held) {
		t.Fatalf("dry listed %v, --apply listed %v", dry.Held, applied.Held)
	}
	for i := range dry.Held {
		if dry.Held[i].Memory.ID != applied.Held[i].Memory.ID || dry.Held[i].Reason() != applied.Held[i].Reason() {
			t.Errorf("held row %d differs: dry %q/%q, apply %q/%q", i,
				dry.Held[i].Memory.ID, dry.Held[i].Reason(), applied.Held[i].Memory.ID, applied.Held[i].Reason())
		}
	}
	if len(applyStore.cleared) != len(appliedKept) {
		t.Errorf("--apply cleared %v, want the %d row(s) it reported", applyStore.cleared, len(appliedKept))
	}
}

func ids(mems []memory.Memory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.ID
	}
	return out
}

func contains(mems []memory.Memory, id string) bool {
	for _, m := range mems {
		if m.ID == id {
			return true
		}
	}
	return false
}

func sortedHeldIDs(mems []memory.Memory) string {
	out := ids(mems)
	sort.Strings(out)
	joined := ""
	for i, id := range out {
		if i > 0 {
			joined += ","
		}
		joined += id
	}
	return joined
}
