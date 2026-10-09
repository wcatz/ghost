package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// GlobalConsolidationResult holds the result of a global consolidation pass.
type GlobalConsolidationResult struct {
	// Clusters is the list of duplicate clusters found. Each cluster contains
	// the IDs of memories that were folded together, with the first ID being
	// the survivor (the one kept).
	Clusters []GlobalCluster

	// Folded is the number of memories folded (removed) during consolidation.
	// Only populated when Apply is true.
	Folded int

	// DryRun indicates this was a dry run — no changes were written.
	DryRun bool
}

// GlobalCluster represents a cluster of near-duplicate global memories.
type GlobalCluster struct {
	// Survivor is the memory that was kept (or would be kept).
	Survivor Memory

	// Folded are the memories that were folded into the survivor.
	Folded []Memory

	// Similarity is the Jaccard similarity between the survivor and the
	// folded memories (minimum across the cluster).
	Similarity float64
}

// ConsolidateGlobal runs a deterministic consolidation pass over _global
// memories, folding near-duplicates using the same similarity rule as the
// project-level SQLite consolidation tier (Jaccard >= 0.5 or containment == 1.0,
// with numeric conflict guard).
//
// It never touches pinned rows' content — a pinned row always survives its
// cluster. The survivor is chosen by: pinned (kept), then newest created_at,
// then highest importance, then longest content.
//
// History and provenance are recorded like any other fold: phaseMerge on the
// survivor with the folded content as merged_content, and an 'observed'
// evidence row for each folded report.
//
// DryRun lists clusters without writing anything. Apply writes the folds.
func (s *Store) ConsolidateGlobal(ctx context.Context, opts GlobalConsolidationOptions) (GlobalConsolidationResult, error) {
	// Load all global memories (excluding pinned from being folded, but they
	// participate in clustering as potential survivors).
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "consolidate-global")
	if err != nil {
		return GlobalConsolidationResult{}, fmt.Errorf("begin global consolidate tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txCtx := withStoreTx(ctx, tx)

	// Get all global memories
	rows, err := tx.QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE project_id = '_global'
		ORDER BY created_at, id
	`)
	if err != nil {
		return GlobalConsolidationResult{}, fmt.Errorf("query global memories: %w", err)
	}
	memories, err := scanMemories(rows)
	if err != nil {
		return GlobalConsolidationResult{}, fmt.Errorf("scan global memories: %w", err)
	}

	if len(memories) == 0 {
		if err := tx.Commit(); err != nil {
			return GlobalConsolidationResult{}, fmt.Errorf("commit empty: %w", err)
		}
		lock.reportHold("consolidate-global", time.Now())
		return GlobalConsolidationResult{DryRun: opts.DryRun}, nil
	}

	// Tokenize all memories for similarity comparison (mirrors reflection's tokenize)
	type tokenized struct {
		mem   Memory
		tokens map[string]bool
	}
	items := make([]tokenized, len(memories))
	for i, m := range memories {
		items[i] = tokenized{mem: m, tokens: globalTokenize(m.Content)}
	}

	// Find clusters using the same similarity rule as SQLiteConsolidator
	// Jaccard >= 0.5 or containment == 1.0, with numeric conflict guard
	absorbed := make([]bool, len(items))
	var clusters []GlobalCluster

	for i := range items {
		if absorbed[i] {
			continue
		}

		best := items[i].mem
		bestTokens := items[i].tokens
		var folded []Memory
		minSim := 1.0

		for j := i + 1; j < len(items); j++ {
			if absorbed[j] {
				continue
			}

			sim := jaccard(bestTokens, items[j].tokens)
			// Containment only fires on full subsumption
			if c := containment(bestTokens, items[j].tokens); c == 1.0 {
				sim = 1.0
			}
			if sim >= 0.5 && !numericConflict(bestTokens, items[j].tokens) {
				absorbed[j] = true
				folded = append(folded, items[j].mem)
				if sim < minSim {
					minSim = sim
				}
			}
		}

		if len(folded) > 0 {
			// Determine survivor: pinned > newest created_at > highest importance > longest content
			survivor := best
			for _, f := range folded {
				if f.Pinned && !survivor.Pinned {
					survivor = f
				} else if f.Pinned == survivor.Pinned {
					// Compare created_at (newer wins)
					if f.CreatedAt > survivor.CreatedAt {
						survivor = f
					} else if f.CreatedAt == survivor.CreatedAt {
						if f.Importance > survivor.Importance {
							survivor = f
						} else if f.Importance == survivor.Importance && len(f.Content) > len(survivor.Content) {
							survivor = f
						}
					}
				}
			}
			clusters = append(clusters, GlobalCluster{
				Survivor:   survivor,
				Folded:     folded,
				Similarity: minSim,
			})
		}
	}

	if opts.DryRun {
		if err := tx.Commit(); err != nil {
			return GlobalConsolidationResult{}, fmt.Errorf("commit dry-run: %w", err)
		}
		lock.reportHold("consolidate-global", time.Now())
		return GlobalConsolidationResult{
			Clusters: clusters,
			DryRun:   true,
		}, nil
	}

	// Apply the folds
	var foldedCount int
	for _, cluster := range clusters {
		// Fold each folded memory into the survivor
		for _, folded := range cluster.Folded {
			// Skip if survivor is pinned (never fold a pinned row)
			if folded.Pinned {
				continue
			}

			// Update survivor: max importance, union tags, longest content
			if folded.Importance > cluster.Survivor.Importance {
				cluster.Survivor.Importance = folded.Importance
			}
			if len(folded.Content) > len(cluster.Survivor.Content) {
				cluster.Survivor.Content = folded.Content
			}
			// Union tags
			tagSet := make(map[string]bool)
			for _, t := range cluster.Survivor.Tags {
				tagSet[t] = true
			}
			for _, t := range folded.Tags {
				tagSet[t] = true
			}
			cluster.Survivor.Tags = make([]string, 0, len(tagSet))
			for t := range tagSet {
				cluster.Survivor.Tags = append(cluster.Survivor.Tags, t)
			}
			sort.Strings(cluster.Survivor.Tags)

			// Record phaseMerge history on survivor with folded content as merged_content
			err := appendHistoryEventsTx(txCtx, tx, []historyEvent{{
				phase:         phaseMerge,
				prov:          Provenance{Agent: folded.Agent, SessionID: folded.SessionID, SourceRef: folded.SourceRef, Confidence: folded.Confidence},
				mergedContent: folded.Content,
			}}, []string{cluster.Survivor.ID})
			if err != nil {
				return GlobalConsolidationResult{}, fmt.Errorf("append history for global fold: %w", err)
			}

			// Record evidence: the folded report is an 'observed' evidence on the survivor
			err = appendEvidenceTx(txCtx, tx, cluster.Survivor.ID, evidenceObserved,
				Provenance{Agent: folded.Agent, SessionID: folded.SessionID, SourceRef: folded.SourceRef, Confidence: folded.Confidence}, false)
			if err != nil {
				return GlobalConsolidationResult{}, fmt.Errorf("append evidence for global fold: %w", err)
			}

			// Delete the folded memory
			_, err = tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ? AND project_id = '_global'`, folded.ID)
			if err != nil {
				return GlobalConsolidationResult{}, fmt.Errorf("delete folded global memory: %w", err)
			}

			foldedCount++
		}

		// Update the survivor row with merged importance, content, tags
		tagsJSON, _ := json.Marshal(cluster.Survivor.Tags)
		_, err = tx.ExecContext(ctx, `
			UPDATE memories
			SET importance = ?, content = ?, tags = ?
			WHERE id = ? AND project_id = '_global'
		`, cluster.Survivor.Importance, cluster.Survivor.Content, string(tagsJSON), cluster.Survivor.ID)
		if err != nil {
			return GlobalConsolidationResult{}, fmt.Errorf("update survivor: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return GlobalConsolidationResult{}, fmt.Errorf("commit global consolidate: %w", err)
	}
	lock.reportHold("consolidate-global", time.Now())

	return GlobalConsolidationResult{
		Clusters: clusters,
		Folded:   foldedCount,
		DryRun:   false,
	}, nil
}

// GlobalConsolidationOptions controls the global consolidation pass.
type GlobalConsolidationOptions struct {
	// DryRun lists clusters without writing anything.
	DryRun bool
}

// globalTokenize splits text into a set of lowercase word tokens, excluding
// stopwords. Mirrors reflection's tokenize for identical similarity scoring.
func globalTokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if isNumericToken(word) {
			tokens[word] = true
			continue
		}
		if len(word) > 1 && !stopwords[word] {
			tokens[word] = true
		}
	}
	return tokens
}

// stopwords mirrors reflection's stopwords for identical tokenization.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

// isNumericToken reports whether s is purely numeric.
func isNumericToken(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

// containment is the overlap coefficient |A∩B| / min(|A|,|B|).
func containment(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	denom := len(a)
	if len(b) < denom {
		denom = len(b)
	}
	return float64(intersection) / float64(denom)
}

// numericConflict reports whether the two token sets differ on a purely-numeric
// token — a precise fact that must not be merged even when surrounding words
// overlap heavily. Mirrors reflection's numericConflict.
func numericConflict(a, b map[string]bool) bool {
	for token := range a {
		if isNumericToken(token) && !b[token] {
			return true
		}
	}
	for token := range b {
		if isNumericToken(token) && !a[token] {
			return true
		}
	}
	return false
}