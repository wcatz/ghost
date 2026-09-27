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
// belongs.
package supersede

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// reassessStore is the subset of *memory.Store the repair pass needs; narrowed
// for testability. It reads the live edges and their endpoints' scopes, and it
// writes nothing but the invalidation.
type reassessStore interface {
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
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
	Causes       int // came back CAUSES: the older note is still true, so the edge goes
	Reversed     int // came back REVERSED: the edge runs against the pair, so it goes
	Unclassified int // unparseable verdict; the edge is left alone and re-offered
	Withdrawn    int // edges actually invalidated (0 in dry-run)
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
	// Written is true only when --apply actually invalidated the edge, so a
	// dry-run list can never be read as a change that happened.
	Written bool
}

// Reassess re-judges every live 'supersedes'/'llm' edge in the project with the
// current rules and returns the edges the pass withdrew, or would withdraw with
// --apply. A dry run (apply=false) writes nothing.
//
// A classifier error on any batch is fatal and withdraws nothing: a partial
// repair would drop edges on a judgment the harness never made. An unparseable
// verdict is not fatal and withdraws nothing — the edge stays and the pair is
// counted as Unclassified, so a later pass can ask again. A failed invalidation
// is fatal for the same reason a failed clear is fatal in resolve's repair: a
// repair that did not happen must not be reported as one.
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
			settled = append(settled, judged{cand: cand, reason: "vetoed: " + reason})
			continue
		}
		open = append(open, cand)
	}

	if len(open) > 0 {
		verdicts, err := cls.ClassifyBatch(ctx, open)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d live supersedes edge(s): %w", len(open), err)
		}
		if len(verdicts) != len(open) {
			return res, nil, fmt.Errorf("classify %d live supersedes edge(s): classifier returned %d verdict(s)", len(open), len(verdicts))
		}
		for i, c := range open {
			switch verdicts[i] {
			case RelationSupersedes:
				res.Confirmed++
			case RelationCauses:
				res.Causes++
				settled = append(settled, judged{cand: c, reason: "causes: the older note is still independently true"})
			case RelationReversed:
				res.Reversed++
				settled = append(settled, judged{cand: c, reason: "reversed: the older note is the current one"})
			case RelationNeither:
				settled = append(settled, judged{cand: c, reason: "neither: both notes are still true"})
			default:
				// Relation("") and any invalid value are a missing judgment, not a
				// denial: the edge stays, counted, and the pair is offered again.
				res.Unclassified++
			}
		}
	}

	withdrawn := make([]WithdrawnEdge, 0, len(settled))
	for _, j := range settled {
		w := WithdrawnEdge{NewerID: j.cand.NewerID, OlderID: j.cand.OlderID, Reason: j.reason}
		if apply {
			n, err := store.InvalidateLink(ctx, w.NewerID, w.OlderID, string(RelationSupersedes))
			if err != nil {
				return res, nil, fmt.Errorf("withdraw supersedes link %s→%s: %w", w.NewerID, w.OlderID, err)
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
		withdrawn = append(withdrawn, w)
	}

	if logger != nil {
		logger.Info("supersede reassess",
			"loaded", res.Loaded, "skipped", res.Skipped, "vetoed", res.Vetoed,
			"confirmed", res.Confirmed, "causes", res.Causes, "reversed", res.Reversed,
			"unknown", res.Unclassified, "withdrawn", res.Withdrawn)
	}
	return res, withdrawn, nil
}
