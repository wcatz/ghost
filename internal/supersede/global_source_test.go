package supersede

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// seedGlobalSourceEdge writes a live 'supersedes'/'llm' edge over two notes of
// project "p" and then promotes the SOURCE to _global — the shape
// `ghost_memory_promote` and `ghost reflect --promote-globals` leave behind. A
// promotion moves the memory and KEEPS its links, so the edge is left with one
// endpoint in the project and one in the shared scope.
//
// That is the whole fixture. The target stays in "p", so "p" is the project the
// edge buries a memory in, and "p" is the project an operator thinks in when they
// read that memory missing from a session.
func seedGlobalSourceEdge(t *testing.T, store *memory.Store) (source, target string) {
	t.Helper()
	ctx := context.Background()
	source = mustCreatePlain(t, store, "A note promoted to _global after it superseded another one.")
	target = mustCreatePlain(t, store, "The project note that promoted one superseded.")
	if err := store.CreateLink(ctx, source, target, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteToGlobal(ctx, "p", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	return source, target
}

// TestWithdrawReachesAnEdgeWhoseSourceWasPromoted is #786. A live 'supersedes'
// edge does two things to its target — it demotes it in ranking and it
// justifies a resolved_at — and both are unscoped, so they follow the target into
// the project that owns it. Before this the repair surfaces were not: the target's
// own project refused the withdrawal with "no memory in this project supersedes
// it", and the one surface that could see the edge (`--reassess` in `_global`)
// withdraws only what a classifier rejects. So a wrong edge whose source had been
// promoted had no operator-facing undo at all, which is the exact repair
// withdraw.go's own doc comment promises and does not deliver.
//
// Both routes an operator has are pinned, because they are different commands
// reaching the same edge: the target's own project, and the shared scope the
// promoted source now lives in.
func TestWithdrawReachesAnEdgeWhoseSourceWasPromoted(t *testing.T) {
	for _, projectID := range []string{"p", memory.GlobalProjectID} {
		t.Run("from "+projectID, func(t *testing.T) {
			store, _ := seed(t)
			ctx := context.Background()
			source, target := seedGlobalSourceEdge(t, store)

			// A dry run first, so what is under test is that the request is
			// RESOLVED — the refs name the edge and the edge is found — and not
			// that the write works.
			res, err := Withdraw(ctx, store, projectID,
				[]WithdrawPair{{Source: source, Target: target}}, false, discardLogger())
			if err != nil {
				t.Fatalf("Withdraw(%s) refused an edge whose source was promoted to _global: %v", projectID, err)
			}
			if len(res.Links) != 1 || res.Links[0].SourceID != source || res.Links[0].TargetID != target {
				t.Fatalf("resolved to %+v, want the %s→%s edge", res.Links, source, target)
			}
			if res.Withdrawn != 0 {
				t.Errorf("a dry run withdrew %d edge(s)", res.Withdrawn)
			}
			if liveEdgeCount(t, store, target) != 1 {
				t.Fatalf("live edges = %d after the dry run, want 1", liveEdgeCount(t, store, target))
			}

			// And the write lands, through the ordinary InvalidateLink path, so
			// the audit shows the claim and the withdrawal.
			applied, err := Withdraw(ctx, store, projectID,
				[]WithdrawPair{{Source: source, Target: target}}, true, discardLogger())
			if err != nil {
				t.Fatalf("Withdraw(%s, apply): %v", projectID, err)
			}
			if applied.Withdrawn != 1 || !applied.Links[0].Withdrawn {
				t.Errorf("applied = %+v, want the one edge withdrawn", applied)
			}
			if liveEdgeCount(t, store, target) != 0 {
				t.Errorf("live edges = %d after the withdrawal, want 0", liveEdgeCount(t, store, target))
			}
			assertUnsupersedeHistory(t, store, target)
			// The target is repairable again, which is the state the follow-up
			// exists to hand over.
			if got := RepairableTargets(applied.Links); len(got) != 1 || got[0] != target {
				t.Errorf("RepairableTargets = %v, want [%s]: the memory whose resolution the edge justified", got, target)
			}
		})
	}
}

// TestWithdrawStillRefusesAnEdgeNoProjectClaims is the boundary the widened
// ownership rule must not cross. `_global` is shared — a memory there is visible
// in every project and every ref resolves to it — but a project is not. An edge
// with both endpoints in "q" stays outside "p" even though "p" shares the store,
// and the refusal still names the project it looked in, so a reader is not sent
// looking for a claim that exists under a name they were not given.
func TestWithdrawStillRefusesAnEdgeNoProjectClaims(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "q", "/tmp/q", "q"); err != nil {
		t.Fatal(err)
	}
	foreign := func(content string) string {
		id, err := store.Create(ctx, "q", memory.Memory{Category: "fact", Content: content, Source: "mcp"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	source := foreign("Another project's superseding note, and not a global one.")
	target := foreign("Another project's stale note, in a project of its own.")
	if err := store.CreateLink(ctx, source, target, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	// Named from "p": the refs do not resolve there, which is the refusal a
	// reader can act on.
	_, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: source, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw reached an edge belonging to another project")
	}
	if !strings.Contains(err.Error(), "no memory in project p") {
		t.Errorf("the refusal is not the project-scoped ref resolution: %v", err)
	}
	// Named from "_global", where the REFS are unscoped: the edge read is what
	// refuses, and it refuses by naming the scope it looked in rather than
	// claiming nothing supersedes the memory anywhere.
	_, err = Withdraw(ctx, store, memory.GlobalProjectID,
		[]WithdrawPair{{Source: source, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("Withdraw from _global reached an edge with no endpoint in the shared scope")
	}
	if !strings.Contains(err.Error(), "no live supersedes link") {
		t.Errorf("the refusal is not about the edge's ownership: %v", err)
	}
	if strings.Contains(err.Error(), "nothing supersedes that memory") {
		t.Errorf("the refusal makes a claim about the whole graph: %v", err)
	}
	if links, lerr := store.SupersedesLinksInto(ctx, memory.GlobalProjectID, target); lerr != nil {
		t.Fatalf("SupersedesLinksInto: %v", lerr)
	} else if len(links) != 0 {
		t.Errorf("the other project's edge is visible from _global as %d live row(s): the shared scope is not every project", len(links))
	}
}

// TestReassessReachesAnEdgeWhoseSourceWasPromoted: the second repair surface.
// `--reassess` judges live edges with the current rules and withdraws what they
// no longer support, so an edge it cannot LOAD is an edge no rule can ever reach
// — which is how a wrong edge outlives every pass that would have fixed it.
//
// The source is the endpoint a promotion moves, and the load is scoped to the
// edge's SOURCE, so this is the half of #786 the ordinary pass and the resolve
// piggyback share.
func TestReassessReachesAnEdgeWhoseSourceWasPromoted(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	source, target := seedGlobalSourceEdge(t, store)

	// The load is the first thing to be wrong: Loaded is the count the pass
	// reports as the edges it considered, so a zero here is a report claiming
	// the project holds no live supersedes edge at all.
	cls := &neitherReassessClassifier{}
	res, _, err := Reassess(ctx, store, cls, "p", false, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Loaded != 1 {
		t.Fatalf("Reassess(%q).Loaded = %d, want 1: the project that owns the target cannot load the edge burying it", "p", res.Loaded)
	}
	if res.Withdrawn != 0 {
		t.Errorf("a dry run withdrew %d edge(s)", res.Withdrawn)
	}

	applied, withdrawn, err := Reassess(ctx, store, &neitherReassessClassifier{}, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess(apply): %v", err)
	}
	if applied.Withdrawn != 1 {
		t.Errorf("Reassess(%q).Withdrawn = %d, want 1", "p", applied.Withdrawn)
	}
	if len(withdrawn) != 1 || withdrawn[0].NewerID != source || withdrawn[0].OlderID != target {
		t.Fatalf("withdrawn = %+v, want the %s→%s edge", withdrawn, source, target)
	}
	if liveEdgeCount(t, store, target) != 0 {
		t.Errorf("live edges = %d after the repair, want 0", liveEdgeCount(t, store, target))
	}
}

// TestEverySurfaceAgreesOnWhichEdgesExist is the consistency rule behind both
// halves of #786, and it is stated as one test because the disagreement IS the
// bug: SupersedePenalties carries no project predicate, so it demotes a target
// for ANY live edge — including one whose source has been promoted to _global.
// A repair surface that cannot see that edge is what leaves the two consumers
// disagreeing, and the disagreement is not cosmetic:
//
//   - the demotion is permanent, and nothing in the ranking path can undo it;
//   - resolve's repair pass honours a live edge as a floor, so it HOLDS the
//     target's resolved_at while the demotion is un-withdrawable, and releases
//     the moment the edge is withdrawn;
//   - so the target was resolved with the ranking still demoting it, and no
//     command could name the edge that caused either.
//
// The three assertions below are one claim: the ranking demotes the target, the
// floor holds it for the same edge, and both repair surfaces can name that edge.
// Any one of them going quiet while the demotion persists is #786.
func TestEverySurfaceAgreesOnWhichEdgesExist(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	source, target := seedGlobalSourceEdge(t, store)

	// 1. The ranking. Unscoped by construction, and it demotes the target.
	penalty, err := memory.SupersedePenalties(ctx, db, []string{source, target}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if penalty[target] != 1 {
		t.Fatalf("SupersedePenalties = %v, want {%s: 1}: the demotion is what makes the edge worth repairing", penalty, target)
	}

	// 2. The floor. resolve's repair pass holds a resolved row back while a live
	// edge asserts it, and the hold is named with the edge's source — so a floor
	// that cannot see the edge is a row reported as repairable and re-stamped by
	// the next ordinary pass.
	if n, serr := store.SetResolved(ctx, []string{target}); serr != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, serr)
	}
	held, reKept, err := resolve.Reassess(ctx, store, &keepVerdictClassifier{}, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess: %v", err)
	}
	if held.Demoted != 1 || held.Cleared != 0 {
		t.Errorf("the floor asserted=%d cleared=%d, want 1/0: a live edge holds the target down, and the same edge is one --withdraw away", held.Demoted, held.Cleared)
	}
	if len(reKept) != 0 {
		t.Errorf("reKept = %+v, want nothing: the target is held, not repairable", reKept)
	}

	// 3. The repair surfaces, over the same edge the two above act on. Both name
	// it before anything is written, so what is proved is reachability.
	if _, _, err := Reassess(ctx, store, &neitherReassessClassifier{}, "p", false, discardLogger()); err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: source, Target: target}}, false, discardLogger()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	// And the whole loop closes: withdrawing the edge releases the demotion and
	// the floor together, and the target becomes repairable again — which is the
	// state the two commands between them are supposed to produce. Before #786
	// the second half was unreachable, so this half never happened.
	if _, err := Withdraw(ctx, store, "p", []WithdrawPair{{Source: source, Target: target}}, true, discardLogger()); err != nil {
		t.Fatalf("Withdraw(apply): %v", err)
	}
	after, err := memory.SupersedePenalties(ctx, db, []string{source, target}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties (after): %v", err)
	}
	if len(after) != 0 {
		t.Errorf("SupersedePenalties = %v after the withdrawal, want empty: a withdrawn edge demotes nothing", after)
	}
	released, reKept, err := resolve.Reassess(ctx, store, &keepVerdictClassifier{}, "p", true, resolve.Scope{}, discardLogger())
	if err != nil {
		t.Fatalf("resolve.Reassess (after): %v", err)
	}
	if released.Cleared != 1 || len(reKept) != 1 || reKept[0].ID != target {
		t.Errorf("after the withdrawal: cleared=%d reKept=%+v, want the target cleared and nothing held back", released.Cleared, reKept)
	}
	if isResolved(t, store, target) {
		t.Error("the target is still stamped resolved_at after the chain closed")
	}
}

// TestWithdrawReachesAnEdgeWhoseTargetWasPromoted is the mirror of the case
// above, and it is a different half of the same rule. A promotion can move either
// endpoint: when the TARGET is promoted, the edge is a claim made from a project
// note about a memory in the shared scope, and the shared scope is where that
// memory lives and where the operator sees it missing from every session. So the
// `_global` command has to reach it — which needs the target's half of the
// ownership rule, not the source's.
func TestWithdrawReachesAnEdgeWhoseTargetWasPromoted(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	source := mustCreatePlain(t, store, "A project note that supersedes a note about to be promoted.")
	target := mustCreatePlain(t, store, "A note promoted to _global after this one superseded it.")
	if err := store.CreateLink(ctx, source, target, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteToGlobal(ctx, "p", target); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	res, err := Withdraw(ctx, store, memory.GlobalProjectID,
		[]WithdrawPair{{Source: source, Target: target}}, true, discardLogger())
	if err != nil {
		t.Fatalf("Withdraw(_global) refused an edge whose TARGET is in the shared scope: %v", err)
	}
	if res.Withdrawn != 1 {
		t.Errorf("Withdrawn = %d, want 1", res.Withdrawn)
	}
	if links, lerr := store.LinksByRelationSource(ctx, "p", string(RelationSupersedes), "llm"); lerr != nil {
		t.Fatalf("LinksByRelationSource: %v", lerr)
	} else if len(links) != 0 {
		t.Errorf("live edge(s) = %d after the withdrawal, want 0", len(links))
	}
}

// TestWithdrawCannotNameAnotherProjectsSource bounds the ownership rule from the
// other side, and it is about REFS rather than about the edge read. `ghost project
// merge` moves a memory between projects and leaves its links, so a project can own
// a target whose source sits in a project that is not its own — and the ownership
// rule above says that target's project may withdraw that edge. It still cannot,
// because the SOURCE ref does not resolve there.
//
// That is deliberate, and the alternative is worse than the limitation. Widening a
// project's ref scope to reach a neighbour would make `ghost supersede p --withdraw
// <q-prefix> …` answer "no live supersedes link" for an id that does not exist in p
// — which is a question about another project's corpus asked through this one, and
// the reason MemoryIDsByIDPrefix is project-scoped at all. So the edge read is
// wider than the refs on purpose: it can see the edge, and the command still cannot
// name it from here. A shared-scope endpoint is the case that IS reachable, and
// neither test above is about this one.
func TestWithdrawCannotNameAnotherProjectsSource(t *testing.T) {
	store, _ := seed(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "q", "/tmp/q", "q"); err != nil {
		t.Fatal(err)
	}
	source, err := store.Create(ctx, "q", memory.Memory{
		Category: "fact", Content: "A note in q that supersedes one in p.", Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := mustCreatePlain(t, store, "The p note that a q note superseded, and kept when q was merged away.")
	if err := store.CreateLink(ctx, source, target, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	_, err = Withdraw(ctx, store, "p", []WithdrawPair{{Source: source, Target: target}}, true, discardLogger())
	if err == nil {
		t.Fatal("a project named another project's memory")
	}
	if !strings.Contains(err.Error(), "no memory in project p") {
		t.Errorf("the refusal is not the project-scoped ref resolution: %v", err)
	}
	// The edge read DOES see it — the target's project owns the memory being
	// buried — so the refusal is about naming the pair, not about the ownership
	// rule having lost the edge.
	links, lerr := store.SupersedesLinksInto(ctx, "p", target)
	if lerr != nil {
		t.Fatalf("SupersedesLinksInto: %v", lerr)
	}
	if len(links) != 1 {
		t.Errorf("SupersedesLinksInto(p) = %+v, want the one edge burying p's own memory", links)
	}
}

// neitherReassessClassifier answers NEITHER to every relation question, which is
// the verdict that denies a replacement in either direction and so withdraws
// whatever live edge the pair carries.
type neitherReassessClassifier struct{ calls int }

func (n *neitherReassessClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	n.calls++
	out := make([]Relation, len(pairs))
	for i := range out {
		out[i] = RelationNeither
	}
	return out, nil
}
