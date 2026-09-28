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
// have not changed: skip-if-unchanged holds it quiet forever.
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
	// Unjudged names the pairs the classifier produced no verdict for, because
	// the call failed on both attempts or the reply's verdict count did not
	// match the pairs asked about (#699). Their edges are LEFT ALIVE: nothing
	// was decided about them, and the next pass re-asks them, so this is the
	// list of what a rerun still owes. It is a list rather than a count because
	// "a project of 87 edges, 6 of them unjudged" is a different thing for an
	// operator than "6 unjudged" with no way to find them — and one failed call
	// can be the whole set.
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
	// Written is true only when --apply actually invalidated the edge, so a
	// dry-run list can never be read as a change that happened.
	Written bool
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
	type judged struct {
		cand   Candidate
		reason string // non-empty means already withdrawn, with this reason
		vetoed bool   // settled without a harness call
		// sweep arms the other-relation sweep: every withdrawal that DENIES a
		// relation drops the 'causes' edge too, exactly as Run does. A CAUSES
		// verdict affirms that relation instead, so its row sweeps nothing.
		sweep bool
	}
	var open []Candidate
	var settled []judged
	for _, l := range links {
		newer, okNewer := byID[l.SourceID]
		older, okOlder := byID[l.TargetID]
		if !okNewer || !okOlder {
			// memory_links cascades with its memories, so this should be
			// unreachable; if it is reached, the pair cannot be judged on its own
			// merits, and the failure modes are not equal.
			res.Skipped++
			continue
		}
		if memory.ScopesConflict(newer.Scope, older.Scope) {
			res.Skipped++
			if logger != nil {
				logger.Debug("supersede reassess: leaving a scope-conflicting edge alone",
					"newer", l.SourceID, "older", l.TargetID)
			}
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
	}

	// A failure here is recorded, not raised: the settled rows below do not
	// depend on it, and 87 edges waiting on a rerun because one call died is the
	// #699 failure. The pairs move to Unjudged with their edges live, and the
	// error is returned at the end so the exit still says "rerun me".
	var fail error
	if len(open) > 0 {
		verdicts, err := cls.ClassifyBatch(ctx, open)
		if err == nil && len(verdicts) != len(open) {
			err = fmt.Errorf("classifier returned %d verdict(s) for %d pair(s)", len(verdicts), len(open))
		}
		switch {
		case err != nil:
			fail = fmt.Errorf("classify %d live supersedes edge(s): %w", len(open), err)
			res.Unjudged = unjudgedPairs(open)
			if logger != nil {
				logger.Warn("supersede reassess: the classifier failed; the edges it would have judged stand",
					"unjudged", len(res.Unjudged), "settled", len(settled), "error", err)
			}
		default:
			for i, c := range open {
				switch verdicts[i] {
				case RelationSupersedes:
					res.Confirmed++
				case RelationCauses:
					res.Causes++
					settled = append(settled, judged{cand: c, reason: "causes: the older note is still independently true"})
				case RelationReversed:
					res.Reversed++
					settled = append(settled, judged{cand: c, reason: "reversed: the older note is the current one", sweep: true})
				case RelationNeither:
					res.Neither++
					settled = append(settled, judged{cand: c, reason: "neither: both notes are still true", sweep: true})
				default:
					// Relation("") and any invalid value are a missing judgment, not a
					// denial: the edge stays, counted, and the pair is offered again.
					res.Unclassified++
				}
			}
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
	causesPairs, err := liveCausesPairs(ctx, store, rows)
	if err != nil {
		return res, nil, err
	}

	withdrawn := make([]WithdrawnEdge, 0, len(settled))
	for _, j := range settled {
		w := WithdrawnEdge{NewerID: j.cand.NewerID, OlderID: j.cand.OlderID, Reason: j.reason, Vetoed: j.vetoed}
		if j.sweep {
			// A PREDICTION, for a dry run only: the row says how many 'causes'
			// edges this withdrawal would take with it. Under --apply the field
			// is overwritten with what the sweep actually moved, because a report
			// that claims a deletion which did not happen is the one thing this
			// pass cannot be for.
			if causesPairs[[2]string{j.cand.OlderID, j.cand.NewerID}] {
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
		// one line up. Ahead of the failure return, not after it: a dry run that
		// also hit a classify failure still reports the rows it is about to
		// print, because that is the pass the operator is being asked about.
		for _, w := range withdrawn {
			res.CausesWithdrawn += w.CausesSwept
		}
	}
	if logger != nil {
		logger.Info("supersede reassess",
			"loaded", res.Loaded, "skipped", res.Skipped, "vetoed", res.Vetoed,
			"confirmed", res.Confirmed, "neither", res.Neither, "causes", res.Causes,
			"reversed", res.Reversed, "unknown", res.Unclassified, "unjudged", len(res.Unjudged),
			"withdrawn", res.Withdrawn, "causes_withdrawn", res.CausesWithdrawn,
			"causes_sweep_failed", res.CausesSweepFailed,
			"retries", retriesOf(cls), "failed", fail != nil)
	}
	if fail != nil {
		return res, withdrawn, fail
	}
	return res, withdrawn, nil
}

// unjudgedPairs projects the open pairs a failed classify call left without a
// verdict. It is every open pair, not the subset the harness named: a call that
// failed answers nothing about anything it carried, and the next pass re-asks all
// of them anyway (nothing is cached for an unanswered edge), so a narrower list
// would be a claim the pass cannot make.
func unjudgedPairs(open []Candidate) []UnjudgedPair {
	out := make([]UnjudgedPair, 0, len(open))
	for _, c := range open {
		out = append(out, UnjudgedPair{NewerID: c.NewerID, OlderID: c.OlderID})
	}
	return out
}
