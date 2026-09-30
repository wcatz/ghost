package supersede

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// seedTwoEdges writes one live 'supersedes'/'llm' edge a→t and a second,
// independent replacement b→t over the same target, and returns the three ids.
// The pair exists for the case a single withdrawal must NOT un-bury: the target
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

// liveEdgeCount reports how many live 'supersedes' edges still point at target.
func liveEdgeCount(t *testing.T, store *memory.Store, target string) int {
	t.Helper()
	links, err := store.SupersedesLinksInto(context.Background(), "p", target)
	if err != nil {
		t.Fatalf("SupersedesLinksInto: %v", err)
	}
	return len(links)
}

// pinID writes a memory under a chosen id, so a fixture can make two of them
// share a prefix. It writes the row directly because every id-derived table —
// memory_links, memory_embeddings, memory_provenance — references memories(id),
// so an id cannot be changed after the fact, only chosen up front.
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
// report, and every Ghost report shortens it to eight characters. A withdrawal
// that only accepted a full 32-character id could not be driven from the report
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
			// A dry run resolves without writing, so the resolution is what is
			// under test here; a later test pins that apply moves the edge.
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
// question with two answers, and the edge it would have withdrawn is not
// something to guess at. The refusal lists the matches so the operator can add
// characters, and nothing is written.
func TestWithdrawRefusesAnAmbiguousPrefix(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, _, target := seedTwoEdges(t, store, db)
	// Two memories sharing an eight-character prefix, which random ids never do
	// on their own — the shortest ref length a withdrawal accepts.
	first := "aaaaaaaa" + "1" + strings.Repeat("0", 23)
	second := "aaaaaaaa" + "2" + strings.Repeat("0", 23)
	pinID(t, db, first, "A note whose id was pinned so a prefix is ambiguous.")
	pinID(t, db, second, "Another note whose id shares the first eight characters.")

	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: "aaaaaaaa", Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw accepted an ambiguous prefix")
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list the match %s: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "more") {
		t.Errorf("the refusal does not say how to disambiguate: %v", err)
	}
	// Nothing moved: the target still has every live edge it had.
	if got := liveEdgeCount(t, store, target); got != 2 {
		t.Errorf("live edges into the target = %d, want 2 — an ambiguous ref must write nothing", got)
	}
}

// TestWithdrawRefusesARefItCannotResolve: a ref that names nothing is a refusal,
// and it says WHICH form failed — a reader who pasted a full id needs to be told
// the store does not hold it, not that what they pasted is malformed.
func TestWithdrawRefusesARefItCannotResolve(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, _, target := seedTwoEdges(t, store, db)

	for _, tc := range []struct {
		name        string
		source, tgt string
		want        string
	}{
		{name: "seven hex characters", source: a[:7], tgt: target, want: "too short to be a prefix"},
		{name: "not hex", source: "not-an-id-at-all", tgt: target, want: "no memory in project"},
		{name: "empty", source: "", tgt: target, want: "empty"},
		{name: "a memory that does not exist", source: "ffffffffffffffffffffffffffffffff", tgt: target, want: "no memory in project"},
		{name: "target that does not exist", source: a, tgt: "ffffffffffffffffffffffffffffffff", want: "no memory in project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: tc.source, Target: tc.tgt}}, true, discardLogger())
			if err == nil {
				t.Fatal("Withdraw accepted a ref that names nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if got := liveEdgeCount(t, store, target); got != 2 {
				t.Errorf("live edges = %d, want 2: a refused ref must write nothing", got)
			}
		})
	}
}

// TestWithdrawNamesAnImportedID: `ghost import` writes an artifact's ids
// verbatim, and nothing about the column says they are hex — the hex default is
// only a default. A withdrawal that could not name such a row would be a repair
// nobody could perform on an imported corpus, and the refusal would compound it
// by telling the operator their full id was not a full id.
func TestWithdrawNamesAnImportedID(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// The two endpoints carry imported, non-hex ids; the link between them is a
	// real graph row.
	newer := pinID(t, db, "imported-note-a", "An imported note that restates the claim.")
	older := pinID(t, db, "imported-note-b", "An imported note that was recorded first.")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: newer, Target: older}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if res.Withdrawn != 1 || res.Links[0].TargetID != older {
		t.Fatalf("withdrawn=%d links=%+v, want the imported edge withdrawn", res.Withdrawn, res.Links)
	}
	// A short imported id is a full id too, so it is nameable — a length floor
	// applies to a PREFIX, not to an id the store actually holds.
	pinID(t, db, "abc", "A third imported note with a three character id.")
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: "abc", Target: older}}, false, discardLogger()); err == nil {
		t.Fatal("a full id shorter than the prefix floor was refused")
	} else if !strings.Contains(err.Error(), "no live supersedes link") {
		t.Errorf("the short full id was not resolved, so the refusal is about resolution: %v", err)
	}
}

// TestWithdrawDryRunWritesNothing: the preview is the decision an operator makes
// --apply against, so it has to be a preview. Both halves of "withdrawn" are
// checked — the edge is still live, and the unsupersede history row that says
// so is not there.
func TestWithdrawDryRunWritesNothing(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, _, target := seedTwoEdges(t, store, db)

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a[:8], Target: target[:8]}}, false, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw (dry run): %v", err)
	}
	if res.Resolved != 1 || res.Withdrawn != 0 || len(res.Links) != 1 {
		t.Fatalf("dry run: resolved=%d withdrawn=%d links=%d, want 1, 0, 1", res.Resolved, res.Withdrawn, len(res.Links))
	}
	if res.Links[0].Withdrawn {
		t.Error("a dry run reported a withdrawal it did not make")
	}
	if got := liveEdgeCount(t, store, target); got != 2 {
		t.Errorf("live edges = %d, want 2: the dry run withdrew one", got)
	}
	if hasUnsupersedeHistory(t, store, target) {
		t.Error("a dry run wrote the unsupersede history row")
	}
}

// TestWithdrawApplyInvalidatesAndRecordsTheWithdrawal: apply is the ordinary
// InvalidateLink path, so it writes the `unsupersede` history row. A corpus
// whose audit shows a supersession and no withdrawal reads as though the stale
// claim is still live, so that row is part of what "withdrawn" means.
func TestWithdrawApplyInvalidatesAndRecordsTheWithdrawal(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, _, target := seedTwoEdges(t, store, db)

	res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a[:8], Target: target[:8]}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw (apply): %v", err)
	}
	if res.Resolved != 1 || res.Withdrawn != 1 || len(res.Links) != 1 || !res.Links[0].Withdrawn {
		t.Fatalf("apply: resolved=%d withdrawn=%d links=%+v, want 1, 1 and one written link", res.Resolved, res.Withdrawn, res.Links)
	}
	if got := liveEdgeCount(t, store, target); got != 1 {
		t.Errorf("live edges into the target = %d, want 1: the other edge is untouched", got)
	}
	// The withdrawn edge is gone from every reader, not merely stamped.
	if pairs, err := store.SupersedesWithin(ctx, []string{a, target}); err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	} else if len(pairs) != 0 {
		t.Errorf("the withdrawn edge is still ranked as a supersession: %v", pairs)
	}
	assertUnsupersedeHistory(t, store, target)
	// The report says which edge's own source column the withdrawal carried, so
	// a reader can tell the edge resolve's piggyback acts on from one it does not.
	if res.Links[0].LinkSource != "llm" {
		t.Errorf("LinkSource = %q, want the edge's own source \"llm\"", res.Links[0].LinkSource)
	}
	// And it carries the target's own text, so an operator can tell from the
	// report whether the edge they just withdrew was the one they meant.
	if res.Links[0].TargetText != "The restore path on one spindle is safe and takes under a minute." {
		t.Errorf("TargetText = %q, want the target's stored content", res.Links[0].TargetText)
	}
}

// TestWithdrawRefusesAPairWithNoLiveLink: a pair with no live edge is an error,
// not a silent success — "withdrew 0" and "withdrew the edge" are the same
// sentence to a reader and only one of them is true. The refusal names the
// target's live edges so the operator can see what they meant.
func TestWithdrawRefusesAPairWithNoLiveLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)

	// Reverse direction: b→target is live, target→b is not a supersession of
	// anything (a memory cannot supersede its own replacement's target).
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

	// Withdraw one edge, then ask for it again: the second ask is the same
	// error, not a quiet no-op that reads as a completed withdrawal.
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
	_, err = Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("re-withdrawing a withdrawn edge reported success")
	}
	// The refusal names the target's remaining live edges, because "no live
	// supersedes link A→B" on its own is a dead end and "B is still superseded by
	// C" is an answer.
	if !strings.Contains(err.Error(), "superseded, from this project, by "+b[:8]) {
		t.Errorf("the refusal does not name the target's live edges: %v", err)
	}
	if got := liveEdgeCount(t, store, target); got != 1 {
		t.Errorf("live edges = %d, want 1", got)
	}
}

// TestWithdrawChangesNothingWhenAnyPairIsWrong: the contract for a batch. One
// bad pair in a list of five must not withdraw four and report an error — the
// operator asked for a specific set, and half of it is a decision they did not
// make. So every pair is resolved and checked before any write.
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
// of zero withdrawals — the second reads as "there was nothing to withdraw" and
// is a claim about the graph.
func TestWithdrawWithNoPairs(t *testing.T) {
	store, _ := seed(t)
	if _, err := Withdraw(context.Background(), store, "p", nil, true, discardLogger()); err == nil {
		t.Fatal("Withdraw accepted an empty request")
	}
}

// TestWithdrawRefusesASelfPair: both refs resolving to one memory is a pair that
// cannot be a supersession (CreateLink refuses self-links), and saying so beats
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

// TestWithdrawLeavesAnotherProjectsEdgeAlone: the edge is found through the
// project that owns its SOURCE, so a project can neither withdraw nor be told
// about an edge belonging to another one. This is the same scoping
// LinksByRelationSource and resolve's piggyback apply, and it is what keeps the
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

	// The target's id does not resolve from p at all, so the request never
	// reaches the graph: a project cannot even name another project's memory.
	_, err = Withdraw(ctx, store, "p", []WithdrawPair{{Source: newer, Target: older}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw reached an edge owned by another project")
	}
	if !strings.Contains(err.Error(), "no memory in project p") {
		t.Errorf("the refusal is not the project-scoped ref resolution: %v", err)
	}
	links, err := store.SupersedesLinksInto(ctx, "q", older)
	if err != nil {
		t.Fatalf("SupersedesLinksInto: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("the other project's edge = %d live row(s), want 1: it must be untouched", len(links))
	}
}

// The case `ghost_memory_promote` leaves behind — a live edge whose SOURCE has
// moved to _global while its target stayed — is TestWithdrawReachesAnEdgeWhose-
// SourceWasPromoted in global_source_test.go, with the reachability this file
// used to refuse: the ownership rule is now "either endpoint is ours", and
// `_global` is ours from every project. What must still be refused is an edge no
// project claims, which TestWithdrawLeavesAnotherProjectsEdgeAlone above and
// TestWithdrawStillRefusesAnEdgeNoProjectClaims there pin.

// TestWithdrawChainsIntoResolveReassess is the step that makes a withdrawal
// visible in a session. A 'supersedes' edge is not informational: resolve's
// piggyback stamps resolved_at on the older endpoint for free, and its repair
// pass deliberately HONOURS a live edge as a floor. So withdrawing the only edge
// that buried a memory is half the repair — `ghost resolve --reassess` has to
// clear the resolved_at it caused, or the memory stays out of injection anyway.
func TestWithdrawChainsIntoResolveReassess(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	a, b, target := seedTwoEdges(t, store, db)
	if n, err := store.SetResolved(ctx, []string{target}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, err)
	}

	// Withdraw ONE of the two live edges. The other still asserts the target, so
	// resolve's repair pass must keep holding the row back: a withdrawal is not a
	// statement about every edge that points at the same memory.
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: a, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	// Dry run, so this half of the chain does not consume the stamp the next one
	// needs to clear: what is under test is WHICH rows the pass would clear, and a
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

	// Withdraw the last one. Now nothing asserts the target, the same KEEP
	// verdict clears it, and the memory returns to ranked injection.
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
	// Both withdrawals left their audit row, so the history reads as a
	// supersession and two withdrawals rather than a supersession standing.
	assertUnsupersedeHistory(t, store, target)

	// The other half of the chain, the way this PR's follow-up actually invokes
	// it: a SCOPED repair over the withdrawn edges' own targets — here one pair, so
	// one id, which is the list the printed command carries. A second
	// resolved row that no edge ever pointed at is in the pool to prove the
	// scope kept the pass off it — the whole reason the follow-up is scoped
	// (#698 measured an unscoped repair proposing to un-hide 143 rows, ~35% of
	// them stale). This is the case the printed command has to get right.
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
// visible as one. The edges before the failure are gone and cannot be un-gone, so
// the count and the list come back with the error; and the edges AFTER it were
// never touched, so they are still live. Marking those as anything else — a
// withdrawal, or a concurrent pass's — is a claim about a graph change that did
// not happen, on rows the run never reached.
// The failingStore above is #688's, reused here: it is the same seam this needs —
// one InvalidateLink call in three fails — and a second copy in this package would
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
	// And the graph agrees with the markers: the third edge is still live.
	if n := liveEdgeCount(t, store, fourth); n != 1 {
		t.Errorf("live edges into the third target = %d, want 1: it was never reached", n)
	}
}

// TestWithdrawRefusesAShortRefWithoutDumpingTheProject: a ref shorter than the
// floor names a SLICE of the project's ids, and an error message that lists that
// slice is a dump of what the caller could not otherwise enumerate. The refusal
// says the ref is too short and prints no ids at all.
func TestWithdrawRefusesAShortRefWithoutDumpingTheProject(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// Twenty memories sharing a one-character prefix, and the ref is that ONE
	// character — so the slice the comment describes is really the slice the query
	// returns. A ref matching nothing would take the zero-match branch instead and
	// would pass against code that printed the whole slice.
	for i := 0; i < 20; i++ {
		pinID(t, db, "z"+string(rune('a'+i))+strings.Repeat("0", 30), "A note whose id starts with z.")
	}
	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: "z", Target: "za" + strings.Repeat("0", 30)}}, true, discardLogger())
	if err == nil {
		t.Fatal("a one-character ref was accepted")
	}
	if !strings.Contains(err.Error(), "too short to be a prefix") {
		t.Errorf("the refusal does not say the ref is too short: %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("0", 30)) {
		t.Errorf("the refusal listed the project's ids, which is a dump of what the caller could not enumerate: %v", err)
	}
}

// TestWithdrawPrefersTheSpellingThatWasTyped: the store matches case-insensitively
// and `memories.id` is BINARY-unique, so a corpus holding both "abc" and "ABC" —
// which `ghost import` admits, its presence check being case-sensitive — is
// addressable by the spelling the caller typed, and only by that. Reading it the
// other way (a case-insensitive match, then a guess) would make the repair
// unavailable for such a store, since no spelling would ever be accepted.
func TestWithdrawPrefersTheSpellingThatWasTyped(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	lowerTarget := pinID(t, db, "abcd", "The note the lower-case id supersedes.")
	upperTarget := pinID(t, db, "ABCD", "The note the upper-case id supersedes.")
	lower := pinID(t, db, "abc", "An imported note whose id is lower case.")
	upper := pinID(t, db, "ABC", "An imported note whose id is upper case.")
	for _, pair := range [][2]string{{lower, lowerTarget}, {upper, upperTarget}} {
		if err := store.CreateLink(ctx, pair[0], pair[1], string(RelationSupersedes), 0.9, "llm"); err != nil {
			t.Fatal(err)
		}
	}

	// Each spelling withdraws ITS OWN edge, and only that one.
	for _, tc := range []struct{ ref, target string }{{"abc", lowerTarget}, {"ABC", upperTarget}} {
		res, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: tc.ref, Target: tc.target}}, true, discardLogger())
		if err != nil {
			t.Fatalf("Withdraw(%s): %v", tc.ref, err)
		}
		if res.Withdrawn != 1 || res.Links[0].SourceID != tc.ref {
			t.Errorf("Withdraw(%s) resolved to %+v, want that exact id", tc.ref, res.Links)
		}
	}
	if n := liveEdgeCount(t, store, lowerTarget) + liveEdgeCount(t, store, upperTarget); n != 0 {
		t.Errorf("%d live edge(s) left, want 0: each spelling withdrew its own", n)
	}
}

// TestWithdrawRefusesAThirdCasingOfCaseVariantIDs: a ref spelled as NEITHER of two
// ids that differ only in letter case is the one shape no spelling can address,
// because the match is case-insensitive. That is a real dead end, so the refusal
// says so and names no remedy — naming a command would be naming a step that
// cannot open this one, and the same text is returned verbatim to an MCP caller
// that may have no shell.
func TestWithdrawRefusesAThirdCasingOfCaseVariantIDs(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	lowerTarget := pinID(t, db, "abcd", "The note the lower-case id supersedes.")
	upperTarget := pinID(t, db, "ABCD", "The note the upper-case id supersedes.")
	if err := store.CreateLink(ctx, pinID(t, db, "abc", "An imported note whose id is lower case."), lowerTarget, string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, pinID(t, db, "ABC", "An imported note whose id is upper case."), upperTarget, string(RelationSupersedes), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: "aBc", Target: lowerTarget}}, true, discardLogger())
	if err == nil {
		t.Fatal("a third casing of two case-variant ids was resolved to one of them")
	}
	if !strings.Contains(err.Error(), "differ only in letter case") {
		t.Errorf("the refusal does not name the collision: %v", err)
	}
	if strings.Contains(err.Error(), "ghost ") {
		t.Errorf("the refusal names a command as the way out, and the match is case-insensitive so no command argument opens it: %v", err)
	}
	// And it moved nothing.
	if n := liveEdgeCount(t, store, lowerTarget); n != 1 {
		t.Errorf("live edges into the lower-case target = %d, want 1: a refused ref must write nothing", n)
	}
	if n := liveEdgeCount(t, store, upperTarget); n != 1 {
		t.Errorf("live edges into the upper-case target = %d, want 1: a refused ref must write nothing", n)
	}
}

// TestWithdrawResolvesAnExactShortIDThatPrefixesALongerOne: the floor applies to
// a PREFIX, and "abc" is a full id that happens to prefix a longer one. Refusing
// it as ambiguous would contradict the rule the docs state, and would make an
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

// The whole feature rests on one round trip: a report prints the first eight
// characters of an id, and that string is what the operator pastes back into
// --withdraw. If the report abbreviates by BYTES and the query bounds by runes,
// the two measures disagree and the ref can never resolve — and the refusal
// asserts no memory has that id, which is the opposite of the truth.
//
// Ids are not necessarily hex: `ghost import` writes an artifact's ids verbatim,
// so this is reachable, and only through an id no ordinary test would use.
func TestWithdrawalRoundTripsTheReportFormOfANonASCIIID(t *testing.T) {
	ctx := context.Background()
	store, _ := seed(t)
	const source = "新しい復元手順-2026"
	const target = "古い復元手順-2025"
	if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
		ID: source, ProjectID: "p", Category: "fact",
		Content: "A restore that spanned two spindles took 41 minutes.", Source: "mcp",
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory(source): %v", err)
	}
	if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
		ID: target, ProjectID: "p", Category: "fact",
		Content: "The restore path on one spindle is safe and takes under a minute.", Source: "mcp",
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory(target): %v", err)
	}
	if err := store.CreateLink(ctx, source, target, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	// What a report would have put on screen for the wrong edge.
	reported := short(source)
	if !utf8.ValidString(reported) {
		t.Fatalf("the report form %q is not valid UTF-8 — it cannot be pasted, let alone matched", reported)
	}
	if n := utf8.RuneCountInString(reported); n != 8 {
		t.Fatalf("the report form is %d characters, want the documented 8: %q", n, reported)
	}
	// And pasting it withdraws the edge the report named.
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: reported, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("Withdraw with the reported form %q: %v", reported, err)
	}
	if n := liveEdgeCount(t, store, target); n != 0 {
		t.Errorf("the withdrawal left %d live edge(s) — the report form did not address the row", n)
	}
}

// The floor is a floor in CHARACTERS, which is what the docs say. A byte count
// admits a 3-character CJK ref (9 bytes) as an identity and refuses a
// 2-character one (6 bytes) — the exact class of near-miss the floor exists to
// prevent, inverted.
func TestResolveRefCountsCharactersNotBytes(t *testing.T) {
	store, _ := seed(t)
	const threeChars = "日本語" // 3 characters, 9 bytes
	if _, _, _, err := store.ImportMemory(context.Background(), memory.PortableMemory{
		ID: threeChars + "-note", ProjectID: "p", Category: "fact",
		Content: "An imported note with a short non-ASCII id.", Source: "mcp",
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if _, err := resolveRef(context.Background(), store, "p", "source", threeChars); err == nil {
		t.Error("a 3-character ref is under the 8-character floor and must be refused as too short")
	} else if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error %q must say the ref is too short, not that it is ambiguous or unknown", err)
	}
	// The full id still resolves, whatever its length: the floor is about prefixes.
	got, err := resolveRef(context.Background(), store, "p", "source", threeChars+"-note")
	if err != nil {
		t.Fatalf("resolveRef on the full id: %v", err)
	}
	if got != threeChars+"-note" {
		t.Errorf("resolveRef = %q, want the stored id", got)
	}
}
