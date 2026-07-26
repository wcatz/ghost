// Package resolve marks "resolved-evidence" memories so they drop out of the
// ranked session-start injection while staying searchable. It is the detection
// half of the resolution classifier; consumption is the AND resolved_at IS NULL
// predicate on the injection/browse queries.
//
// Design mirrors internal/supersede: a cheap local prefilter proposes
// candidates, an LLM Classifier adjudicates each with a crisp one-word
// question (biased to KEEP), and — with apply — the confirmed set is stamped
// via SetResolved. The real Classifier will live behind an LLM implementation
// added in a later task. The command is a standalone `ghost resolve` batch,
// never a hook: the stop hook contract forbids DB access on that path
// (internal/mcpinit/stophook.go). The pass is re-runnable and idempotent —
// already-resolved rows are excluded by ResolveCandidates.
package resolve

import (
	"context"
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
}

// Classifier decides whether a memory's content is resolved evidence (true) or
// a terminal conclusion / still-active knowledge (false). The LLM
// implementation lives in haiku.go; tests inject a deterministic fake. It is
// biased to KEEP (return false when uncertain): a false resolve buries a useful
// memory, a missed resolve merely leaves the status quo.
type Classifier interface {
	IsResolved(ctx context.Context, content string) (bool, error)
}

// resolveStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type resolveStore interface {
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	SetResolved(ctx context.Context, ids []string) (int, error)
}

// Result summarizes a pass.
type Result struct {
	Loaded     int // candidates returned by the store (already category/pin/NULL filtered)
	Candidates int // survived the keyword prefilter and were classified
	Confirmed  int // classified as resolved evidence
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

// Run loads eligible candidates, prefilters them, classifies each, and — when
// apply is true — stamps resolved_at on every confirmed memory in one batch.
// Dry-run (apply=false) writes nothing but returns the confirmed set for
// preview. A classifier error on any memory is fatal so a partial pass is never
// silently applied.
func Run(ctx context.Context, store resolveStore, cls Classifier, projectID string, apply bool, logger *slog.Logger) (Result, []memory.Memory, error) {
	loaded, err := store.ResolveCandidates(ctx, projectID)
	if err != nil {
		return Result{}, nil, fmt.Errorf("load candidates: %w", err)
	}
	cands := Prefilter(loaded)
	res := Result{Loaded: len(loaded), Candidates: len(cands)}
	if logger != nil {
		logger.Info("resolve prefilter",
			"loaded", len(loaded), "kept", len(cands), "skipped", len(loaded)-len(cands))
	}

	var confirmed []memory.Memory
	for _, m := range cands {
		ok, err := cls.IsResolved(ctx, m.Content)
		if err != nil {
			return res, nil, fmt.Errorf("classify %s: %w", m.ID, err)
		}
		if !ok {
			continue
		}
		res.Confirmed++
		confirmed = append(confirmed, m)
	}

	if apply && len(confirmed) > 0 {
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
	return res, confirmed, nil
}
