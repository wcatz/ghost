// The repair pass: re-judge the 'supersedes' edges already in the graph under
// the current rules, and withdraw the ones those rules no longer support.
//
// Issue #686 measured the creation pass on a real database and found 43%
// precision: of the edges one dry run proposed, every wrong one joined two notes
// that were BOTH still true. Those edges are already in the stores that ran the
// old pass, and they are not inert. `ghost resolve`'s supersedes piggyback stamps
// resolved_at on the older endpoint for free, which removes it from ranked
// injection everywhere — and resolve's own repair pass deliberately HONOURS a
// live 'supersedes' edge as a floor, so it cannot undo the resolution while the
// edge stands. Nothing in the ordinary pass can reach an edge whose endpoints
// have not changed: skip-if-unchanged holds it quiet forever, whichever way its
// own candidate scan proposes the pair (#787) and however long ago the edge was
// last confirmed (#784).
//
// This pass is the operator-facing undo. It re-applies the deterministic
// imperative veto and the current classifier to every live 'supersedes'/'llm'
// edge, and with --apply invalidates the ones that come back NEITHER, vetoed,
// CAUSES or REVERSED through the ordinary InvalidateLink path, which writes the
// `unsupersede` history row. It then reports which edges it withdrew, so the
// repair is auditable: a corpus whose history shows a supersession and no
// withdrawal reads as though the stale claim is still live.
//
// Withdrawing the edge is half the repair. A memory that only a now-withdrawn
// edge justified is still stamped resolved_at, and only `ghost resolve
// --reassess` can clear that — see TestReassessResolvesAChainIntoResolveReassess,
// which runs the chain against a real store. Until that second pass runs, the
// withdrawn edge's older endpoint stays out of injection, which is the safe
// direction: a duplicated stale note beats a memory nobody is reminded of.
//
// It deliberately does NOT write 'causes' links for the edges it withdraws, and
// it does not re-link the ones it confirms. A repair pass that also creates
// edges has two jobs and one dry-run answer, and every withdrawn pair comes back
// as a fresh candidate on the next ordinary pass, which is where a causes edge
// belongs. It DOES sweep the other relation's edge, exactly as Run does on the
// same verdicts: a 'causes' link pointing INTO a note the pass has just decided
// is still current asserts the opposite of that decision. Only the two
// self-contradicting shapes are swept — NEITHER, the veto and REVERSED — because
// a CAUSES verdict is a claim the existing 'causes' edge may well be making
// correctly, and a repair pass is not the place to re-decide it.
//
// ONE THING HERE DELETES, and it is the veto, so the creation pass's error
// argument does not transfer to it wholesale. There a false veto costs recall —
// a stale note stays ranked and a later pass can still link it. Here it costs a
// correct edge, and because the veto is deterministic on the same two note
// bodies, Run will re-fire it on every later pass: the pair cannot be re-linked
// until one of the notes changes. That is the trade the operator is being asked
// to make, and it is why the report says "veto, no harness call" on those rows
// instead of leaving them indistinguishable from a model-adjudicated verdict.
// Run's veto is deliberately NOT withdrawn on the ordinary pass for the same
// reason: a creation pass is not where graph history gets deleted.
//
// A CYCLE — one pair live in BOTH directions — is the one shape this pass used to
// get wrong, and getting it wrong looked like success (#778). It loaded every live
// edge as an independent candidate, so a pair claimed in both directions was asked
// the same question twice; a classifier that cannot decline a direction answered
// `supersedes` to both, and the pass reported Confirmed 2, Withdrawn 0 and left
// the cycle in the graph — the shape where each edge demotes the endpoint the
// other promotes, so the repair that exists to remove wrong edges preserved the
// worst one and said nothing. So the edges are grouped by the UNORDERED pair
// first, a cycle is ONE question, and the verdict is read as a DIRECTION:
// `supersedes` names one of the two live edges as current, so that edge stands and
// its reverse is withdrawn; `reversed` names the other, so the other stands;
// `neither` and `causes` deny the replacement in either direction, so both go. The
// two outcomes that decide NOTHING are separate values — a missing verdict is
// answered by a RERUN, a pair with no knowable direction only by the operator —
// and neither withdraws, because withdrawing half a cycle on a hunch would leave a
// live edge this pass never judged: the state the cycle itself is a report about.
// `ReassessResult.Cyclic` is a list for that reason, and the veto is not applied
// to a cycle: it asks one orientation's question, and a cycle has two over the
// same two bodies. A CycleOutcome holds no prose, because the report is the only
// thing that knows whether this run was a dry run and must be the only thing
// spelling a withdrawal as one.
package supersede

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// reassessStore is the subset of *memory.Store the repair pass needs; narrowed
// for testability. It reads the live edges and their endpoints' scopes, and it
// writes nothing but the invalidation.
type reassessStore interface {
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetLinks(ctx context.Context, memoryID string) ([]memory.Link, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error)
}

// ReassessResult summarizes a reassess pass. Every input row is an edge that is
// already in the graph, so the outcomes are "still supported" and "withdrawn" —
// there is no "would create" side, and Withdrawn counts the invalidations that
// actually landed (0 in dry-run).
type ReassessResult struct {
	Loaded       int // live 'supersedes'/'llm' edges the pass considered
	Skipped      int // edges it does not judge: an endpoint is gone, or the pair is scope-conflicting
	Vetoed       int // settled by the deterministic imperative veto, no classifier call
	Confirmed    int // came back SUPERSEDES: the edge stands
	Neither      int // came back NEITHER: both notes are still true, so the edge goes
	Causes       int // came back CAUSES: the older note is still true, so the edge goes
	Reversed     int // came back REVERSED: the edge runs against the pair, so it goes
	Unclassified int // unparseable verdict; the edge is left alone and re-offered
	Withdrawn    int // supersedes edges actually invalidated (0 in dry-run)
	// CausesWithdrawn counts the 'causes' edges the other-relation sweep moved,
	// or would move in a dry run. A concurrent pass can take one first, so under
	// --apply it can read lower than the dry run's prediction — the same
	// relationship Withdrawn has to the list of rows.
	CausesWithdrawn int
	// CausesSweepFailed counts the rows whose 'causes' sweep errored, where the
	// number of edges that went is UNKNOWN. It is reported separately from
	// CausesWithdrawn because "swept 0" and "could not tell" are different
	// sentences, and an operator who read "would sweep 1" in the dry run needs
	// the second one.
	CausesSweepFailed int
	// Unoriented counts the pairs the pass could not even frame a question about
	// because both rows share their updated_at AND their created_at, so no
	// direction is knowable (#778). It is zero on every store this pass normally
	// sees, and non-zero on a bulk-imported one: a cycle whose notes were
	// stamped in the same batch is reported and left alone, because guessing a
	// direction for it is the harm the #778 rule exists to prevent. The pair
	// still appears in Cyclic, so the report names it and hands over the
	// --withdraw commands.
	//
	// Only a CYCLE can land here, and that is not a narrowing of the rule: an
	// ordinary edge is judged in the direction the GRAPH asserts, which it
	// carries, so the pass never needs a chronology to ask about it. A cycle is
	// the one shape with no stored direction to fall back on — it holds both.
	Unoriented int
	// CausesPredictionFailed counts the dry-run rows whose 'causes' prediction
	// the pass could not read at all, so their second-deletion count is unknown
	// rather than zero. It is 0 in every --apply run, which never reads the
	// prediction (the sweep's observed count is the only number it reports), and
	// it is counted rather than silently dropped for the same reason as
	// CausesSweepFailed: a preview that prints "would sweep 0" for a read that
	// never happened sends the operator away believing there is nothing else to
	// delete.
	CausesPredictionFailed int
	// Cyclic names every pair the graph claims in BOTH directions — the cycle a
	// pass before #778 could write, whose two edges each demote one of the pair's
	// two memories, so neither note stays ranked until one of them goes.
	//
	// It is a LIST and not a count because a count cannot be acted on: the repair
	// for a cycle is one `ghost supersede --withdraw` command per edge, and an
	// operator deciding which half to drop needs the ids. Every pair here was
	// judged ONCE, as an unordered pair, so a verdict could keep exactly one
	// direction and withdraw the edge asserting the opposite. The two outcomes
	// that deny nothing are separate because the operator's next step differs: a
	// missing verdict is answered by a RERUN, and only a pair with no knowable
	// direction is the operator's to decide.
	Cyclic []CyclicPair
	// Unjudged names the pairs the classifier produced no verdict for, because
	// the call that carried them failed on both attempts or the reply's verdict
	// count did not match the pairs asked about (#699). Their edges are LEFT
	// ALIVE: nothing was decided about them, and the next pass re-asks them, so
	// this is the list of what a rerun still owes. It is a list rather than a
	// count because "a project of 87 edges, 6 of them unjudged" is a different
	// thing for an operator than "6 unjudged" with no way to find them — and one
	// failed call can be the whole set.
	//
	// It is the pairs the FAILED calls carried, not every open pair: a project's
	// open pairs are chunked across several harness calls, and a call that fails
	// answers nothing about the pairs IT carried while saying everything about
	// the pairs the calls before it already answered (#808). Those verdicts are
	// applied like any others, so a rerun owes this list and not the whole pass.
	Unjudged []UnjudgedPair
}

// UnjudgedPair is one live edge whose pair the classifier never answered. It
// carries the ids only, because that is all an operator needs to re-run the
// pass over it — and all the pass can know: a classify call that failed answers
// nothing about any pair it carried, not only the one the harness named last.
type UnjudgedPair struct {
	NewerID string
	OlderID string
}

// CyclicEdge is one of a pair's two live 'supersedes' edges, in the direction it
// is written: Source supersedes Target. It is a pair of ids and nothing else,
// because a cycle is a graph shape and the report draws it, not the text of the
// notes the two edges were written from.
type CyclicEdge struct {
	SourceID string
	TargetID string
}

// CyclicPair is one pair the graph claims in BOTH directions, and what this pass
// did about it. First and Second are the two live edges, in the order the store
// returned them — an order that carries no meaning, which is exactly why the pass
// does not use it to choose a direction (see the cycle handling in Reassess).
type CyclicPair struct {
	First  CyclicEdge
	Second CyclicEdge
	// Outcome names what happened, because "0 withdrawn" over a pair whose both
	// edges are still live is the single most misleading line this report could
	// print, and the reader cannot tell it from a pass that had nothing to do.
	// It is one of the four CycleOutcome values.
	Outcome CycleOutcome
}

// CycleOutcome is what a pass decided about a pair live in BOTH directions. The
// values are a DECISION and not a sentence, deliberately: the report is where the
// wording lives, because only the report knows whether this was a dry run, and a
// constant that spelled out "was withdrawn" would put a past-tense claim under a
// list of "would withdraw" rows (#780's review). Each value is a claim the pass
// can make from the verdict it received:
//
//   - A SUPERSEDES verdict names the direction the pair runs, so that edge stays
//     and the edge asserting the opposite goes. KeptFirst is the ordinary case
//     and KeptSecond the mirror of it, because which of the two live edges the
//     pass asked about depends on the timestamps and not on the store's order.
//   - A REVERSED verdict names the OTHER direction as the current one, so the
//     other edge is the one that stands.
//   - NEITHER and CAUSES deny the same-fact replacement in either direction, so
//     both go: an edge left live asserts what the verdict just denied.
//   - The two outcomes that decide NOTHING are SEPARATE, because they are
//     different facts and the operator's next step is different for each. A
//     missing verdict — an unparseable reply, or a classify call that failed —
//     means the pass asked and got nothing, so a RERUN is the answer and the
//     cycle is not yet the operator's call. An unoriented pair — two rows sharing
//     every timestamp, so there is no direction to ask about — means the pass
//     deliberately did not ask, and a rerun will not change that, so the
//     operator decides. Collapsing the two into one sentence had the transient
//     #699 failure reported as a missing chronology and answered with "delete an
//     edge".
type CycleOutcome string

const (
	// CycleKeptFirst: the edge in First's direction stands; the other is denied.
	CycleKeptFirst CycleOutcome = "kept-first"
	// CycleKeptSecond: the edge in Second's direction stands; the other is denied.
	CycleKeptSecond CycleOutcome = "kept-second"
	// CycleBothWithdrawn: neither direction is supported, so both edges are
	// denied.
	CycleBothWithdrawn CycleOutcome = "both-denied"
	// CycleNoVerdict: the pass asked and got nothing — an unparseable reply, or a
	// classify call that failed. Both edges stand and the next pass re-asks.
	CycleNoVerdict CycleOutcome = "no-verdict"
	// CycleUnoriented: both rows share updated_at AND created_at, so there is no
	// direction to ask about (#778) and the pass did not ask. Both edges stand,
	// and a rerun will not change that.
	CycleUnoriented CycleOutcome = "unoriented"
)

// cycleDecision is the working state of one cyclic pair while its single verdict
// is outstanding: the two live edges it holds, and which of them the pass asked
// about. It carries nothing else — the verdict's outcome is returned to the caller
// as the CyclicPair and the withdrawals, so there is no second copy of a decision
// to read back and no field a verdict could set without anyone consulting.
type cycleDecision struct {
	first  CyclicEdge
	second CyclicEdge
	// askedFirst is true when the Candidate this pair contributed ran in First's
	// direction, so a verdict that keeps "the direction it was asked about" is
	// about First.
	askedFirst bool
}

// keptOutcome is the outcome for a verdict that named ONE of the two live edges
// as the current direction: that edge stands and the other is withdrawn.
// namedIsFirst says which one the verdict named — the edge the pass asked about
// for a SUPERSEDES answer, and the OTHER one for a REVERSED answer, which is
// what "the older note is the current one" says about the pair.
func (c *cycleDecision) keptOutcome(namedIsFirst bool) CycleOutcome {
	if namedIsFirst {
		return CycleKeptFirst
	}
	return CycleKeptSecond
}

// edgeOf returns the live edge the pass asked about, and the other one.
func (c *cycleDecision) edgeOf() (judged, other CyclicEdge) {
	if c.askedFirst {
		return c.first, c.second
	}
	return c.second, c.first
}

// WithdrawnEdge is one edge the pass withdrew, or would withdraw under --apply.
// The reason is the report's content: an operator deciding whether to apply needs
// to see WHICH rule withdrew each edge, and "neither" and "vetoed" are different
// findings about the corpus.
type WithdrawnEdge struct {
	NewerID string
	OlderID string
	// Reason is the rule that withdrew the edge: "vetoed: …", "neither",
	// "causes" or "reversed".
	Reason string
	// Vetoed marks the row as settled by the deterministic veto rather than by
	// a harness answer, so the report can say which edges no model ever looked
	// at. The two are not the same kind of finding and a reader deciding whether
	// to apply needs to tell them apart.
	Vetoed bool
	// CausesSwept counts the 'causes' edges this row's withdrawal also dropped,
	// because a withdrawal that removes a second graph row and does not say so
	// is a report the operator cannot decide from. It is the PREDICTION in a dry
	// run and what the sweep MOVED under --apply. Read it as "the store moved no
	// live row" rather than "there was nothing to move": for a non-supersedes
	// relation InvalidateLink swallows a RowsAffected error and returns (0, nil)
	// with the write already committed (internal/memory/links.go), so 0 is the
	// count the store could confirm, not a promise about the graph. When the
	// sweep FAILED, SweepFailed is set instead — that state is unknown, and
	// reporting it as 0 would tell an operator who saw "would sweep 1" in the
	// dry run that nothing else was deleted.
	CausesSwept int
	// SweepFailed marks the row whose 'causes' sweep errored, so the report
	// says "unknown" where the truth is unknown instead of "0".
	SweepFailed bool
	// PredictionUnknown marks the row whose DRY-RUN 'causes' prediction is
	// missing, because the read that produces it failed (a dry run is the only
	// mode that reads it: under --apply the field is overwritten with what the
	// sweep actually moved). It is not SweepFailed — no sweep ran here — and
	// the report must not print a count of second deletions it never looked
	// for, or an operator decides about a deletion the preview never showed.
	PredictionUnknown bool
	// Written is true only when --apply actually invalidated the edge, so a
	// dry-run list can never be read as a change that happened.
	Written bool
	// TargetProjectID is the project the target lives in, and the follow-up is
	// scoped to IT rather than to the project the pass was run against. Those are
	// the same project for every target the pass loaded — its own edges — and
	// differ only for a target promoted to `_global`, whose resolved_at no
	// `ghost resolve <project> --reassess` could clear. See RepairableTargets.
	TargetProjectID string
}

// liveCausesPairs returns the [olderID, newerID] pairs carrying a live
// 'causes'/'llm' edge — the orientation 'causes' is written in (the cause
// precedes its effect), which is the reverse of a supersedes edge.
//
// It is read through each pair's OWN endpoints rather than through a
// project-scoped query, and that is not a style choice. The sweep deletes by id
// with no project predicate, so a pair whose older endpoint has LEFT the project
// — promoted to _global by `ghost reflect --promote-globals`, or moved by
// `ghost project merge` — is still swept, while a project-scoped read (which
// joins on the edge's source, and a 'causes' edge's source is the OLDER note)
// cannot see it. Reading the older endpoint's own links sees exactly the
// population the deletion can reach, which is the one the report has to be about.
func liveCausesPairs(ctx context.Context, store reassessStore, settled []judgedEdge) (map[[2]string]bool, error) {
	ids := make([]string, 0, len(settled))
	seen := make(map[string]bool, len(settled))
	for _, j := range settled {
		if seen[j.OlderID] {
			continue
		}
		seen[j.OlderID] = true
		ids = append(ids, j.OlderID)
	}
	out := make(map[[2]string]bool, len(settled))
	for _, id := range ids {
		links, err := store.GetLinks(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load links of %s: %w", id, err)
		}
		for _, l := range links {
			if l.Relation == string(RelationCauses) && l.SourceID == id {
				out[[2]string{l.SourceID, l.TargetID}] = true
			}
		}
	}
	return out, nil
}

// judgedEdge is the projection liveCausesPairs needs: the ids of one settled
// pair, with nothing else.
type judgedEdge struct {
	OlderID string
	NewerID string
}

// judged is one pair this pass has settled WITHOUT the classifier's answer, or
// with an answer that is a denial: a withdrawal, with the reason that withdrew
// it and whether the other relation is swept with it. Package-level rather than
// local to Reassess because settleCycle builds the cycle's rows, and a type a
// free function cannot name is a type that function cannot be.
type judged struct {
	cand   Candidate
	reason string // non-empty means already withdrawn, with this reason
	vetoed bool   // settled without a harness call
	// sweep arms the other-relation sweep: every withdrawal that DENIES a relation
	// drops the 'causes' edge too, exactly as Run does. A CAUSES verdict affirms
	// that relation instead, so its row sweeps nothing.
	sweep bool
}

// retryReporter is the optional retry tally a Classifier may carry.
// RelationClassifier counts the calls it repeated; a Classifier that decides
// verdicts itself (a test mock) has no such method, and the count is then zero —
// nothing is invented for a mock.
type retryReporter interface{ Retries() int }

// retriesOf reports how many calls cls repeated after a failure, or 0 for a
// Classifier that does not count them.
func retriesOf(cls Classifier) int {
	if r, ok := cls.(retryReporter); ok {
		return r.Retries()
	}
	return 0
}

// Reassess re-judges every live 'supersedes'/'llm' edge in the project with the
// current rules and returns the edges the pass withdrew, or would withdraw with
// --apply. A dry run (apply=false) writes nothing.
//
// A classifier call that keeps failing is NOT fatal to the rows a deterministic
// rule already settled (#699). The veto needs no harness, and withdrawing an edge
// is the safe direction — it only puts the older memory back into ranked
// injection, and resolve's own repair pass can re-stamp it. Abandoning them over
// one dead `opencode run` is how a real rehearsal withdrew nothing at all, 76 of
// 87 edges waiting for a rerun. So the pass applies the settled withdrawals,
// records the pairs the classifier never answered in ReassessResult.Unjudged
// with their edges left live, and STILL returns an error: a partial repair is
// partial, and a zero exit would read as "done". The same treatment covers a
// reply whose verdict count does not match the pairs asked about — both are the
// same state here, no verdict for any open pair.
//
// The partial repair is per CALL, not per pass (#808). A project's open pairs are
// chunked across several harness calls, and a call that fails says nothing about
// the pairs the calls before it already answered: those verdicts are complete,
// parsed and indexed by pair number, and there is nothing downstream that needs
// the whole set at once — a cycle contributes one candidate and so lives in one
// chunk, and an ordinary edge's verdict is read on its own. So the chunks that
// answered are applied like any other verdicts and only the failed call's pairs
// are reported unjudged; the error, and the non-zero exit it produces, are
// unchanged. What #699's rehearsal lost 76 edges to was a failure with nothing
// decided behind it, and that case is unchanged: a failure on the FIRST chunk
// still decides nothing.
//
// An unparseable verdict is not an error at all: the edge stays, and the pair is
// counted as Unclassified, so a later pass can ask again.
//
// A failed invalidation IS fatal, and like the classify failure it is a case
// where the pass returns an error AND a result: the writes before it already
// landed, they cannot be un-landed, and a later pass will not see those edges
// again. So the caller gets the count, the per-edge list and the log line, and
// the non-zero exit says the repair is incomplete. A classify failure and a write
// failure can both be real in one run, so they are joined rather than one
// replacing the other.
//
// A read of the 'causes' edges the sweep would take fails the pass too, but it
// does not stop it, and only a DRY RUN makes it: that read exists solely to
// produce the per-row prediction, and under --apply every count comes from the
// sweep itself. So a dry run that cannot read it marks the row's second deletion
// unknown rather than printing a count it does not have, and --apply never reads
// it at all — a discarded read cannot be allowed to fail a repair that
// completed.
//
// Scope is honoured as an exemption, not a verdict. A scope-conflicting edge
// asserts no replacement — the ordinary pass leaves it in the graph and every
// reader ignores it — so this pass does not judge it and does not delete it.
// Withdrawing it would be removing graph history over a rule about which pairs
// may be linked, not about whether this one was a supersession. Such edges are
// counted as Skipped, along with an edge whose endpoint no longer exists.
func Reassess(ctx context.Context, store reassessStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (ReassessResult, []WithdrawnEdge, error) {
	var res ReassessResult
	links, err := store.LinksByRelationSource(ctx, projectID, string(RelationSupersedes), "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes links: %w", err)
	}
	res.Loaded = len(links)
	if len(links) == 0 {
		return res, nil, nil
	}

	// The endpoints' scopes, read from the store rather than the link, because
	// the link row carries neither.
	ids := make([]string, 0, len(links)*2)
	seen := make(map[string]bool, len(links)*2)
	for _, l := range links {
		for _, id := range []string{l.SourceID, l.TargetID} {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes edge endpoints: %w", err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}

	// Settle the free decisions first — the veto, the missing endpoints and the
	// scope exemption — so the harness is only asked about pairs a rule did not
	// already answer.
	var open []Candidate
	var settled []judged
	// openCycles[i] is the cyclic pair that open[i] asks about, or nil for an
	// ordinary edge. It is parallel to `open` rather than indexed into it so a
	// candidate cannot be attached to a decision that belongs to another pair.
	openCycles := make([]*cycleDecision, 0, len(links))
	// The live edges, grouped by the UNORDERED pair, because a pair is the unit
	// this pass judges (#778) and a pair with two live edges is a CYCLE. The
	// groups keep the store's order and are never read as a direction: which edge
	// of a cycle the pass asks about comes from the timestamps, not from which row
	// the query returned first, because that order is not a promise.
	groups := make([][]memory.Link, 0, len(links))
	groupIndex := make(map[pairKey]int, len(links))
	for _, l := range links {
		key := newPairKey(l.SourceID, l.TargetID)
		if i, ok := groupIndex[key]; ok {
			groups[i] = append(groups[i], l)
			continue
		}
		groupIndex[key] = len(groups)
		groups = append(groups, []memory.Link{l})
	}

	for _, g := range groups {
		l := g[0]
		newer, okNewer := byID[l.SourceID]
		older, okOlder := byID[l.TargetID]
		if !okNewer || !okOlder {
			// memory_links cascades with its memories, so this should be
			// unreachable; if it is reached, the pair cannot be judged on its own
			// merits, and the failure modes are not equal.
			res.Skipped += len(g)
			continue
		}
		if memory.ScopesConflict(newer.Scope, older.Scope) {
			res.Skipped += len(g)
			if logger != nil {
				logger.Debug("supersede reassess: leaving a scope-conflicting edge alone",
					"newer", l.SourceID, "older", l.TargetID)
			}
			continue
		}
		// A CYCLE: the same pair live in both directions, each edge asserting
		// what the other denies, and each demoting the endpoint the other
		// promotes. It is ONE question, asked in the direction the timestamps
		// give — never the store's row order — and a verdict that names a
		// direction keeps that edge and withdraws its reverse.
		if len(g) > 1 {
			dec := &cycleDecision{
				first:  CyclicEdge{SourceID: g[0].SourceID, TargetID: g[0].TargetID},
				second: CyclicEdge{SourceID: g[1].SourceID, TargetID: g[1].TargetID},
			}
			cyc := CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleUnoriented}
			// The direction to ask about, and whether one is knowable at all. A
			// pair tying on both timestamps is the #778 rule, and it refuses
			// rather than falls back: the two rows carry no chronology, so there
			// is nothing to ask "which is newer" about. Nothing is withdrawn and
			// the cycle is reported, because the operator's --withdraw is the
			// repair for a pair this pass cannot even frame a question about.
			cNewer, cOlder, oriented := orient(newer, older)
			if !oriented {
				res.Cyclic = append(res.Cyclic, cyc)
				res.Unoriented++
				if logger != nil {
					logger.Info("supersede reassess: a cyclic pair whose two rows share both timestamps; no edge withdrawn",
						"a", dec.first.SourceID, "b", dec.first.TargetID)
				}
				continue
			}
			dec.askedFirst = dec.first.SourceID == cNewer.ID
			cand := Candidate{
				NewerID: cNewer.ID, NewerContent: cNewer.Content, NewerCreatedAt: cNewer.CreatedAt,
				OlderID: cOlder.ID, OlderContent: cOlder.Content, OlderCreatedAt: cOlder.CreatedAt,
				Similarity: g[0].Strength,
			}
			// The veto is deliberately NOT applied to a cycle, and that is a
			// decision rather than an oversight. VetoSupersede asks one
			// orientation's question — does the OLDER note's rule get retired by
			// the newer one — and a cycle has two orientations over the same two
			// bodies, so settling either one here would withdraw an edge on a
			// direction the pass chose rather than one the graph asserts. The
			// classifier is asked instead, and its answer is read as a direction.
			open = append(open, cand)
			openCycles = append(openCycles, dec)
			continue
		}
		cand := Candidate{
			NewerID: newer.ID, NewerContent: newer.Content, NewerCreatedAt: newer.CreatedAt,
			OlderID: older.ID, OlderContent: older.Content, OlderCreatedAt: older.CreatedAt,
			Similarity: l.Strength,
		}
		if reason, vetoed := VetoSupersede(cand); vetoed {
			res.Vetoed++
			if logger != nil {
				logger.Debug("supersede reassess: the older note states a rule this edge does not retire",
					"newer", cand.NewerID, "older", cand.OlderID, "reason", reason)
			}
			settled = append(settled, judged{cand: cand, reason: "vetoed: " + reason, vetoed: true, sweep: true})
			continue
		}
		open = append(open, cand)
		openCycles = append(openCycles, nil)
	}

	// A failure here is recorded, not raised: the settled rows below do not
	// depend on it, and 87 edges waiting on a rerun because one call died is the
	// #699 failure. The pairs move to Unjudged with their edges live, and the
	// error is returned at the end so the exit still says "rerun me".
	//
	// Every failure in this pass is recorded rather than raised, so they are all
	// joined at the end and the summary is written once, on the single exit that
	// is left. A run that recorded a partial state has the most to explain.
	var fail error
	if len(open) > 0 {
		verdicts, err := cls.ClassifyBatch(ctx, open)
		if err == nil && len(verdicts) != len(open) {
			err = fmt.Errorf("classifier returned %d verdict(s) for %d pair(s)", len(verdicts), len(open))
		}
		// How much of the question was answered, which is not always none of it
		// (#808). ClassifyBatch chunks a project's open pairs across several
		// harness calls, and a transport failure in one of them leaves the
		// chunks before it holding complete, parsed, number-indexed verdicts.
		// Nothing downstream needs the whole set — a cycle is ONE candidate and
		// so lives in a single chunk, and an ordinary edge's verdict is read on
		// its own — so the answered prefix is settled below and only the pairs
		// the failed call carried are reported unjudged. A reply whose verdict
		// count does not match is NOT a partial answer: the mapping from reply
		// to pair is exactly what is in doubt there, so nothing is settled.
		answered := len(open)
		if err != nil {
			answered = answeredPrefix(verdicts, len(open), err)
		}
		switch {
		case err != nil:
			if answered > 0 {
				settleOpen(&res, &settled, open[:answered], verdicts[:answered], openCycles[:answered])
			}
			fail = fmt.Errorf("classify %d live supersedes edge(s): %w", len(open), err)
			res.Unjudged = unjudgedPairs(open[answered:])
			// A cycle whose question was on the failed call is reported NO-VERDICT
			// alongside the ordinary unjudged pairs, and for the same reason: the
			// call answered nothing about any pair it carried, so no edge of the
			// cycle moves. Silently omitting the cycle from a report that is
			// otherwise listing every unjudged pair would leave the operator
			// believing its two edges were left alone on purpose.
			//
			// NoVerdict, not Unoriented, and the difference is the operator's next
			// step: the harness died, so the report tells them to RE-RUN this pass.
			// Only a pair with no knowable direction is theirs to settle, and that
			// one is the report's --withdraw commands.
			for _, dec := range openCycles[answered:] {
				if dec != nil {
					res.Cyclic = append(res.Cyclic, CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleNoVerdict})
				}
			}
			if logger != nil {
				logger.Warn("supersede reassess: the classifier failed; the edges it would have judged stand",
					"answered", answered, "unjudged", len(res.Unjudged), "settled", len(settled), "error", err)
			}
		default:
			settleOpen(&res, &settled, open, verdicts, openCycles)
		}
	}

	// The live 'causes' edges on the pairs this pass is about to withdraw, read
	// once and before any write, so a dry run can say how many rows each
	// withdrawal would take with it. An operator deciding whether to pass
	// --apply is deciding about that second deletion too.
	rows := make([]judgedEdge, 0, len(settled))
	for _, j := range settled {
		rows = append(rows, judgedEdge{OlderID: j.cand.OlderID, NewerID: j.cand.NewerID})
	}
	// The 'causes' prediction is a DRY-RUN read, and only a dry run's: under
	// --apply every row's CausesSwept is overwritten with what the sweep actually
	// moved, so reading it there would be a GetLinks per settled older id whose
	// answer is discarded — and a discarded read that could fail the run would be
	// a repair reporting a fault it did not have. When the read does fail, the
	// dry run's rows are marked unknown rather than printed with a count of
	// second deletions nobody looked for, and the error is returned.
	var causesPairs map[[2]string]bool
	predictionFailed := false
	if !apply {
		var err error
		causesPairs, err = liveCausesPairs(ctx, store, rows)
		if err != nil {
			fail = errors.Join(fail, err)
			causesPairs = nil
			predictionFailed = true
		}
	}

	withdrawn := make([]WithdrawnEdge, 0, len(settled))
	for _, j := range settled {
		w := WithdrawnEdge{
			NewerID: j.cand.NewerID, OlderID: j.cand.OlderID,
			Reason: j.reason, Vetoed: j.vetoed,
			// From the same byID the pass already loaded the pair from, so this
			// costs no read: the follow-up is about the memory being un-hidden, and
			// a repair scoped to any other project's pool cannot reach it.
			TargetProjectID: byID[j.cand.OlderID].ProjectID,
		}
		if j.sweep {
			// A PREDICTION, for a dry run only: the row says how many 'causes'
			// edges this withdrawal would take with it. Under --apply the field
			// is overwritten with what the sweep actually moved, because a report
			// that claims a deletion which did not happen is the one thing this
			// pass cannot be for. And when the read itself failed there is no
			// prediction to print at all, so the row says the count is unknown
			// rather than 0 — an operator who is about to apply has to be able to
			// tell "there is nothing else to delete" from "nobody looked".
			switch {
			case predictionFailed:
				w.PredictionUnknown = true
				res.CausesPredictionFailed++
			case causesPairs[[2]string{j.cand.OlderID, j.cand.NewerID}]:
				w.CausesSwept = 1
			}
		}
		if apply {
			n, err := store.InvalidateLink(ctx, w.NewerID, w.OlderID, string(RelationSupersedes))
			if err != nil {
				// The edges already withdrawn in this loop are gone, and a
				// re-run cannot find them again (LinksByRelationSource returns
				// live edges only). So the partial list and the count go back
				// WITH the error and the summary is logged here: a repair whose
				// whole justification is auditability must not vanish because
				// its Nth write failed. Joined, not assigned: a classify
				// failure from earlier in this run is still true, and the
				// unjudged edges it left behind are the other half of what the
				// operator has to fix.
				fail = errors.Join(fail, fmt.Errorf("withdraw supersedes link %s→%s: %w", w.NewerID, w.OlderID, err))
				break
			}
			if n == 0 {
				// A concurrent pass withdrew it first. The edge is gone either
				// way, so this is not a failure — but it is not this pass's
				// write, and the report says so.
				w.Written = false
			} else {
				w.Written = true
				res.Withdrawn++
			}
		}
		// Appended BEFORE the sweep, so a sweep that fails cannot drop the row
		// for an edge whose own withdrawal already landed: the count and the
		// list would then disagree, which is the invisibility this pass exists
		// to remove.
		withdrawn = append(withdrawn, w)
		if apply && j.sweep {
			// The both-relations sweep Run performs on the same verdicts: a
			// 'causes' link pointing INTO a note this pass just decided is
			// still a standing rule asserts the opposite of that decision.
			// InvalidateLink writes no history row for a non-supersedes
			// relation, so this leaves the audit exactly as Run leaves it.
			n, err := store.InvalidateLink(ctx, w.OlderID, w.NewerID, string(RelationCauses))
			if err != nil {
				// The count is not taken on this path: a definite number would be
				// a statement about the graph nobody can make after a failed
				// write, and the PREDICTION that is in the field is not a count
				// of what moved. The row says the outcome is unknown instead —
				// the report prints that in place of any count — and the summary
				// counts the failures, so the report and the exit status agree.
				w.CausesSwept = 0
				w.SweepFailed = true
				withdrawn[len(withdrawn)-1] = w
				res.CausesSweepFailed++
				fail = errors.Join(fail, fmt.Errorf("withdraw causes link %s→%s: %w", w.OlderID, w.NewerID, err))
				break
			}
			// What actually moved, not what the row predicted.
			w.CausesSwept = int(n)
			withdrawn[len(withdrawn)-1] = w
			res.CausesWithdrawn += int(n)
		}
	}

	if !apply {
		// Nothing was moved, so the count is the PREDICTION — the same number an
		// --apply run reports unless a concurrent pass takes one of those edges
		// first. A dry run whose summary said 0 above rows marked [+1 causes
		// edge] would be the invisibility the per-row marker exists to remove,
		// one line up. A row whose prediction is unknown contributes nothing to
		// it: that is what the marker, and CausesPredictionFailed, are for.
		for _, w := range withdrawn {
			res.CausesWithdrawn += w.CausesSwept
		}
	}
	if logger != nil {
		logger.Info("supersede reassess",
			"loaded", res.Loaded, "skipped", res.Skipped, "vetoed", res.Vetoed,
			"confirmed", res.Confirmed, "neither", res.Neither, "causes", res.Causes,
			"reversed", res.Reversed, "unknown", res.Unclassified, "unjudged", len(res.Unjudged),
			"cyclic", len(res.Cyclic), "unoriented", res.Unoriented,
			"withdrawn", res.Withdrawn, "causes_withdrawn", res.CausesWithdrawn,
			"causes_sweep_failed", res.CausesSweepFailed,
			"causes_prediction_failed", res.CausesPredictionFailed,
			"retries", retriesOf(cls), "failed", fail != nil)
	}
	if fail != nil {
		return res, withdrawn, fail
	}
	return res, withdrawn, nil
}

// settleOpen reads one verdict per pair into the counts it moves and the
// withdrawals it implies. It is a named function because Reassess now calls it
// twice — once for the whole set and once for the prefix a failed classify call
// left answered (#808) — and the two must settle a verdict by the same rules,
// which a copy of this switch would not guarantee.
//
// The three slices are parallel and are the SAME slice of the pass's work:
// pairs[i] is what verdicts[i] is about, and cycles[i] is the cycle that pair
// contributed (nil for an ordinary edge). A cycle is settled by settleCycle,
// which reads the verdict as a direction, and an ordinary edge by the four-way
// switch below. A missing verdict is neither a denial nor a withdrawal in either
// path: the edge stays, the pair is counted, and the next pass asks again.
func settleOpen(res *ReassessResult, settled *[]judged, pairs []Candidate, verdicts []Relation, cycles []*cycleDecision) {
	for i, c := range pairs {
		if dec := cycles[i]; dec != nil {
			*settled = append(*settled, settleCycle(dec, verdicts[i], res)...)
			continue
		}
		switch verdicts[i] {
		case RelationSupersedes:
			res.Confirmed++
		case RelationCauses:
			res.Causes++
			*settled = append(*settled, judged{cand: c, reason: "causes: the older note is still independently true"})
		case RelationReversed:
			res.Reversed++
			*settled = append(*settled, judged{cand: c, reason: "reversed: the older note is the current one", sweep: true})
		case RelationNeither:
			res.Neither++
			*settled = append(*settled, judged{cand: c, reason: "neither: both notes are still true", sweep: true})
		default:
			// Relation("") and any invalid value are a missing judgment, not a
			// denial: the edge stays, counted, and the pair is offered again.
			res.Unclassified++
		}
	}
}

// unjudgedPairs projects the open pairs no verdict arrived for: the tail a
// failed classify call carried, and — when the first call itself failed — every
// open pair, because that call answered nothing about anything it carried. The
// next pass re-asks all of them either way (nothing is cached for an unanswered
// edge), so this list is what a rerun still owes rather than a claim about which
// of them the harness had looked at.
func unjudgedPairs(open []Candidate) []UnjudgedPair {
	out := make([]UnjudgedPair, 0, len(open))
	for _, c := range open {
		out = append(out, UnjudgedPair{NewerID: c.NewerID, OlderID: c.OlderID})
	}
	return out
}

// settleCycle reads ONE verdict about a pair live in both directions into the
// withdrawals it implies, and records the pair on the result so the report can
// name the cycle whether or not anything moved.
//
// The four branches are CycleOutcome's table, read as a direction: a verdict
// that NAMES one of the two live edges as current keeps it and withdraws the
// other, and a verdict that denies the same-fact replacement withdraws both. The
// default is the important one — a missing verdict withdraws NEITHER, because a
// cycle is the one state where withdrawing half of it would leave a live edge
// this pass never judged, which is indistinguishable from the cycle it was asked
// to repair.
func settleCycle(dec *cycleDecision, verdict Relation, res *ReassessResult) []judged {
	cyc := CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleNoVerdict}
	judgedEdge, other := dec.edgeOf()
	reverse := func(e CyclicEdge) Candidate {
		return Candidate{NewerID: e.SourceID, OlderID: e.TargetID}
	}
	// The 'causes' sweep rides on the same verdicts it rides on everywhere else
	// in this pass: a denial sweeps, a CAUSES verdict affirms that relation. A
	// cycle's rows are ordinary edges in that respect — the two edges of a pair
	// are two rows, and each row's withdrawal sweeps the one 'causes' edge that
	// contradicts it.
	var out []judged
	switch verdict {
	case RelationSupersedes:
		res.Confirmed++
		cyc.Outcome = dec.keptOutcome(dec.askedFirst)
		out = append(out, judged{
			cand:   reverse(other),
			reason: "the reverse of the direction the verdict confirmed: this edge asserts the opposite, and both directions at once demote both endpoints",
			sweep:  true,
		})
	case RelationReversed:
		res.Reversed++
		cyc.Outcome = dec.keptOutcome(!dec.askedFirst)
		out = append(out, judged{
			cand:   reverse(judgedEdge),
			reason: "reversed: the note this edge supersedes is the current one, so this edge runs against the pair and the reverse is the direction that stands",
			sweep:  true,
		})
	case RelationCauses:
		res.Causes++
		cyc.Outcome = CycleBothWithdrawn
		out = append(out,
			judged{cand: reverse(judgedEdge), reason: "causes: the older note is still independently true"},
			judged{cand: reverse(other), reason: "causes: the older note is still independently true, and its reverse asserts the opposite"},
		)
	case RelationNeither:
		res.Neither++
		cyc.Outcome = CycleBothWithdrawn
		out = append(out,
			judged{cand: reverse(judgedEdge), reason: "neither: both notes are still true", sweep: true},
			judged{cand: reverse(other), reason: "neither: both notes are still true, and its reverse asserts the opposite", sweep: true},
		)
	default:
		// A missing verdict, not a denial. Counted the way every other missing
		// verdict in this pass is counted, and the pair is still reported: the
		// cycle is live and demoting both endpoints, which is a finding about the
		// graph whatever this pass could or could not decide.
		res.Unclassified++
	}
	res.Cyclic = append(res.Cyclic, cyc)
	return out
}
