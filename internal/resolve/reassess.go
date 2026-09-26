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

	"github.com/wcatz/ghost/internal/memory"
)

// reassessStore is the subset of *memory.Store the repair pass needs; narrowed
// for testability. It never writes a resolution, but it does read the
// *unresolved* pool and the supersedes links, because a row Run would re-stamp
// for free is not a repair candidate (see the package comment).
type reassessStore interface {
	ResolvedCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
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
func Reassess(ctx context.Context, store reassessStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (ReassessResult, []memory.Memory, error) {
	var res ReassessResult
	loaded, err := store.ResolvedCandidates(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolved candidates: %w", err)
	}
	res.Loaded = len(loaded)

	keptHashes, err := store.ResolveKeptHashes(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolve kept hashes: %w", err)
	}

	// Run's two free demotions, as a floor. Both are computed before the veto
	// and before the KEEP cache in Run, so a row either one covers comes back
	// stamped on the next ordinary pass; the repair pass must not claim it.
	asserted, err := assertedByDemotions(ctx, store, projectID, loaded)
	if err != nil {
		return res, nil, err
	}

	// Settle the free decisions first: the veto and the KEEP cache both answer
	// KEEP without a harness call. reKeptIDs collects every KEEP outcome, and
	// the returned list is built from it in the store's own order below, so a
	// report reads the same way every run.
	reKeptIDs := make(map[string]bool, len(loaded))
	var pending []memory.Memory
	var pendingContents []string
	for _, m := range loaded {
		if asserted[m.ID] {
			res.Demoted++
			continue
		}
		if reason, vetoed := VetoKeep(m.Content); vetoed {
			res.Vetoed++
			reKeptIDs[m.ID] = true
			if logger != nil {
				logger.Debug("reassess veto kept memory", "id", m.ID, "pattern", reason)
			}
			continue
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
	res.ReKept = len(reKept)
	if logger != nil {
		logger.Info("reassess classified",
			"loaded", res.Loaded, "rekept", len(reKept), "vetoed", res.Vetoed,
			"cached", res.Cached, "asserted", res.Demoted,
			"still_resolved", res.StillResolved, "unknown", res.Unknown)
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

// assertedByDemotions returns the IDs among the already-resolved pool that Run
// would stamp again for free on its next pass, so the repair pass leaves them
// alone. It mirrors the two mechanisms in Run, in the same order they matter:
//
//   - the supersedes-edge piggyback: the older endpoint of a live
//     'supersedes'/'llm' link, which is the only demotion that needs no other
//     row to be present;
//   - correction pairing: an older row a NEWER correction in either pool shares
//     rare subject tokens with. The correction itself usually still lives — a
//     correction is a terminal conclusion and stays KEEP — so the unresolved
//     pool has to be read for the pairing to be visible here at all.
//
// The rare-token document frequency is counted over both pools together rather
// than over the unresolved pool alone as Run does, because both are already
// loaded. That widens the DF, so it can only *narrow* the pairing set: a row
// this skips is a row that is left resolved. Leaving durable knowledge resolved
// is the status quo and the safe direction to err in; clearing a row the next
// pass re-stamps is the failure this function exists to prevent.
func assertedByDemotions(ctx context.Context, store reassessStore, projectID string, resolved []memory.Memory) (map[string]bool, error) {
	asserted := make(map[string]bool, len(resolved))
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
	for _, l := range links {
		if resolvedIDs[l.TargetID] {
			asserted[l.TargetID] = true
		}
	}

	unresolved, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("load unresolved candidates: %w", err)
	}
	union := make([]memory.Memory, 0, len(resolved)+len(unresolved))
	union = append(union, resolved...)
	union = append(union, unresolved...)
	for _, m := range correctionPairTargets(union, resolved) {
		asserted[m.ID] = true
	}
	return asserted, nil
}
