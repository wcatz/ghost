// The repair pass: re-run resolve's decision over memories resolve already
// resolved.
//
// Issue #640 measured the ordinary pass against a real database and found
// roughly one resolve in three had buried durable knowledge — a safety lesson, a
// standing "NEVER …" rule, a CI gotcha — none of which any later pass would ever
// look at again, because ResolveCandidates excludes rows that already carry
// resolved_at. Nothing in the ordinary path can undo that. Reassess is the
// operator-facing way to undo it: it re-applies the deterministic KEEP vetoes and
// the same classifier to the already-resolved pool and returns the notes that now
// come back KEEP, clearing resolved_at with --apply so they return to ranked
// session-start injection.
//
// It deliberately does NOT re-run the keyword prefilter. The prefilter bounds
// the cost of a pass over every memory in a project; here the pool is already the
// small resolved subset, and a wrongly-resolved note is not hidden behind a
// resolution keyword — it is hidden behind the *absence* of one, or behind a
// narrative that reads like a fix. Skipping the filter is also what makes a
// repair auditable: every already-resolved memory in the project is re-judged
// under the same rules, so the report has no silent gap.
//
// It does honour Run's two free deterministic demotions, as a floor rather than
// as part of the repair. Both fire before the veto and before the KEEP cache, so
// a row they assert is re-stamped by the very next ordinary pass; clearing it
// would print "cleared resolved_at for N" and change nothing. Such rows are
// counted as Demoted and left alone, and the operator sees them in the summary.
//
// The KEEP cache is honoured, for the same convergence reason the ordinary pass
// honours it: content that already carries a current-version KEEP hash was
// judged KEEP by these rules, so it is cleared without paying for the call again.
package resolve

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// reassessStore is the subset of *memory.Store the repair pass needs; narrowed
// for testability. It never writes a resolution, but it does read the
// *unresolved* pool and the supersedes links, because a row Run would re-stamp
// for free is not a repair candidate (see the package comment).
type reassessStore interface {
	ResolvedCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error)
	ClearResolved(ctx context.Context, projectID string, ids []string) (int, error)
	MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error
}

// ReassessResult summarizes a reassess pass. Unlike Result it has no "would
// resolve" side: every input row is already resolved, so the only outcomes are
// "still resolved" and "back in the ranked surface".
type ReassessResult struct {
	Loaded        int // already-resolved eligible memories the pass considered
	Demoted       int // already-resolved rows Run's free demotions still assert; left alone
	Vetoed        int // settled KEEP by the deterministic veto, no classifier call
	Cached        int // skipped: the content already carries a KEEP hash
	ReKept        int // came back KEEP, so resolved_at is now wrong (veto + classifier)
	StillResolved int // came back RESOLVED with a closed-by reason; stays resolved
	Unknown       int // unparseable verdict; left resolved and re-offered next pass
	Cleared       int // rows actually cleared (0 in dry-run)
	Pool          int // the project's already-resolved pool before any scope narrowed it
	Misses        []ScopeMiss
	// Held is every row the pass left resolved for a deterministic reason, in
	// pool order, each naming what holds it. Demoted is len(Held): the count and
	// the list are one finding read two ways, and a summary count that can
	// disagree with the list under it is the defect #712 opened with.
	Held []HeldMemory
	// Rounds is how many hold-back re-checks the pass needed to reach a fixed
	// point, and BoundHit says the bound stopped it with a row still changing —
	// so the repair is short of the answer a further pass would give. Both are
	// properties of the DECISION, so a dry run reports the same pair as --apply
	// and a dry run's bound is the bound --apply would hit.
	Rounds   int
	BoundHit bool
}

// HoldKind is which of Run's two free demotions is asserting a row, because the
// two need different remedies and an operator cannot tell which from a count.
type HoldKind string

const (
	// HoldSupersedes is a live 'supersedes'/'llm' edge whose older endpoint is the
	// held row. `ghost supersede --withdraw` can undo it.
	HoldSupersedes HoldKind = "supersedes"
	// HoldCorrection is an explicit correction that pairs the held row. Nothing
	// withdraws a pairing, so this is a row to READ, and the correction's own id
	// is what makes that checkable.
	HoldCorrection HoldKind = "correction"
)

// Hold is one thing asserting an already-resolved row, and the id an operator
// acts on. Holder is the edge's SOURCE — 'supersedes' is written newer→older, so
// the source is the newer note an operator withdraws or reads — and the
// correction's own id.
type Hold struct {
	Kind   HoldKind
	Holder string
}

// HeldMemory is a row the repair leaves resolved, with everything holding it.
// A row can have more than one holder: a note two newer notes both supersede is
// held by either, and withdrawing one of the two releases nothing, so naming only
// one would send the operator to undo an edge whose undo changes nothing.
type HeldMemory struct {
	Memory memory.Memory
	Holds  []Hold
}

// Reason is the sentence the report prints for a held row. Sorted on a copy —
// supersedes before corrections, then by id — so the list reads the same way
// however the caller assembled it, and so a report a reader compares between two
// runs is comparable. Empty when nothing holds the row, which the pass never
// renders: a row with no holder is not a held row.
// Reason is the sentence a report prints about what holds the row, and it is a
// DISPLAY method rather than a fact about the graph: the holders are stored ids
// (a supersedes source, a correction's paired row), so each one is rendered
// through assemble.Token exactly as the id on the same line is. A holder that is
// a pre-#791 import id, a restored snapshot row or a hand-edited row can hold a
// newline, and because it is printed mid-line the text after that newline begins
// a line of its own, outside every «» block. What the store holds is unchanged —
// this is the one place the reason is built, and it has one caller.
func (h HeldMemory) Reason() string {
	if len(h.Holds) == 0 {
		return ""
	}
	parts := make([]string, 0, len(h.Holds))
	supersedes := make([]string, 0, len(h.Holds))
	corrections := make([]string, 0, len(h.Holds))
	for _, k := range h.Holds {
		switch k.Kind {
		case HoldSupersedes:
			supersedes = append(supersedes, string(k.Kind)+" "+assemble.Token(k.Holder))
		default:
			corrections = append(corrections, string(k.Kind)+" "+assemble.Token(k.Holder))
		}
	}
	sort.Strings(supersedes)
	sort.Strings(corrections)
	parts = append(parts, supersedes...)
	parts = append(parts, corrections...)
	return "held by " + strings.Join(parts, ", ")
}

// Reassess re-runs the vetoes and the classifier over the memories resolve has
// ALREADY stamped resolved_at, and returns the ones that now come back KEEP (in
// load order). With apply it clears resolved_at on exactly those rows and
// records the classifier's KEEP verdicts in the content-hash cache, so the
// ordinary pass does not re-ask a note that was just repaired. Dry-run
// (apply=false) writes nothing.
//
// A classifier error on any batch is fatal and clears nothing: a partial repair
// would return notes to injection that the harness never judged. A failed clear
// is fatal for the same reason — a repair that did not happen must not be
// reported as one. A failed cache write only warns, because the repair itself
// has already landed and losing derived state costs one re-ask next pass.
func Reassess(ctx context.Context, store reassessStore, cls Classifier, projectID string, apply bool, scope Scope, logger *slog.Logger) (ReassessResult, []memory.Memory, error) {
	var res ReassessResult
	pool, err := store.ResolvedCandidates(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolved candidates: %w", err)
	}
	res.Pool = len(pool)
	loaded, misses, err := scope.Select(pool)
	if err != nil {
		return res, nil, err
	}
	res.Misses = misses
	res.Loaded = len(loaded)

	keptHashes, err := store.ResolveKeptHashes(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolve kept hashes: %w", err)
	}

	// Run's two free demotions, as a floor. Both are computed before the veto
	// and before the KEEP cache in Run, so a row either one covers comes back
	// stamped on the next ordinary pass; the repair pass must not claim it.
	// The pairing needs a second look once the re-KEEP set is known, because a
	// correction in that set is leaving the resolved pool (see holdBack).
	unresolved, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load unresolved candidates: %w", err)
	}
	asserted, err := assertedByDemotions(ctx, store, projectID, loaded, unresolved)
	if err != nil {
		return res, nil, err
	}

	// The subject of every unresolved note, for the one case where the veto is
	// not allowed to settle a note on its own (#698). Built once over the whole
	// pool; only a vetoed note is looked up in it.
	newerSubjects := indexSubjects(unresolved)

	// Settle the free decisions first: the veto and the KEEP cache both answer
	// KEEP without a harness call. reKeptIDs collects every KEEP outcome, and
	// the returned list is built from it in the store's own order below, so a
	// report reads the same way every run.
	reKeptIDs := make(map[string]bool, len(loaded))
	var pending []memory.Memory
	var pendingContents []string
	for _, m := range loaded {
		if len(asserted[m.ID]) > 0 {
			continue
		}
		if reason, vetoed := VetoKeep(m.Content); vetoed {
			// A veto is a KEEP here, and on THIS pass a KEEP un-hides the note.
			// It does not get to do that on a phrase alone when a newer memory
			// in the same project is demonstrably about the same thing: the veto
			// protects the wording of a changelog entry, not the claim in it
			// (#698). The note goes to the classifier instead. The ordinary
			// pass's veto is untouched — there the same false veto costs one
			// noisy memory, not a permanent un-hiding.
			if newer, shared, shadowed := newerSubjects.shadowing(m); shadowed {
				if logger != nil {
					logger.Debug("reassess veto deferred to the classifier: a newer note names the same identifiers",
						"id", m.ID, "pattern", reason, "newer", newer, "shared_identifiers", shared)
				}
			} else {
				res.Vetoed++
				reKeptIDs[m.ID] = true
				if logger != nil {
					logger.Debug("reassess veto kept memory", "id", m.ID, "pattern", reason)
				}
				continue
			}
		}
		if keptHashes[m.ID] == ContentHash(m.Content) {
			res.Cached++
			reKeptIDs[m.ID] = true
			continue
		}
		pending = append(pending, m)
		pendingContents = append(pendingContents, m.Content)
	}

	// newKept collects the classifier's KEEP verdicts; written only on apply.
	// A row holdBack removes is deleted from it below: it is not being
	// repaired, and a KEEP hash on a row Run will re-stamp anyway is state
	// that reads as a decision resolve never made.
	newKept := make(map[string]string)
	if len(pendingContents) > 0 {
		verdicts, err := cls.IsResolvedBatch(ctx, pendingContents)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d resolved candidate(s): %w", len(pendingContents), err)
		}
		if len(verdicts) != len(pendingContents) {
			return res, nil, fmt.Errorf("classify %d resolved candidate(s): classifier returned %d verdict(s)", len(pendingContents), len(verdicts))
		}
		for i, m := range pending {
			switch verdicts[i] {
			case VerdictKeep:
				newKept[m.ID] = ContentHash(m.Content)
				reKeptIDs[m.ID] = true
			case VerdictResolved:
				res.StillResolved++
			default:
				// UNKNOWN (or an invalid classifier value) is not an implicit
				// KEEP: the note keeps resolved_at and is offered again.
				res.Unknown++
			}
		}
	}

	var reKept []memory.Memory
	for _, m := range loaded {
		if reKeptIDs[m.ID] {
			reKept = append(reKept, m)
		}
	}
	kept := reKept
	heldByHoldBack := holdBack(kept, unresolved, maxHoldBackRounds)
	reKept, res.Rounds, res.BoundHit = heldByHoldBack.kept, heldByHoldBack.rounds, heldByHoldBack.boundHit
	if len(reKept) != len(kept) {
		inReKept := make(map[string]bool, len(reKept))
		for _, m := range reKept {
			inReKept[m.ID] = true
		}
		for _, m := range kept {
			if !inReKept[m.ID] {
				delete(newKept, m.ID)
			}
		}
	}
	res.ReKept = len(reKept)
	// One list, in pool order, holding both the up-front floor's rows and the
	// hold-back's. A row can be held by both, and then it is listed once with
	// both reasons, so the list cannot over-count against Demoted either.
	byID := make(map[string]memory.Memory, len(loaded))
	for _, m := range loaded {
		byID[m.ID] = m
	}
	for _, m := range loaded {
		holds := asserted[m.ID]
		holds = append(holds, heldByHoldBack.holds[m.ID]...)
		if len(holds) == 0 {
			continue
		}
		res.Held = append(res.Held, HeldMemory{Memory: byID[m.ID], Holds: holds})
	}
	res.Demoted = len(res.Held)
	if logger != nil {
		logger.Info("reassess classified",
			"loaded", res.Loaded, "rekept", len(reKept), "vetoed", res.Vetoed,
			"cached", res.Cached, "asserted", res.Demoted,
			"still_resolved", res.StillResolved, "unknown", res.Unknown,
			"holdback_rounds", res.Rounds, "holdback_bound_hit", res.BoundHit)
	}

	if apply && len(reKept) > 0 {
		ids := make([]string, len(reKept))
		for i, m := range reKept {
			ids[i] = m.ID
		}
		n, err := store.ClearResolved(ctx, projectID, ids)
		if err != nil {
			return res, nil, fmt.Errorf("clear resolved: %w", err)
		}
		res.Cleared = n
		if logger != nil {
			logger.Info("reassess applied", "cleared", res.Cleared)
		}
	}
	if apply && len(newKept) > 0 {
		if err := store.MarkResolveKept(ctx, projectID, newKept); err != nil {
			// The repair is already written; losing derived cache state only
			// costs a re-ask next pass, so warn rather than report failure.
			if logger != nil {
				logger.Warn("resolve kept cache write failed", "error", err)
			}
		}
	}
	return res, reKept, nil
}

// maxHoldBackRounds caps how many hold-back re-checks ONE run performs.
//
// It is not what makes the loop terminate. What terminates it is that every
// round which finds anything removes at least one row from the repair set, so
// the loop is bounded by len(repair) rounds however the pairings move — and
// those do move, in both directions. A held-back row leaves the pool the
// re-check reads, and that pool is BOTH the frequency pool and the correction
// pool, so dropping a row lowers the document frequency of the tokens it
// carries (which can reveal a pairing the round before was blind to) AND, when
// the row is correction-marked, takes away every pairing that row was asserting.
// A later round can report FEWER pairings than the one before it, and
// TestHoldBackPairingsAreNotMonotoneAcrossRounds is the case that does.
//
// The bound is therefore a cap on the WORK ONE RUN DOES, not a termination
// argument: with it and without it the re-check ends, and what changes is
// whether one run reaches its fixed point or leaves work for the next. Reaching
// a bound with a row still changing is a finding the report has to carry rather
// than a truncation to swallow, which is why the loop reports it.
const maxHoldBackRounds = 8

// holdBackResult is the hold-back re-check's answer: the rows still repairable,
// the rows it holds with what holds them, how many re-checks it took, and
// whether the bound stopped it short of a fixed point.
type holdBackResult struct {
	kept     []memory.Memory
	holds    map[string][]Hold
	rounds   int
	boundHit bool
}

// holdBack removes from the repair set any row a correction that is itself being
// repaired would re-demote. Both rows are resolved right now, so the up-front
// floor sees no pairing; but clearing the correction puts it back in
// ResolveCandidates, and mechanism 2 then re-stamps the older row on the very
// next ordinary pass — ahead of the veto and the KEEP cache, neither of which
// can save it. The repair would be undone by the pass that follows it, so the
// row is held back and reported as asserted instead.
//
// The re-check reads the pool the NEXT ORDINARY PASS will read, which is the
// unresolved pool plus the rows this run clears, and it reads it as both the
// frequency pool and the correction pool — the same two jobs Run gives its own
// ResolveCandidates pool. That is the whole of #712's second observation: read
// over the live pool alone, a row this run clears is missing from the count, so
// its subject tokens look rarer than the next pass will find them, and a pairing
// that next pass will not make holds a row back for a second, identical pass to
// release.
//
// One round cannot answer it. A row held back leaves that pool, which lowers the
// document frequency of the tokens it carries, which can turn a token rare that
// was not, which can reveal a pairing that was invisible a round earlier. So the
// re-check repeats until it finds nothing new, and the rounds are reported: an
// operator who sees the repair needed three of them knows the answer is not the
// one a single re-check would have given. Bounded at maxHoldBackRounds, and a
// bound reached with a row still changing says so, because the repair is then
// short of the answer a further pass would give.
//
// The same removal works the other way too — see maxHoldBackRounds — so a later
// round can find FEWER pairings than the one before it. Which is why dropping is
// one-way: a row removed because a correction asserted it is never re-added, so
// a correction that is itself held back cannot free its row again. Without that
// rule the answer would depend on how the pairings happened to move between
// rounds, and the LAST round would win rather than the union. It errs toward
// leaving durable knowledge resolved, which is the status quo and the visible,
// safe direction, rather than toward a repair the next pass undoes.
func holdBack(repair, unresolved []memory.Memory, maxRounds int) holdBackResult {
	out := holdBackResult{kept: repair, holds: make(map[string][]Hold)}
	kept := repair
	for round := 1; round <= maxRounds; round++ {
		// The pool the next ordinary pass reads: the live one, plus every row
		// this run is about to clear into it. Built fresh each round — the set
		// changes as rows are held back, and appending onto the live pool's own
		// backing array would corrupt the next round's count.
		pool := make([]memory.Memory, 0, len(unresolved)+len(kept))
		pool = append(pool, unresolved...)
		pool = append(pool, kept...)

		// The candidate pool is the keyword-prefiltered subset, because that is
		// what Run pairs: a row with no resolution keyword is never a pairing
		// target there, so holding it back here would be a permanent,
		// mislabelled hold that no later pass could undo.
		dropped := make(map[string][]Hold)
		for _, p := range correctionPairingsFrom(pool, Prefilter(kept), pool) {
			dropped[p.Target.ID] = append(dropped[p.Target.ID], Hold{Kind: HoldCorrection, Holder: p.Correction.ID})
		}
		out.rounds = round
		if len(dropped) == 0 {
			return out
		}
		for id, holds := range dropped {
			out.holds[id] = append(out.holds[id], holds...)
		}
		// Rebuild the repair set: the pool the next round counts over can only
		// shrink here, since a dropped row never reaches the next ordinary pass.
		next := make([]memory.Memory, 0, len(kept))
		for _, m := range kept {
			if _, dropped := dropped[m.ID]; !dropped {
				next = append(next, m)
			}
		}
		kept = next
		out.kept = kept
		if round == maxRounds {
			out.boundHit = true
			return out
		}
	}
	return out
}

// assertedByDemotions returns, for every ID among the already-resolved pool that
// Run would stamp again for free on its next pass, what holds it — so the repair
// pass leaves those rows alone AND can name why. It mirrors the two mechanisms in
// Run, in the same order they matter:
//
//   - the supersedes-edge piggyback: the older endpoint of a live
//     'supersedes'/'llm' link, which is the only demotion that needs no other
//     row to be present;
//   - correction pairing: an older row a NEWER correction shares rare subject
//     tokens with. The correction itself usually still lives — a correction is
//     a terminal conclusion and stays KEEP — so the unresolved pool has to be
//     read for the pairing to be visible here at all, and it is the only pool
//     searched for corrections, exactly as in Run. A correction that is itself
//     resolved asserts nothing, because the next ordinary pass will not see it
//     either.
//
// It is a FLOOR, and deliberately the conservative one: corrections come only
// from the live pool, because a resolved correction this run does not clear will
// not be there for the next ordinary pass. The count it therefore takes is over
// the live pool too, which can only make tokens look rarer than the next pass
// will find them, so this floor can hold a row the hold-back re-check releases.
// That is the right way round — an over-held row is left resolved and reported
// with a reason an operator can check, while an under-held one is cleared and
// immediately re-stamped — but it is why the re-check exists and why it runs on
// the post-repair pool.
func assertedByDemotions(ctx context.Context, store reassessStore, projectID string, resolved, unresolved []memory.Memory) (map[string][]Hold, error) {
	asserted := make(map[string][]Hold, len(resolved))
	if len(resolved) == 0 {
		return asserted, nil
	}
	resolvedIDs := make(map[string]bool, len(resolved))
	for _, m := range resolved {
		resolvedIDs[m.ID] = true
	}

	links, err := store.LinksByRelationSource(ctx, projectID, "supersedes", "llm")
	if err != nil {
		return nil, fmt.Errorf("load supersedes links: %w", err)
	}
	// The same exemption Run applies, for the same reason: an edge across a
	// scope conflict asserts no replacement, so holding the row back on its
	// account would report "still demoted" for a retirement that should not
	// exist, and no pass could ever repair it.
	links, err = scopeCompatibleSupersedes(ctx, store, links)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if resolvedIDs[l.TargetID] {
			asserted[l.TargetID] = append(asserted[l.TargetID], Hold{Kind: HoldSupersedes, Holder: l.SourceID})
		}
	}

	// The unresolved pool fills both of the roles it fills in Run: it is where
	// the frequencies are counted and where a demoting correction is looked
	// for. So a correction that is itself already resolved — invisible to the
	// next ordinary pass — asserts nothing here either, and a row it used to
	// demote becomes repairable again instead of being reported under the
	// wrong label. Only the candidate set differs, because the rows to protect
	// are exactly the ones ResolveCandidates cannot return.
	// Prefilter, for the same reason as in holdBack: Run pairs only the
	// keyword-passing subset, so only those rows can be re-stamped by the next
	// ordinary pass and only those may be held back here.
	for _, p := range correctionPairingsFrom(unresolved, Prefilter(resolved), unresolved) {
		asserted[p.Target.ID] = append(asserted[p.Target.ID], Hold{Kind: HoldCorrection, Holder: p.Correction.ID})
	}
	return asserted, nil
}
