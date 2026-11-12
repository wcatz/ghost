// The repair pass: re-judge the 'supersedes' edges already in the graph under
// the current rules, and withdraw the ones those rules no longer support.
package supersede

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

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
	Loaded                 int // live 'supersedes'/'llm' edges the pass considered
	Skipped                int // edges it does not judge: an endpoint is gone, or the pair is scope-conflicting
	Vetoed                 int // settled by the deterministic imperative veto, no classifier call
	Confirmed              int // came back SUPERSEDES: the edge stands
	Neither                int // came back NEITHER: both notes are still true, so the edge goes
	Causes                 int // came back CAUSES: the older note is still true, so the edge goes
	Reversed               int // came back REVERSED: the edge runs against the pair, so it goes
	Unclassified           int // unparseable verdict; the edge is left alone and re-offered
	Withdrawn              int // supersedes edges actually invalidated (0 in dry-run)
	CausesWithdrawn        int
	CausesSweepFailed      int
	Unoriented             int
	CausesPredictionFailed int
	Cyclic                 []CyclicPair
	Unjudged               []UnjudgedPair
	Consensus              int
	NotAgreed              int
	Disputed               []Disputed
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
	First   CyclicEdge
	Second  CyclicEdge
	Outcome CycleOutcome
}

// CycleOutcome is what a pass decided about a pair live in BOTH directions. The
// values are a DECISION and not a sentence, deliberately: the report is where the
// wording lives, because only the report knows whether this was a dry run, and a
// constant that spelled out "was withdrawn" would put a past-tense claim under a
// list of "would withdraw" rows (#780's review). Each value is a claim the pass
// can make from the answers it received — four of them from one verdict, and the
// fifth from the N of them under a consensus gate:
type CycleOutcome string

const (
	CycleKeptFirst     CycleOutcome = "kept-first"
	CycleKeptSecond    CycleOutcome = "kept-second"
	CycleBothWithdrawn CycleOutcome = "both-denied"
	CycleNoVerdict     CycleOutcome = "no-verdict"
	CycleUnoriented    CycleOutcome = "unoriented"
	CycleNotAgreed     CycleOutcome = "not-agreed"
)

type cycleDecision struct {
	first      CyclicEdge
	second     CyclicEdge
	askedFirst bool
}

func (c *cycleDecision) keptOutcome(namedIsFirst bool) CycleOutcome {
	if namedIsFirst {
		return CycleKeptFirst
	}
	return CycleKeptSecond
}

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
	NewerID           string
	OlderID           string
	Reason            string
	Vetoed            bool
	CausesSwept       int
	SweepFailed       bool
	PredictionUnknown bool
	Written           bool
	TargetProjectID   string
}

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
			return nil, fmt.Errorf("load links of %s: %w", memory.RefusalArg("memory_id", id), err)
		}
		for _, l := range links {
			if l.Relation == string(RelationCauses) && l.SourceID == id {
				out[[2]string{l.SourceID, l.TargetID}] = true
			}
		}
	}
	return out, nil
}

type judgedEdge struct {
	OlderID string
	NewerID string
}

type judged struct {
	cand   Candidate
	reason string // non-empty means already withdrawn, with this reason
	vetoed bool   // settled without a harness call
	sweep  bool
}

type retryReporter interface{ Retries() int }

func retriesOf(cls Classifier) int {
	if r, ok := cls.(retryReporter); ok {
		return r.Retries()
	}
	return 0
}

// ReassessOptions carries ReassessWith's per-call decisions: Apply (write what
// the verdicts decide) and Consensus (how many passes must agree before a live
// edge moves). See ReassessWith for why both are here and what the gate does to
// the pass. The zero value is today's ungated repair.
type ReassessOptions struct {
	Apply     bool
	Consensus int
}

// Reassess re-judges every live 'supersedes'/'llm' edge in the project with the
// current rules and returns the edges the pass withdrew, or would withdraw with
// --apply. A dry run (apply=false) writes nothing. It is ReassessWith with no
// consensus: one verdict per edge, which is what `--reassess` has always meant.
func Reassess(ctx context.Context, store reassessStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (ReassessResult, []WithdrawnEdge, error) {
	return ReassessWith(ctx, store, cls, projectID, ReassessOptions{Apply: apply}, logger)
}

// ReassessWith is Reassess with the per-call decisions in one value, and the
// gate it adds is #862's: since the creation pass stopped withdrawing (#845) this
// repair is the only path that DELETES a live 'supersedes' edge, and it was the
// one reading a single verdict per edge while `--consensus` was refused beside it.
func ReassessWith(ctx context.Context, store reassessStore, cls Classifier, projectID string, opts ReassessOptions, logger *slog.Logger) (ReassessResult, []WithdrawnEdge, error) {
	passes := opts.Consensus
	if passes < 1 {
		passes = 1
	}
	apply := opts.Apply
	var res ReassessResult
	res.Consensus = passes
	links, err := store.LinksByRelationSource(ctx, projectID, string(RelationSupersedes), "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes links: %w", err)
	}
	res.Loaded = len(links)
	if len(links) == 0 {
		return res, nil, nil
	}

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

	var open []Candidate
	var settled []judged
	openCycles := make([]*cycleDecision, 0, len(links))
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
			res.Skipped += len(g)
			continue
		}
		if memory.ScopesConflict(newer.Scope, older.Scope) {
			res.Skipped += len(g)
			if logger != nil {
				logger.Debug("supersede reassess: leaving a scope-conflicting edge alone",
					"newer", memory.RefusalArg("memory_id", l.SourceID), "older", memory.RefusalArg("memory_id", l.TargetID))
			}
			continue
		}
		if len(g) > 1 {
			dec := &cycleDecision{
				first:  CyclicEdge{SourceID: g[0].SourceID, TargetID: g[0].TargetID},
				second: CyclicEdge{SourceID: g[1].SourceID, TargetID: g[1].TargetID},
			}
			cyc := CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleUnoriented}
			cNewer, cOlder, oriented := orient(newer, older)
			if !oriented {
				res.Cyclic = append(res.Cyclic, cyc)
				res.Unoriented++
				if logger != nil {
					logger.Info("supersede reassess: a cyclic pair whose two rows share both timestamps; no edge withdrawn",
						"a", memory.RefusalArg("memory_id", dec.first.SourceID), "b", memory.RefusalArg("memory_id", dec.first.TargetID))
				}
				continue
			}
			dec.askedFirst = dec.first.SourceID == cNewer.ID
			cand := Candidate{
				NewerID: cNewer.ID, NewerContent: cNewer.Content, NewerCreatedAt: cNewer.CreatedAt,
				OlderID: cOlder.ID, OlderContent: cOlder.Content, OlderCreatedAt: cOlder.CreatedAt,
				Similarity: g[0].Strength,
			}
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
					"newer", memory.RefusalArg("memory_id", cand.NewerID), "older", memory.RefusalArg("memory_id", cand.OlderID), "reason", reason)
			}
			settled = append(settled, judged{cand: cand, reason: "vetoed: " + reason, vetoed: true, sweep: true})
			continue
		}
		open = append(open, cand)
		openCycles = append(openCycles, nil)
	}

	var fail error
	if len(open) > 0 {
		votes, asked, classifyErr := classifyVotes(ctx, cls, open, passes)
		if classifyErr != nil {
			fail = errors.Join(fail, classifyErr)
		}
		answered := 0
		for i, c := range open {
			if asked[i] < passes {
				res.Unjudged = append(res.Unjudged, UnjudgedPair{NewerID: c.NewerID, OlderID: c.OlderID})
				if dec := openCycles[i]; dec != nil {
					res.Cyclic = append(res.Cyclic, CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleNoVerdict})
				}
				continue
			}
			answered++
			verdict, state := votes[i].quorum(passes)
			if state == disputed {
				res.NotAgreed++
				res.Disputed = append(res.Disputed, Disputed{
					NewerID: c.NewerID, OlderID: c.OlderID,
					Tally: votes[i].tally(), Unreadable: votes[i].unreadable,
				})
				if dec := openCycles[i]; dec != nil {
					res.Cyclic = append(res.Cyclic, CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleNotAgreed})
				}
				if logger != nil {
					top, n := votes[i].best()
					logger.Info("supersede reassess: the passes did not agree; no edge withdrawn",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID),
						"passes", passes, "top", string(top), "top_votes", n)
				}
				continue
			}
			settleOne(&res, &settled, c, verdict, openCycles[i])
		}
		if classifyErr != nil && logger != nil {
			logger.Warn("supersede reassess: the classifier failed; the edges it would have judged stand",
				"answered", answered, "unjudged", len(res.Unjudged), "settled", len(settled),
				"passes", passes, "error", classifyErr)
		}
	}

	rows := make([]judgedEdge, 0, len(settled))
	for _, j := range settled {
		rows = append(rows, judgedEdge{OlderID: j.cand.OlderID, NewerID: j.cand.NewerID})
	}
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
			TargetProjectID: byID[j.cand.OlderID].ProjectID,
		}
		if j.sweep {
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
				fail = errors.Join(fail, fmt.Errorf("withdraw supersedes link %s→%s: %w", memory.RefusalArg("memory_id", w.NewerID), memory.RefusalArg("memory_id", w.OlderID), err))
				break
			}
			if n == 0 {
				w.Written = false
			} else {
				w.Written = true
				res.Withdrawn++
			}
		}
		withdrawn = append(withdrawn, w)
		if apply && j.sweep {
			n, err := store.InvalidateLink(ctx, w.OlderID, w.NewerID, string(RelationCauses))
			if err != nil {
				w.CausesSwept = 0
				w.SweepFailed = true
				withdrawn[len(withdrawn)-1] = w
				res.CausesSweepFailed++
				fail = errors.Join(fail, fmt.Errorf("withdraw causes link %s→%s: %w", memory.RefusalArg("memory_id", w.OlderID), memory.RefusalArg("memory_id", w.NewerID), err))
				break
			}
			w.CausesSwept = int(n)
			withdrawn[len(withdrawn)-1] = w
			res.CausesWithdrawn += int(n)
		}
	}

	if !apply {
		for _, w := range withdrawn {
			res.CausesWithdrawn += w.CausesSwept
		}
	}
	if logger != nil {
		logger.Info("supersede reassess",
			"loaded", res.Loaded, "skipped", res.Skipped, "vetoed", res.Vetoed,
			"confirmed", res.Confirmed, "neither", res.Neither, "causes", res.Causes,
			"reversed", res.Reversed, "unknown", res.Unclassified, "unjudged", len(res.Unjudged),
			"consensus", res.Consensus, "not_agreed", res.NotAgreed,
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

func classifyVotes(ctx context.Context, cls Classifier, pairs []Candidate, passes int) ([]voteCount, []int, error) {
	votes := make([]voteCount, len(pairs))
	asked := make([]int, len(pairs))
	var fail error
	for pass := 0; pass < passes; pass++ {
		relations, err := cls.ClassifyBatch(ctx, pairs)
		if err == nil && len(relations) != len(pairs) {
			err = fmt.Errorf("classifier returned %d verdict(s) for %d pair(s)", len(relations), len(pairs))
		}
		answered := len(pairs)
		if err != nil {
			answered = answeredPrefix(relations, len(pairs), err)
			fail = errors.Join(fail, fmt.Errorf("classify %d live supersedes edge(s) (pass %d of %d): %w", len(pairs), pass+1, passes, err))
		}
		for i := 0; i < answered; i++ {
			votes[i].add(relations[i])
			asked[i]++
		}
		if err != nil {
			break
		}
	}
	return votes, asked, fail
}

func settleOne(res *ReassessResult, settled *[]judged, c Candidate, verdict Relation, dec *cycleDecision) {
	if dec != nil {
		*settled = append(*settled, settleCycle(dec, verdict, res)...)
		return
	}
	switch verdict {
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
		res.Unclassified++
	}
}

func settleCycle(dec *cycleDecision, verdict Relation, res *ReassessResult) []judged {
	cyc := CyclicPair{First: dec.first, Second: dec.second, Outcome: CycleNoVerdict}
	judgedEdge, other := dec.edgeOf()
	reverse := func(e CyclicEdge) Candidate {
		return Candidate{NewerID: e.SourceID, OlderID: e.TargetID}
	}
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
		res.Unclassified++
	}
	res.Cyclic = append(res.Cyclic, cyc)
	return out
}
