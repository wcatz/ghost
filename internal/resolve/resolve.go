// Package resolve marks "resolved-evidence" memories so they drop out of the
package resolve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

var resolveKeywords = []string{
	"no-go", "resolved", "shipped", "retracted", "superseded", "abandoned",
	"fixed in", "removed", "merged", "kill experiment", "root cause",
	"concluded", "closed", "reverted", "deprecated", "landed in",
	"cost estimate", "postmortem", "changelog:", "investigation note",
	"pr locator", "reference only", "history only", "no follow-up",
	"completed",
}

// Verdict is the classifier's answer for one memory. UNKNOWN means the reply
type Verdict string

const (
	VerdictResolved Verdict = "resolved"
	VerdictKeep     Verdict = "keep"
	VerdictUnknown  Verdict = "unknown"
)

// Classifier decides whether each memory is resolved evidence, an explicit
type Classifier interface {
	IsResolvedBatch(ctx context.Context, contents []string) ([]Verdict, error)
}

const keepCacheHashVersion = memory.ContentHashVersion

// ContentHash is the KEEP-cache key: resolve's question is content-only, so a
func ContentHash(content string) string {
	return memory.ContentHash(content)
}

// KeepStamp is the KEEP-cache key for ONE judged KEEP, and it carries both
func KeepStamp(content string, ev memory.FlagEvidence) string {
	base := ContentHash(content)
	if ev.Flagged > 0 {
		return fmt.Sprintf("%s:%d", base, ev.Flagged)
	}
	return base
}

func askedWithEvidence(mems []memory.Memory, evidence map[string]memory.FlagEvidence) []Note {
	out := make([]Note, len(mems))
	for i, m := range mems {
		out[i] = Note{Content: m.Content, Evidence: evidence[m.ID].Line()}
	}
	return out
}

type resolveStore interface {
	ResolveCandidates(ctx context.Context, projectID string) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	SetResolvedJustified(ctx context.Context, judged []memory.Memory, why map[string]memory.ResolveJustification) (stamped, stale []string, err error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error)
	MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error
	FlagsByMemory(ctx context.Context, projectID string) (map[string]memory.FlagEvidence, error)
}

type linkScopeReader interface {
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
}

func scopeCompatibleSupersedes(ctx context.Context, store linkScopeReader, links []memory.Link) ([]memory.Link, error) {
	if len(links) == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("load supersedes link endpoints: %w", err)
	}
	scopeByID := make(map[string]map[string]string, len(mems))
	for _, m := range mems {
		scopeByID[m.ID] = m.Scope
	}

	out := make([]memory.Link, 0, len(links))
	for _, l := range links {
		newer, ok := scopeByID[l.SourceID]
		if !ok {
			continue
		}
		older, ok := scopeByID[l.TargetID]
		if !ok {
			continue
		}
		if memory.ScopesConflict(newer, older) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// Result summarizes a pass.
type Result struct {
	Loaded     int // candidates returned by the store (already category/pin/NULL filtered)
	Candidates int // survived the keyword prefilter (demoted + then classified)
	Confirmed  int // classified as resolved evidence by the LLM
	Superseded int // older endpoint of a live 'supersedes'/'llm' link, demoted deterministically
	Corrected  int // older prefilter-passing memory tied to a correction, demoted deterministically
	Vetoed     int // candidates settled KEEP by the deterministic veto, no classifier call
	Skipped    int // candidates skipped via the KEEP cache
	Unknown    int // candidates whose verdict could not be parsed; eligible for a later pass
	Resolved   int // rows written (0 in dry-run)
	Stale      int
}

// Prefilter keeps only memories whose content contains a resolution keyword.
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

	links, err := store.LinksByRelationSource(ctx, projectID, "supersedes", "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load supersedes links: %w", err)
	}
	links, err = scopeCompatibleSupersedes(ctx, store, links)
	if err != nil {
		return res, nil, err
	}
	why := make(map[string]memory.ResolveJustification)
	for _, l := range links {
		older, ok := byID[l.TargetID]
		if !ok {
			continue
		}
		res.Superseded++
		addConfirmed(older)
		j := why[older.ID]
		j.EdgeSources = append(j.EdgeSources, l.SourceID)
		why[older.ID] = j
	}

	corrected := make(map[string]bool)
	for _, p := range correctionPairingsFrom(loaded, cands, loaded) {
		j := why[p.Target.ID]
		j.Corrections = append(j.Corrections, p.Correction)
		why[p.Target.ID] = j
		if corrected[p.Target.ID] {
			continue
		}
		corrected[p.Target.ID] = true
		res.Corrected++
		addConfirmed(p.Target)
	}

	keptHashes, err := store.ResolveKeptHashes(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load resolve kept hashes: %w", err)
	}

	var gated []memory.Memory
	for _, m := range cands {
		if confirmedSet[m.ID] {
			continue
		}
		if reason, vetoed := VetoKeep(m.Content); vetoed {
			res.Vetoed++
			if logger != nil {
				logger.Debug("resolve veto kept memory", "id", memory.RefusalArg("memory_id", m.ID), "pattern", reason)
			}
			continue
		}
		gated = append(gated, m)
	}

	var evidence map[string]memory.FlagEvidence
	if len(gated) > 0 {
		read, evErr := store.FlagsByMemory(ctx, projectID)
		if evErr != nil {
			if logger != nil {
				logger.Warn("resolve flag evidence unavailable", "error", evErr)
			}
			read = nil
		}
		evidence = read
	}

	var pending []memory.Memory
	var pendingContents []string
	for _, m := range gated {
		if keptHashes[m.ID] == KeepStamp(m.Content, evidence[m.ID]) {
			res.Skipped++
			continue
		}
		pending = append(pending, m)
		pendingContents = append(pendingContents, m.Content)
	}

	newKept := make(map[string]string)
	var llmConfirmed int
	if len(pending) > 0 {
		asked := askedWithEvidence(pending, evidence)
		verdicts, err := classifyNotes(ctx, cls, asked)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d candidate(s): %w", len(pendingContents), err)
		}
		if len(verdicts) != len(pendingContents) {
			return res, nil, fmt.Errorf("classify %d candidate(s): classifier returned %d verdict(s)", len(pendingContents), len(verdicts))
		}
		for i, m := range pending {
			switch verdicts[i] {
			case VerdictKeep:
				newKept[m.ID] = KeepStamp(m.Content, evidence[m.ID])
			case VerdictResolved:
				res.Confirmed++
				llmConfirmed++
				addConfirmed(m)
			default:
				res.Unknown++
			}
		}
	}
	if logger != nil {
		logger.Info("resolve classified",
			"confirmed", llmConfirmed, "superseded", res.Superseded,
			"corrected", res.Corrected, "vetoed", res.Vetoed,
			"cached", res.Skipped, "unknown", res.Unknown)
	}

	if apply {
		if len(confirmed) > 0 {
			stamped, stale, err := store.SetResolvedJustified(ctx, confirmed, why)
			if err != nil {
				return res, nil, fmt.Errorf("set resolved: %w", err)
			}
			res.Resolved = len(stamped)
			res.Stale = len(stale)
			if logger != nil {
				logger.Info("resolve applied", "resolved", res.Resolved, "stale", res.Stale)
			}
			stampedSet := make(map[string]bool, len(stamped))
			for _, id := range stamped {
				stampedSet[id] = true
			}
			applied := make([]memory.Memory, 0, len(stamped))
			for _, m := range confirmed {
				if stampedSet[m.ID] {
					applied = append(applied, m)
				}
			}
			confirmed = applied
		}
		if len(newKept) > 0 {
			if err := store.MarkResolveKept(ctx, projectID, newKept); err != nil {
				if logger != nil {
					logger.Warn("resolve kept cache write failed", "error", err)
				}
			}
		}
	}
	return res, confirmed, nil
}
