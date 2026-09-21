// Package resolve marks "resolved-evidence" memories so they drop out of the
// ranked session-start injection while staying searchable. It is the detection
// half of the resolution classifier; consumption is the AND resolved_at IS NULL
// predicate on the injection/browse queries.
//
// Design mirrors internal/supersede: a cheap local prefilter proposes
// candidates, an LLM Classifier adjudicates them in batches of up to eight with
// a numbered KEEP/RESOLVED question (biased to KEEP), and — with apply — the
// confirmed set is stamped via SetResolved while newly-judged KEEP verdicts are
// cached by content hash in memories.resolve_kept_hash so a converged project
// makes no classifier calls at all. The LLM Classifier implementation lives in
// resolution.go; the hosting binary supplies a CLI-harness provider (see
// internal/ai). The stop hook spawns `ghost resolve --apply` as a detached
// background process (internal/mcpinit/stophook.go). The pass is re-runnable
// and idempotent — already-resolved rows are excluded by ResolveCandidates.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// resolveKeywords bounds LLM calls to plausible candidates: memories whose text
// signals a concluded/closed thread. Case-insensitive substring match. Missing
// a keyword only costs recall (the memory stays injectable), so the set is
// deliberately conservative — false negatives are cheap, false positives reach
// the KEEP-biased LLM which is the real gate.
//
// All entries must be lowercase: they are matched against
// strings.ToLower(content) in Prefilter.
var resolveKeywords = []string{
	"no-go", "resolved", "shipped", "retracted", "superseded", "abandoned",
	"fixed in", "removed", "merged", "kill experiment", "root cause",
	"concluded", "closed", "reverted", "deprecated", "landed in",
	// Concluded-work genres the eval corpus showed slipping the net
	// (finding F2, issue #336). Multi-word phrases stay high-precision;
	// "completed" is broader but false positives only cost one KEEP-biased
	// LLM call.
	"cost estimate", "postmortem", "changelog:", "investigation note",
	"pr locator", "reference only", "history only", "no follow-up",
	"completed",
}

// Classifier decides whether each memory's content is resolved evidence (true)
// or a terminal conclusion / still-active knowledge (false). The LLM
// implementation lives in resolution.go; tests inject a deterministic fake. It
// is biased to KEEP (return false when uncertain): a false resolve buries a
// useful memory, a missed resolve merely leaves the status quo. Batched so one
// call adjudicates many notes; the KEEP-cache skip happens in Run.
type Classifier interface {
	IsResolvedBatch(ctx context.Context, contents []string) ([]bool, error)
}

// ContentHash is the KEEP-cache key: resolve's question is content-only, so a
// tag or importance edit must not invalidate a cached verdict. The "v1\x00"
// prefix versions the key — a future prompt/rubric change that could flip
// verdicts bumps it to reset every cached KEEP in one step.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte("v1\x00" + content))
	return hex.EncodeToString(sum[:])
}

// resolveStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type resolveStore interface {
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	SetResolved(ctx context.Context, ids []string) (int, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error)
	MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error
}

// Result summarizes a pass.
type Result struct {
	Loaded     int // candidates returned by the store (already category/pin/NULL filtered)
	Candidates int // survived the keyword prefilter (demoted + then classified)
	Confirmed  int // classified as resolved evidence by the LLM
	Superseded int // older endpoint of a live 'supersedes'/'llm' link, demoted deterministically
	Corrected  int // older prefilter-passing memory tied to a correction, demoted deterministically
	Skipped    int // candidates skipped via the KEEP cache
	Resolved   int // rows written (0 in dry-run)
}

// Prefilter keeps only memories whose content contains a resolution keyword.
// Case-insensitive. Order is preserved.
func Prefilter(mems []memory.Memory) []memory.Memory {
	var out []memory.Memory
	for _, m := range mems {
		lc := strings.ToLower(m.Content)
		for _, kw := range resolveKeywords {
			if strings.Contains(lc, kw) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Run loads eligible candidates, applies two deterministic demotion signals,
// prefilters the rest, skips candidates whose content already earned a KEEP
// verdict (the content-hash cache), classifies the remainder in batches, and —
// when apply is true — stamps resolved_at on every confirmed memory in one
// batch and records the newly-judged KEEP hashes. Dry-run (apply=false) writes
// nothing but returns the confirmed set for preview. A classifier error on any
// batch is fatal so a partial pass is never silently applied.
//
// Deterministic demotions (no LLM call, so no extra CLI spend):
//
//   - Supersedes-edge piggyback: the older endpoint of a live 'supersedes'/'llm'
//     link is already-adjudicated evidence — supersede's LLM decided the newer
//     replaces the older, so resolve acts on that verdict instead of re-asking.
//   - Correction pairing: an explicit correction memory ("CORRECTION/RESOLUTION",
//     "already fixed on main", "no PR is needed") demotes the OLDER
//     prefilter-passing memories sharing its rare subject tokens. The correction
//     itself stays live; only the claims it invalidates are demoted.
func Run(ctx context.Context, store resolveStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (Result, []memory.Memory, error) {
	loaded, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return Result{}, nil, fmt.Errorf("load candidates: %w", err)
	}
	byID := make(map[string]memory.Memory, len(loaded))
	for _, m := range loaded {
		byID[m.ID] = m
	}
	cands := Prefilter(loaded)
	res := Result{Loaded: len(loaded), Candidates: len(cands)}
	if logger != nil {
		logger.Info("resolve prefilter",
			"loaded", len(loaded), "kept", len(cands), "skipped", len(loaded)-len(cands))
	}

	confirmed := make([]memory.Memory, 0, len(cands))
	confirmedSet := make(map[string]bool, len(cands))
	addConfirmed := func(m memory.Memory) {
		if confirmedSet[m.ID] {
			return
		}
		confirmedSet[m.ID] = true
		confirmed = append(confirmed, m)
	}

	// Mechanism 1: supersedes-edge piggyback. The link source is the NEWER
	// memory ('supersedes' is written newer→older), so its TargetID is the older
	// now-obsolete claim. Only demote links whose older endpoint is still an
	// eligible candidate (unresolved, unpinned, non-exempt category) — anything
	// else is already handled or out of scope.
	links, err := store.LinksByRelationSource(ctx, projectID, "supersedes", "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes links: %w", err)
	}
	for _, l := range links {
		older, ok := byID[l.TargetID]
		if !ok {
			continue
		}
		res.Superseded++
		addConfirmed(older)
	}

	// Mechanism 2: correction pairing. correctionPairTargets already returns
	// only prefilter-passing loaded candidates, so each is demoted deterministically.
	for _, m := range correctionPairTargets(loaded, cands) {
		res.Corrected++
		addConfirmed(m)
	}

	// Classify the remaining prefilter candidates; deterministically-demoted
	// memories are excluded so a concurrent verdict can't land twice. The KEEP
	// cache drops candidates whose content already earned a KEEP verdict, so a
	// converged project makes no calls at all. The key is the content hash:
	// resolve's question is content-only, so tag/importance edits do not
	// invalidate a cached verdict.
	keptHashes, err := store.ResolveKeptHashes(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolve kept hashes: %w", err)
	}
	var pending []memory.Memory
	var pendingContents []string
	for _, m := range cands {
		if confirmedSet[m.ID] {
			continue
		}
		if keptHashes[m.ID] == ContentHash(m.Content) {
			res.Skipped++
			continue
		}
		pending = append(pending, m)
		pendingContents = append(pendingContents, m.Content)
	}

	// newKept collects KEEP verdicts to cache; written only on apply.
	newKept := make(map[string]string)
	var llmConfirmed int
	if len(pendingContents) > 0 {
		verdicts, err := cls.IsResolvedBatch(ctx, pendingContents)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d candidate(s): %w", len(pendingContents), err)
		}
		if len(verdicts) != len(pendingContents) {
			return res, nil, fmt.Errorf("classify %d candidate(s): classifier returned %d verdict(s)", len(pendingContents), len(verdicts))
		}
		for i, m := range pending {
			if !verdicts[i] {
				newKept[m.ID] = ContentHash(m.Content)
				continue
			}
			res.Confirmed++
			llmConfirmed++
			addConfirmed(m)
		}
	}
	if logger != nil {
		logger.Info("resolve classified",
			"confirmed", llmConfirmed, "superseded", res.Superseded,
			"corrected", res.Corrected, "cached", res.Skipped)
	}

	if apply {
		if len(confirmed) > 0 {
			ids := make([]string, len(confirmed))
			for i, m := range confirmed {
				ids[i] = m.ID
			}
			n, err := store.SetResolved(ctx, ids)
			if err != nil {
				return res, nil, fmt.Errorf("set resolved: %w", err)
			}
			res.Resolved = n
			if logger != nil {
				logger.Info("resolve applied", "resolved", res.Resolved)
			}
		}
		if len(newKept) > 0 {
			if err := store.MarkResolveKept(ctx, projectID, newKept); err != nil {
				// The resolved rows are already correct; losing derived cache
				// state only costs a re-classify next pass, so warn rather
				// than fail a pass whose primary effect landed.
				if logger != nil {
					logger.Warn("resolve kept cache write failed", "error", err)
				}
			}
		}
	}
	return res, confirmed, nil
}
