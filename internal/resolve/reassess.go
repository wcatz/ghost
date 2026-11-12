// The repair pass: re-run resolve's decision over memories resolve already
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

type reassessStore interface {
	ResolvedCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error)
	ClearResolved(ctx context.Context, projectID string, ids []string) (int, error)
	MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error
	FlagsByMemory(ctx context.Context, projectID string) (map[string]memory.FlagEvidence, error)
}

// ReassessResult summarizes a reassess pass. Unlike Result it has no "would
// resolve" side: every input row is already resolved, so the only outcomes are
// "still resolved" and "back in the ranked surface".
type ReassessResult struct {
	Loaded        int // already-resolved eligible memories the pass considered
	Demoted       int // already-resolved rows Run's free demotions still assert; left alone
	Vetoed        int // settled KEEP by the deterministic veto, no classifier call
	Cached        int // skipped: the stored KEEP stamp matches the evidence the audit holds
	ReKept        int // came back KEEP, so resolved_at is now wrong (veto + classifier)
	StillResolved int // came back RESOLVED with a closed-by reason; stays resolved
	Unknown       int // unparseable verdict; left resolved and re-offered by the next repair run
	Cleared       int // rows actually cleared (0 in dry-run)
	Pool          int // the project's already-resolved pool before any scope narrowed it
	Misses        []ScopeMiss
	Held          []HeldMemory
	Rounds        int
	BoundHit      bool
}

// HoldKind is which of Run's two free demotions is asserting a row, because the
// two need different remedies and an operator cannot tell which from a count.
type HoldKind string

const (
	HoldSupersedes HoldKind = "supersedes"
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
// records the classifier's KEEP verdicts under KeepStamp over the same flag
// count the ordinary pass reads, so the ordinary pass does not re-ask a note
// that was just repaired. Dry-run (apply=false) writes nothing.
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

	unresolved, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load unresolved candidates: %w", err)
	}
	asserted, err := assertedByDemotions(ctx, store, projectID, loaded, unresolved)
	if err != nil {
		return res, nil, err
	}

	newerSubjects := indexSubjects(unresolved)

	var evidence map[string]memory.FlagEvidence
	if len(loaded) > 0 {
		read, evErr := store.FlagsByMemory(ctx, projectID)
		if evErr != nil {
			if logger != nil {
				logger.Warn("resolve flag evidence unavailable", "error", evErr)
			}
			read = nil
		}
		evidence = read
	}

	reKeptIDs := make(map[string]bool, len(loaded))
	var pending []memory.Memory
	var pendingContents []string
	for _, m := range loaded {
		if len(asserted[m.ID]) > 0 {
			continue
		}
		if reason, vetoed := VetoKeep(m.Content); vetoed {
			if newer, shared, shadowed := newerSubjects.shadowing(m); shadowed {
				if logger != nil {
					logger.Debug("reassess veto deferred to the classifier: a newer note names the same identifiers",
						"id", memory.RefusalArg("memory_id", m.ID), "pattern", reason, "newer", memory.RefusalArg("memory_id", newer), "shared_identifiers", shared)
				}
			} else {
				res.Vetoed++
				reKeptIDs[m.ID] = true
				if logger != nil {
					logger.Debug("reassess veto kept memory", "id", memory.RefusalArg("memory_id", m.ID), "pattern", reason)
				}
				continue
			}
		}
		if keptHashes[m.ID] == KeepStamp(m.Content, evidence[m.ID]) {
			res.Cached++
			reKeptIDs[m.ID] = true
			continue
		}
		pending = append(pending, m)
		pendingContents = append(pendingContents, m.Content)
	}

	newKept := make(map[string]string)
	if len(pendingContents) > 0 {
		asked := askedWithEvidence(pending, evidence)
		verdicts, err := classifyNotes(ctx, cls, asked)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d resolved candidate(s): %w", len(pendingContents), err)
		}
		if len(verdicts) != len(pendingContents) {
			return res, nil, fmt.Errorf("classify %d resolved candidate(s): classifier returned %d verdict(s)", len(pendingContents), len(verdicts))
		}
		for i, m := range pending {
			switch verdicts[i] {
			case VerdictKeep:
				newKept[m.ID] = KeepStamp(m.Content, evidence[m.ID])
				reKeptIDs[m.ID] = true
			case VerdictResolved:
				res.StillResolved++
			default:
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
			if logger != nil {
				logger.Warn("resolve kept cache write failed", "error", err)
			}
		}
	}
	return res, reKept, nil
}

const maxHoldBackRounds = 8

type holdBackResult struct {
	kept     []memory.Memory
	holds    map[string][]Hold
	rounds   int
	boundHit bool
}

func holdBack(repair, unresolved []memory.Memory, maxRounds int) holdBackResult {
	out := holdBackResult{kept: repair, holds: make(map[string][]Hold)}
	kept := repair
	for round := 1; round <= maxRounds; round++ {
		pool := make([]memory.Memory, 0, len(unresolved)+len(kept))
		pool = append(pool, unresolved...)
		pool = append(pool, kept...)

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
	links, err = scopeCompatibleSupersedes(ctx, store, links)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if resolvedIDs[l.TargetID] {
			asserted[l.TargetID] = append(asserted[l.TargetID], Hold{Kind: HoldSupersedes, Holder: l.SourceID})
		}
	}

	for _, p := range correctionPairingsFrom(unresolved, Prefilter(resolved), unresolved) {
		asserted[p.Target.ID] = append(asserted[p.Target.ID], Hold{Kind: HoldCorrection, Holder: p.Correction.ID})
	}
	return asserted, nil
}
