package supersede

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// is still asserted, so a second live edge holds it down.
func seedTwoEdges(t *testing.T, store *memory.Store, db *sql.DB) (a, b, target string) {
	t.Helper()
	ctx := context.Background()
	a, target = seedEdge(t, store, db,
		"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
		"The restore path on one spindle is safe and takes under a minute.")
	b = add(t, store, db, "A restore across two spindles now streams the row count, so it needs neither a copy nor a pause.", []float32{1, 0, 0, 0}, "2026-07-01 00:00:00")
	if err := store.CreateLink(ctx, b, target, string(RelationSupersedes), 0.93, "llm"); err != nil {
		t.Fatal(err)
	}
	return a, b, target
}

func liveEdgeCount(t *testing.T, store *memory.Store, target string) int {
	t.Helper()
	links, err := store.LinksInto(context.Background(), "p", target, "supersedes")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	return len(links)
}

// through the empty-relation read a withdrawal uses. It is the #833 counter: a
// a fixture that seeds one has to be able to see it leave.
func liveLinkCount(t *testing.T, store *memory.Store, target string) int {
	t.Helper()
	links, err := store.LinksInto(context.Background(), "p", target, "")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	return len(links)
}

func liveRelationCount(t *testing.T, store *memory.Store, target, relation string) int {
	t.Helper()
	links, err := store.LinksInto(context.Background(), "p", target, relation)
	if err != nil {
		t.Fatalf("LinksInto(%s): %v", relation, err)
	}
	return len(links)
}

// and both endpoints share created_at AND updated_at, which is the #778 tie: the
func seedCausesEdge(t *testing.T, store *memory.Store, db *sql.DB) (cause, effect string) {
	t.Helper()
	ctx := context.Background()
	const stamp = "2026-06-01 00:00:00"
	cause = add(t, store, db, "The migration script left the lock table populated.", []float32{1, 0, 0, 0}, stamp)
	effect = add(t, store, db, "The replica fell behind by four hours after the migration.", []float32{1, 0, 0, 0}, stamp)
	if err := store.CreateLink(ctx, cause, effect, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	return cause, effect
}

// pinID writes a memory under a chosen id, so a fixture can make two of them
func pinID(t *testing.T, db *sql.DB, id, content string) string {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO memories (id, project_id, category, content, importance, source)
		VALUES (?, 'p', 'fact', ?, 0.5, 'mcp')
	`, id, content); err != nil {
		t.Fatalf("pin id %s: %v", id, err)
	}
	return id
}

// mustCreatePlain stores a memory through the store, in project "p".
func mustCreatePlain(t *testing.T, store *memory.Store, content string) string {
	t.Helper()
	id, err := store.Create(context.Background(), "p", memory.Memory{Category: "fact", Content: content, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// TestWithdrawResolvesPrefixesAndFullIDs: the operator reads an id out of a
// that says which edge is wrong.
func TestWithdrawResolvesPrefixesAndFullIDs(t *testing.T) {
	store, db := seed(t)
	a, b, target := seedTwoEdges(t, store, db)

	for _, tc := range []struct {
		name        string
		source, tgt string
	}{
		{name: "full ids", source: a, tgt: target},
		{name: "eight-character prefixes", source: a[:8], tgt: target[:8]},
		{name: "one full, one prefixed", source: a, tgt: target[:16]},
		{name: "uppercase prefix", source: strings.ToUpper(a[:8]), tgt: target},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Withdraw(context.Background(), store, "p", []WithdrawPair{{Source: tc.source, Target: tc.tgt}}, false, discardLogger())
			if err != nil {
				t.Fatalf("Withdraw: %v", err)
			}
			if len(res.Links) != 1 {
				t.Fatalf("links = %+v, want exactly the one edge", res.Links)
			}
			if res.Links[0].SourceID != a || res.Links[0].TargetID != target {
				t.Errorf("resolved to %s→%s, want %s→%s", res.Links[0].SourceID, res.Links[0].TargetID, a, target)
			}
			if res.Withdrawn != 0 || res.Links[0].Withdrawn {
				t.Error("a dry run reported a withdrawal it did not make")
			}
		})
	}
	if liveEdgeCount(t, store, target) != 2 {
		t.Errorf("live edges = %d, want 2: the dry runs above must have written nothing", liveEdgeCount(t, store, target))
	}
	_ = b
}

// TestWithdrawRefusesAnAmbiguousPrefix: a prefix naming two memories is a
// characters, and nothing is written.

// TestWithdrawRefusesARefItCannotResolve: a ref that names nothing is a refusal,

// TestWithdrawNamesAnImportedID: `ghost import` writes an artifact's ids
// by telling the operator their full id was not a full id.

// TestWithdrawDryRunWritesNothing: the preview is the decision an operator makes
// so is not there.

// TestWithdrawApplyInvalidatesAndRecordsTheWithdrawal: apply is the ordinary

// TestWithdrawRefusesAPairWithNoLiveLink: a pair with no live edge is an error,
func TestWithdrawRefusesAPairWithNoLiveLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)

	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: target, Target: b}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw accepted a pair with no live supersedes link")
	}
	if !strings.Contains(err.Error(), "supersedes") {
		t.Errorf("the refusal does not name the relation it looked for: %v", err)
	}
	if got := liveEdgeCount(t, store, target); got != 2 {
		t.Errorf("live edges = %d, want 2: a refused pair must write nothing", got)
	}

	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
	_, err = Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("re-withdrawing a withdrawn edge reported success")
	}
	// would be one the read has not established (#786).
	if !strings.Contains(err.Error(), "still superseded by "+b[:8]) {
		t.Errorf("the refusal does not name the target's live edges: %v", err)
	}
	if got := liveEdgeCount(t, store, target); got != 1 {
		t.Errorf("live edges = %d, want 1", got)
	}
}

// TestWithdrawChangesNothingWhenAnyPairIsWrong: the contract for a batch. One
func TestWithdrawChangesNothingWhenAnyPairIsWrong(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)

	_, err := Withdraw(ctx, store, "p", []WithdrawPair{
		{Source: a, Target: target},
		{Source: b, Target: "ffffffffffffffffffffffffffffffff"},
	}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw accepted a batch with a pair that names no live edge")
	}
	if got := liveEdgeCount(t, store, target); got != 2 {
		t.Errorf("live edges = %d, want 2: one bad pair must withdraw nothing", got)
	}
	if hasUnsupersedeHistory(t, store, target) {
		t.Error("a refused batch wrote the unsupersede history row")
	}
}

// TestWithdrawWithNoPairs: nothing to withdraw is a usage error, not a report
// is a claim about the graph.

// TestWithdrawRefusesASelfPair: both refs resolving to one memory is a pair that
// reporting it as a missing edge.
func TestWithdrawRefusesASelfPair(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, _, target := seedTwoEdges(t, store, db)

	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: target, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw accepted a memory superseding itself")
	}
	if !strings.Contains(err.Error(), "itself") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if got := liveEdgeCount(t, store, target); got != 2 {
		t.Errorf("live edges = %d, want 2", got)
	}
	_ = a
}

// TestWithdrawReachesACausesEdgeByName is #833 at the store-independent layer.
// A 'causes' edge is load-bearing since #823 — its direction decides which way a
// repair at all. The fixture is the #778 TIE on purpose: both endpoints share
// that can settle it.
func TestWithdrawReachesACausesEdgeByName(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	cause, effect := seedCausesEdge(t, store, db)

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: cause, Target: effect}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if res.Resolved != 1 || res.Withdrawn != 1 {
		t.Fatalf("resolved=%d withdrawn=%d, want 1 and 1", res.Resolved, res.Withdrawn)
	}
	// be naming an edge the operator never named.
	if got := res.Links[0].Relation; got != string(RelationCauses) {
		t.Errorf("Relation = %q, want %q", got, RelationCauses)
	}
	if got := liveLinkCount(t, store, effect); got != 0 {
		t.Errorf("live edges into the effect = %d, want 0: the named edge is still there", got)
	}
	// row that would say otherwise.
	if hasUnsupersedeHistory(t, store, effect) {
		t.Error("a 'causes' withdrawal wrote the unsupersede history row; that claim never held its target down")
	}
}

// TestWithdrawPrefersSupersedesWhenAPairHoldsBoth: the default. A pair holding
// existing call site and every pre-#833 report meant, and the one whose removal
// actually un-hides a memory.
func TestWithdrawPrefersSupersedesWhenAPairHoldsBoth(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	cause, effect := seedCausesEdge(t, store, db)
	// is the case the default exists for.
	if err := store.CreateLink(ctx, cause, effect, string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: cause, Target: effect}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if got := res.Links[0].Relation; got != string(RelationSupersedes) {
		t.Fatalf("Relation = %q, want %q: 'supersedes' is the default", got, RelationSupersedes)
	}
	if got := liveRelationCount(t, store, effect, string(RelationCauses)); got != 1 {
		t.Errorf("live 'causes' edges = %d, want 1: only the default relation is withdrawn", got)
	}
	if got := liveRelationCount(t, store, effect, string(RelationSupersedes)); got != 0 {
		t.Errorf("live 'supersedes' edges = %d, want 0", got)
	}
}

// TestWithdrawHonoursAnExplicitRelation: the flag is what makes the pair holding
func TestWithdrawHonoursAnExplicitRelation(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	cause, effect := seedCausesEdge(t, store, db)
	if err := store.CreateLink(ctx, cause, effect, string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: cause, Target: effect, Relation: string(RelationCauses)}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if got := res.Links[0].Relation; got != string(RelationCauses) {
		t.Errorf("Relation = %q, want %q: the pair's own relation decides", got, RelationCauses)
	}
	if got := liveRelationCount(t, store, effect, string(RelationCauses)); got != 0 {
		t.Errorf("live 'causes' edges = %d, want 0", got)
	}
	if got := liveRelationCount(t, store, effect, string(RelationSupersedes)); got != 1 {
		t.Errorf("live 'supersedes' edges = %d, want 1: the pinned relation is the only one acted on", got)
	}
}

// TestWithdrawRefusesTheOtherRelationItWasPinnedAwayFrom: pinning is a filter,
// withdraws an edge they had just excluded.
func TestWithdrawRefusesTheOtherRelationItWasPinnedAwayFrom(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	cause, effect := seedCausesEdge(t, store, db)

	_, err := Withdraw(ctx, store, "p",
		[]WithdrawPair{{Source: cause, Target: effect, Relation: string(RelationSupersedes)}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw fell back to the other relation instead of refusing the pinned one")
	}
	if !strings.Contains(err.Error(), "no live supersedes link") {
		t.Errorf("the refusal does not name the relation it searched for: %v", err)
	}
	// an answer rather than a dead end.
	if !strings.Contains(err.Error(), short(effect)[:8]) {
		t.Errorf("the refusal does not name the target's live edges: %v", err)
	}
	if got := liveLinkCount(t, store, effect); got != 1 {
		t.Errorf("live edges into the effect = %d, want 1: a refused pair must write nothing", got)
	}
}

// TestWithdrawRefusalNamesBothRelations: the refusal's whole job is to be the
// only the one it looked for — the #833 report, from the other side.
func TestWithdrawRefusalNamesBothRelations(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	cause, effect := seedCausesEdge(t, store, db)
	holder := add(t, store, db, "A second note asserts the same replacement of the effect.", []float32{0, 1, 0, 0}, "2026-07-01 00:00:00")
	if err := store.CreateLink(ctx, holder, effect, string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	_, err := Withdraw(ctx, store, "p",
		[]WithdrawPair{{Source: add(t, store, db, "A note with no claim on the effect at all.", []float32{0, 0, 1, 0}, "2026-08-01 00:00:00"), Target: effect}},
		false, discardLogger())
	if err == nil {
		t.Fatal("Withdraw accepted a pair with no live edge")
	}
	for _, want := range []string{short(cause)[:8], short(holder)[:8]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits the holder %s, which points at the target in the other relation: %v", want, err)
		}
	}
}

// TestIntoSuffixNamesTheRelationWithoutClaimingItBuries pins the refusal's whole
// of them was a false claim.
//
// by C" is accurate, "that edge still buries it" is not.
func TestIntoSuffixNamesTheRelationWithoutClaimingItBuries(t *testing.T) {
	const id = "A1B2C3D4E5F60718293A4B5C6D7E8F90"
	sup := []memory.Link{{SourceID: id, Relation: string(RelationSupersedes)}}
	causes := []memory.Link{{SourceID: id, Relation: string(RelationCauses)}}
	both := []memory.Link{{SourceID: id, Relation: string(RelationSupersedes)}, {SourceID: id, Relation: string(RelationCauses)}}

	for _, c := range []struct {
		name  string
		links []memory.Link
		want  string
	}{
		{"a supersedes holder buries the target", sup, "still superseded by A1B2C3D4"},
		{"a causes holder does not", causes, "still caused by A1B2C3D4"},
		{"both relations are named as such", both, "still linked (superseded or caused) by A1B2C3D4"},
		// read returns since #833; a suffix that printed it twice reads as two
		{"one holder, one id, both relations", both, "by A1B2C3D4 —"},
		{"no holder is scoped to the project", nil, "no memory in this project links it"},
		{"an empty holder set is the same sentence", []memory.Link{}, "no memory in this project links it"},
	} {
		got := intoSuffix(c.links)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: intoSuffix = %q, want it to contain %q", c.name, got, c.want)
		}
	}

	for _, links := range [][]memory.Link{sup, both} {
		if got := intoSuffix(links); !strings.Contains(got, "still buries it") {
			t.Errorf("intoSuffix(%v) = %q, and a live 'supersedes' edge among the holders means the target IS still held down", links, got)
		}
	}
	if got := intoSuffix(causes); strings.Contains(got, "still buries it") {
		t.Errorf("intoSuffix(causes-only) = %q claims an edge still buries the target, and a 'causes' edge buries nothing", got)
	}
	for _, c := range []struct {
		name  string
		links []memory.Link
	}{
		{"both relations on one source", both},
		{"the same id three times", []memory.Link{{SourceID: id, Relation: string(RelationCauses)}, {SourceID: id, Relation: string(RelationSupersedes)}, {SourceID: id, Relation: string(RelationCauses)}}},
	} {
		got := intoSuffix(c.links)
		if n := strings.Count(got, "A1B2C3D4"); n != 1 {
			t.Errorf("%s: intoSuffix names the holder %d time(s), want 1:\n%q", c.name, n, got)
		}
	}

	// not left with a bare list of ids and no guidance.
	if got := intoSuffix(causes); !strings.Contains(got, "does not bury its target") {
		t.Errorf("a 'causes'-only suffix does not say the target is not held down: %q", got)
	}
	// And the empty case must not claim a relation it has none of: before #833 it
	// whose only edge is a 'causes' one.
	if got := intoSuffix(nil); strings.Contains(got, "supersedes it") {
		t.Errorf("the empty suffix = %q, which claims a 'supersedes' edge it cannot see", got)
	}
}

// TestWithdrawLeavesAnotherProjectsEdgeAlone: the edge is found through the
// withdrawal from being a way around a project boundary.
func TestWithdrawLeavesAnotherProjectsEdgeAlone(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "q", "/tmp/q", "q"); err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(ctx, "q", memory.Memory{Category: "fact", Content: "Another project's note was reworded and now stands alone.", Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.Create(ctx, "q", memory.Memory{Category: "fact", Content: "Another project's earlier claim.", Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	_, err = Withdraw(ctx, store, "p", []WithdrawPair{{Source: newer, Target: older}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw reached an edge owned by another project")
	}
	if !strings.Contains(err.Error(), "no memory in project p") {
		t.Errorf("the refusal is not the project-scoped ref resolution: %v", err)
	}
	links, err := store.LinksInto(ctx, "q", older, "supersedes")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("the other project's edge = %d live row(s), want 1: it must be untouched", len(links))
	}
}

// TestWithdrawStillRefusesAnEdgeNoProjectClaims there pin.

// TestWithdrawChainsIntoResolveReassess is the step that makes a withdrawal
func TestWithdrawChainsIntoResolveReassess(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)
	if n, err := store.SetResolved(ctx, []string{target}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, err)
	}

	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	// dry run answers that without applying it.
	held := &keepVerdictClassifier{}
	res, reKept, err := resolve.Reassess(ctx, store, held, "p", false, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (one edge left): %v", err)
	}
	if res.Demoted != 1 || res.Cleared != 0 || len(reKept) != 0 {
		t.Errorf("with one live edge left: asserted=%d cleared=%d reKept=%d, want 1, 0, 0 — the surviving edge is still resolve's floor",
			res.Demoted, res.Cleared, len(reKept))
	}
	if !isResolved(t, store, target) {
		t.Fatal("the target is not resolved, so the chain has nothing to clear")
	}

	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: b, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("Withdraw (second): %v", err)
	}
	keep := &keepVerdictClassifier{}
	after, reKept, err := resolve.Reassess(ctx, store, keep, "p", false, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (after): %v", err)
	}
	if after.Demoted != 0 {
		t.Errorf("after the last withdrawal: asserted=%d, want 0 — a withdrawn edge asserts nothing", after.Demoted)
	}
	if len(reKept) != 1 {
		t.Errorf("after the last withdrawal: reKept=%d, want 1 — nothing holds the target down now", len(reKept))
	}
	if after.Cleared != 0 {
		t.Errorf("a dry run cleared %d row(s)", after.Cleared)
	}
	if keep.calls != 1 {
		t.Errorf("the repair pass made %d classify call(s), want 1: with every edge gone the row is a question again", keep.calls)
	}
	if reKept[0].ID != target {
		t.Errorf("reKept = %s, want the withdrawn edge's target %s", reKept[0].ID, target)
	}
	if !isResolved(t, store, target) {
		t.Fatal("the dry runs above applied something: the target is no longer stamped, so the apply below has nothing to clear")
	}
	assertUnsupersedeHistory(t, store, target)

	// (#698 measured an unscoped repair proposing to un-hide 143 rows, ~35% of
	bystander := mustCreatePlain(t, store, "A changelog note nobody superseded, already resolved by an earlier pass.")
	if n, err := store.SetResolved(ctx, []string{bystander}); err != nil || n != 1 {
		t.Fatalf("SetResolved(bystander) = %d, %v; want 1 and no error", n, err)
	}
	scoped := &keepVerdictClassifier{}
	final, reKept, err := resolve.Reassess(ctx, store, scoped, "p", true,
		resolve.Scope{Only: []string{target}}, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (scoped): %v", err)
	}
	if final.Cleared != 1 || len(reKept) != 1 || reKept[0].ID != target {
		t.Errorf("the scoped repair cleared=%d reKept=%+v, want the withdrawn target %s", final.Cleared, reKept, target)
	}
	if !isResolved(t, store, bystander) {
		t.Error("the scoped repair un-hid a memory no withdrawn edge pointed at — the scope did not hold")
	}
}

// TestWithdrawMarksTheRowsAFailedWriteNeverReached: a partial repair has to be
// not happen, on rows the run never reached.
// The failingStore above is #688's, reused here: it is the same seam this needs —
// be two names for one fake.
func TestWithdrawMarksTheRowsAFailedWriteNeverReached(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)
	third := add(t, store, db, "A third note, superseded by a fourth the pass also proposes.", []float32{1, 0, 0, 0}, "2026-08-01 00:00:00")
	fourth := add(t, store, db, "A fourth note that supersedes the third one only briefly.", []float32{0, 1, 0, 0}, "2026-02-01 00:00:00")
	if err := store.CreateLink(ctx, third, fourth, string(RelationSupersedes), 0.91, "llm"); err != nil {
		t.Fatal(err)
	}

	res, err := Withdraw(ctx, &failingStore{Store: store, failAt: 2}, "p", []WithdrawPair{
		{Source: a, Target: target},
		{Source: b, Target: target},
		{Source: third, Target: fourth},
	}, true, discardLogger())
	if err == nil {
		t.Fatal("a failed write reported success")
	}
	if res.Withdrawn != 1 {
		t.Errorf("withdrawn = %d, want 1: only the first write landed", res.Withdrawn)
	}
	if len(res.Links) != 3 {
		t.Fatalf("links = %d, want all 3 reported", len(res.Links))
	}
	if !res.Links[0].Withdrawn {
		t.Error("the row whose write landed is not marked withdrawn")
	}
	if !res.Links[1].WithdrawalFailed {
		t.Error("the row whose write failed is not marked FAILED")
	}
	if res.Links[1].Withdrawn {
		t.Error("the failed row claims a withdrawal that did not happen")
	}
	if !res.Links[2].NotAttempted {
		t.Error("the row after the failure is not marked as never attempted")
	}
	if res.Links[2].Withdrawn {
		t.Error("a row this run never reached claims a withdrawal")
	}
	if n := liveEdgeCount(t, store, fourth); n != 1 {
		t.Errorf("live edges into the third target = %d, want 1: it was never reached", n)
	}
}

// TestWithdrawRefusesAShortRefWithoutDumpingTheProject: a ref shorter than the
// says the ref is too short and prints no ids at all.

// TestWithdrawPrefersTheSpellingThatWasTyped: the store matches case-insensitively

// TestWithdrawRefusesAThirdCasingOfCaseVariantIDs: a ref spelled as NEITHER of two
// that may have no shell.

// TestWithdrawResolvesAnExactShortIDThatPrefixesALongerOne: the floor applies to
// imported id unnameable by prefix collision.
func TestWithdrawResolvesAnExactShortIDThatPrefixesALongerOne(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	exact := pinID(t, db, "abc", "An imported note with a three character id.")
	pinID(t, db, "abcdef", "An imported note whose id extends the other one.")
	if err := store.CreateLink(ctx, exact, pinID(t, db, "abcd", "The note the three character id supersedes."), string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: "abc", Target: "abcd"}}, true, discardLogger())
	if err != nil {
		t.Fatalf("an exact full id was not resolved: %v", err)
	}
	if res.Withdrawn != 1 || res.Links[0].SourceID != exact {
		t.Errorf("withdrawn=%d links=%+v, want the exact-id edge withdrawn", res.Withdrawn, res.Links)
	}
}

// hasUnsupersedeHistory reports whether the memory carries an `unsupersede` row.
func hasUnsupersedeHistory(t *testing.T, store *memory.Store, id string) bool {
	t.Helper()
	entries, err := store.MemoryHistory(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	for _, e := range entries {
		if e.Phase == "unsupersede" {
			return true
		}
	}
	return false
}

// prevent, inverted.
