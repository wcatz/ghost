package assemble

// This file is the answer to "is a row in the block a row the block should not
// have carried", and it is HERE rather than in the benchmark that asks the
// question. That placement is the whole point: a classifier composed in the
// benchmark is a second implementation of rules this package already applies, and
// it is free to drift the moment either is edited — a stage that stops honouring a
// window and a metric that still flags it would then agree, and a leak would be
// reported as clean by the one consumer whose job is to say otherwise. So the
// classifier reads the verdicts the pipeline recorded, not the columns it read
// them from: nothing here parses a validity stamp, re-derives a scope match, or
// names a stage a string literal in another package would have to guess.
//
// The design's Decision 5 composes four exported leaf predicates here instead
// (ExpiredAt, NotYetValidAt, ScopeContradicts, BucketUnexpected) and adds
// ResolvedAt. Reading the recorded verdicts is the same disjunction with one
// fewer way to be wrong: the signals of the stage that dropped a row are the facts
// its exclusion was decided on, and a metric built from them cannot disagree with
// the filter it measures — which is the property the leaf composition reaches for
// and does not quite have, since a leaf called with a second clock is a second
// answer to "is this row in date".

// The contamination arms, as the words a report uses. They are exported because
// the benchmark counts them and prints them, and because a caller that had to
// spell an arm as a string literal would be guessing at this package's
// vocabulary in exactly the way this file exists to prevent.
const (
	// LeakResolved is a row whose resolved_at is set: the store records it as
	// retired evidence. The ranking DEMOTES such a row rather than excluding it,
	// so one reaching the block is a designed possibility rather than a bug — and
	// counting it is how a demote that goes too far, or a window that hands a
	// demoted row a slot it should not have, becomes visible.
	LeakResolved = "resolved"
	// LeakExpired is a row whose validity window has closed.
	LeakExpired = "expired"
	// LeakNotYetValid is a row whose window has not opened yet.
	LeakNotYetValid = "not_yet_valid"
	// LeakOutOfScope is a row that names a different place than the request asked
	// for. Silence is not disagreement, so a row that mentions no scope key
	// contradicts nothing and this arm does not fire for it.
	LeakOutOfScope = "out_of_scope"
	// LeakOtherProject is a row in a bucket the request did not name. It is a
	// measurement arm and not a drop condition: project membership is enforced in
	// the retrieval SQL, and a cross-project request makes no bucket unexpected by
	// definition, which the trace records as ProjectMatch.
	LeakOtherProject = "other_project"
)

// LeakArms lists every arm in report order. A caller that counts arms needs a
// fixed order, and it needs one that includes the arms that fired zero times —
// an absent arm and an arm that was not measured are different facts, and only one
// of them is visible in a map.
var LeakArms = []string{LeakResolved, LeakExpired, LeakNotYetValid, LeakOutOfScope, LeakOtherProject}

// Leak is one admitted item this package classifies as contamination, with every
// arm that fired. One entry per ITEM, not per arm: a row that is both resolved and
// out of date is one contaminated row carrying two reasons, and a caller counting
// rows (the rate's numerator) must not count it twice.
type Leak struct {
	ID   string
	Arms []string
}

// Leaks returns the admitted items this package itself classifies as
// contamination, in the block's own order.
//
// It reads Signals — the per-candidate verdicts stages 2 and 3 recorded — and
// Item.ResolvedAt, which is non-nil exactly when the column is set. Those are the
// assembler's own conclusions about each row, not a second reading of the row.
//
// A result with no trace answers with no leaks. That is deliberate: recording is
// unconditional, so a Result without a trace is a Result whose recording is broken,
// and the honest answer to "which rows leaked" is "this measurement has nothing to
// read" rather than a clean bill of health. A caller that needs the difference to
// be visible checks the trace itself; the benchmark does, and refuses the run
// rather than reporting a zero it cannot support.
//
// An item the trace never reached raises no arm, for the same reason: no recorded
// verdict is not a verdict of "matched everything", and reading a missing entry
// that way would score an unexamined row clean. The pipeline reaches stage 3 for
// every admitted row, so a missing entry is a defect rather than a case, and the
// benchmark asserts there are none.
func (r Result) Leaks() []Leak {
	if r.Trace == nil {
		return nil
	}
	var out []Leak
	for _, it := range r.Items {
		sig, ok := r.Trace.Signals[it.ID]
		if !ok {
			continue
		}
		var arms []string
		switch {
		case it.ResolvedAt != nil:
			arms = append(arms, LeakResolved)
		}
		switch sig.ValidityState {
		case validityExpired:
			arms = append(arms, LeakExpired)
		case validityFuture:
			arms = append(arms, LeakNotYetValid)
		}
		if !sig.ScopeMatched {
			arms = append(arms, LeakOutOfScope)
		}
		if !sig.ProjectMatch {
			arms = append(arms, LeakOtherProject)
		}
		if len(arms) > 0 {
			out = append(out, Leak{ID: it.ID, Arms: arms})
		}
	}
	return out
}

// TrimmedByBudget returns the rows the budget stage removed and the rows the
// response-fit post-pass removed, as two separate lists.
//
// They are separate because they answer different questions and have different
// remedies: the stage-8 trim is the caller's own item bound, raised by raising it,
// while the response-fit trim is the byte cap on the rendered envelope, which a
// larger item count makes WORSE. A caller reading one list and calling it "the
// budget" would report the cost of a tight limit as if it were the cost of a long
// answer, or the reverse.
//
// Both are read by this package's own stage names rather than by a caller naming
// "budget" and "response_fit" as literals. That is a small thing to protect and an
// expensive one to get wrong: a caller with the strings wrong reads an empty list
// and reports that no budget cut anything, which is a confident zero rather than an
// error.
func (t *Trace) TrimmedByBudget() (stage8, responseFit []string) {
	if t == nil {
		return nil, nil
	}
	for _, st := range t.Stages {
		switch st.Stage {
		case stageBudget:
			stage8 = append(stage8, st.DroppedIDs...)
		case stageResponseFit:
			responseFit = append(responseFit, st.DroppedIDs...)
		}
	}
	return stage8, responseFit
}
