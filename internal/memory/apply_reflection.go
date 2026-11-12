package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EmissionDrops counts emissions one replace refused to write, by reason.
type EmissionDrops struct {
	Changed       int
	SameSecond    int
	Overflow      int
	OverflowLinks int
	Scope         int
	Retained      int
}

// Total reports the number of emissions refused for any reason.
func (d EmissionDrops) Total() int { return d.Changed + d.SameSecond + d.Overflow + d.Scope }

type dropReason int

const (
	dropNone dropReason = iota
	dropChanged
	dropSameSecond
	dropOverflow
	dropScope
)

func (d *EmissionDrops) record(reason dropReason, links int) {
	switch reason {
	case dropChanged:
		d.Changed++
	case dropSameSecond:
		d.SameSecond++
	case dropOverflow:
		d.Overflow++
		d.OverflowLinks += links
	case dropScope:
		d.Scope++
	}
}

func (d EmissionDrops) add(o EmissionDrops) EmissionDrops {
	return EmissionDrops{
		Changed:       d.Changed + o.Changed,
		SameSecond:    d.SameSecond + o.SameSecond,
		Overflow:      d.Overflow + o.Overflow,
		OverflowLinks: d.OverflowLinks + o.OverflowLinks,
		Scope:         d.Scope + o.Scope,
		Retained:      d.Retained + o.Retained,
	}
}

func (d EmissionDrops) sub(o EmissionDrops) EmissionDrops {
	return EmissionDrops{
		Changed:       d.Changed - o.Changed,
		SameSecond:    d.SameSecond - o.SameSecond,
		Overflow:      d.Overflow - o.Overflow,
		OverflowLinks: d.OverflowLinks - o.OverflowLinks,
		Scope:         d.Scope - o.Scope,
		Retained:      d.Retained - o.Retained,
	}
}

// ApplyReflection replaces project-scoped reflection results and may promote global candidates in the same transaction.
func (s *Store) ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []Memory, consolidatedSince string, promoteGlobals bool, fed []CorpusRow) (preserved []string, promoted int, keptMems []Memory, projectDropped, globalDropped EmissionDrops, deleted int, appliedByCategory map[string]int, err error) {
	if len(projectMems) == 0 && len(globalMems) == 0 {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, nil
	}

	// Caller-visible return must be non-nil even when no project replace runs.
	appliedByCategory = make(map[string]int)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "reflect-apply")
	if err != nil {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("begin reflection apply tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txCtx := withStoreTx(ctx, tx)
	view := &applyView{concurrent: make(map[string]bool), scopeKept: make(map[string]bool), overflowKept: make(map[string]bool), crossProjectFrom: -1}
	globalGuard, guardErr := s.fedGuardFor(txCtx, tx, projectID, consolidatedSince, fed, view.concurrent)
	if guardErr != nil {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, guardErr
	}
	txCtx = withApplyView(txCtx, view)

	globalMems, globalDropped = dropConcurrentClaims(globalGuard, globalMems)

	if (!promoteGlobals || projectID == GlobalProjectID) && len(globalMems) > 0 {
		view.crossProjectFrom = len(projectMems)
		projectMems = append(append([]Memory(nil), projectMems...), globalMems...)
		globalMems = nil
	}

	candidateIDs := make(map[string]bool, len(globalGuard.candidates))
	for _, c := range globalGuard.candidates {
		candidateIDs[c.id] = true
	}
	promotable := make([]Memory, 0, len(globalMems))
	globalScope := make([]map[string]string, 0, len(globalMems))
	for _, m := range globalMems {
		links, over, overErr := decisionLinksOverflowTx(txCtx, tx, projectID, emissionClaimIDs(globalGuard.candidates, m))
		if overErr != nil {
			return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, overErr
		}
		if over {
			moved := make(map[string]bool, len(view.concurrent))
			for id := range view.concurrent {
				moved[id] = true
			}
			for _, id := range keepEmissionClaims(globalGuard.candidates, view.concurrent, m) {
				if !moved[id] {
					view.overflowKept[id] = true
				}
			}
			globalDropped.record(dropOverflow, links)
			continue
		}
		sc, scopeErr := inheritedScope(txCtx, tx, projectID, m)
		if scopeErr != nil {
			if !errors.Is(scopeErr, errScopeConflict) {
				return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("resolve cross-project scope: %w", scopeErr)
			}
			for _, id := range m.ReplacesIDs {
				if candidateIDs[id] {
					if !view.concurrent[id] {
						view.scopeKept[id] = true
					}
					view.concurrent[id] = true
				}
			}
			globalDropped.record(dropScope, 0)
			continue
		}
		promotable = append(promotable, m)
		globalScope = append(globalScope, sc)
	}
	globalMems = promotable

	var ensureFailure error
	if len(globalMems) > 0 {
		ensureFailure = ensureGlobalProjectTx(ctx, tx)
		if ensureFailure == nil && len(projectMems) > 0 {
			for i, m := range globalMems {
				view.promotions = append(view.promotions, pendingPromotion{m: m, scope: globalScope[i]})
			}
		}
	}

	if len(projectMems) > 0 {
		var projectDrop EmissionDrops
		preserved, projectDrop, deleted, appliedByCategory, err = s.ReplaceNonManual(txCtx, projectID, projectMems, consolidatedSince, fed)
		if err != nil {
			return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("replace project memories: %w", err)
		}
		projectDropped = projectDrop
		projectDropped = projectDropped.sub(view.crossProjectDropped)
		globalDropped = globalDropped.add(view.crossProjectDropped)
		globalDropped = globalDropped.add(view.promotionDropped)
	}

	if len(globalMems) > 0 {
		failed := view.failedPromotions
		promoted = view.promoted
		switch {
		case ensureFailure != nil:
			var recoveryErr error
			keptMems, recoveryErr = s.upsertMemoriesTx(ctx, tx, projectID, globalMems)
			if recoveryErr != nil {
				return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("ensure _global: %v; return candidates to project: %w", ensureFailure, recoveryErr)
			}
			for _, m := range keptMems {
				appliedByCategory[m.Category]++
			}
		case len(projectMems) == 0:
			for i, m := range globalMems {
				_, _, promoteErr, candidateErr := s.promoteReflectionCandidate(ctx, tx, projectID, m, globalScope[i], nil)
				if candidateErr != nil {
					return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, candidateErr
				}
				if promoteErr != nil {
					failed = append(failed, failedPromotion{pendingPromotion: pendingPromotion{m: m, scope: globalScope[i]}, err: promoteErr})
					continue
				}
				promoted++
			}
		}
		for _, f := range failed {
			back := f.m
			back.Scope = f.scope
			if _, err := s.upsertMemoriesTx(ctx, tx, projectID, []Memory{back}); err != nil {
				return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("global promotion failed: %v; return candidate to project: %w", f.err, err)
			}
			keptMems = append(keptMems, f.m)
			appliedByCategory[f.m.Category]++
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("commit reflection apply tx: %w", err)
	}
	lock.reportHold("reflect-apply", time.Now())
	if s.onSave != nil {
		if len(projectMems) > 0 || len(keptMems) > 0 {
			s.onSave(projectID)
		}
		if promoted > 0 {
			s.onSave("_global")
		}
	}
	return preserved, promoted, keptMems, projectDropped, globalDropped, deleted, appliedByCategory, nil
}

func dropConcurrentClaims(g *fedGuard, memories []Memory) (live []Memory, dropped EmissionDrops) {
	if len(memories) == 0 {
		return memories, EmissionDrops{}
	}
	live = make([]Memory, 0, len(memories))
	for _, m := range memories {
		if reason := g.dropReasonFor(m); reason != dropNone {
			dropped.record(reason, 0)
			continue
		}
		live = append(live, m)
	}
	return live, dropped
}

func ensureGlobalProjectTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, updated_at = datetime('now')
	`); err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`); err != nil {
		return fmt.Errorf("ensure ghost state: %w", err)
	}
	return nil
}

type promotionSources struct {
	folded             Memory
	keptLive, consumed map[string]bool
}

func (s *Store) promoteReflectionCandidate(ctx context.Context, tx *sql.Tx, projectID string, m Memory, scope map[string]string, src *promotionSources) (id string, stamped bool, promoteErr, err error) {
	var tier string
	var consumedLinks int
	if src != nil {
		if tier, err = foldedRetention(ctx, tx, projectID, m, src.folded); err != nil {
			return "", false, nil, err
		}
		var consumedIDs []string
		for _, replaced := range m.ReplacesIDs {
			if src.consumed[replaced] {
				consumedIDs = append(consumedIDs, replaced)
			}
		}
		if consumedLinks, _, err = decisionLinksOverflowTx(ctx, tx, projectID, consumedIDs); err != nil {
			return "", false, nil, err
		}
	}

	const savepoint = "ghost_reflect_candidate"
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return "", false, nil, fmt.Errorf("savepoint global promotion: %w", err)
	}

	source := m.Source
	if source == "" {
		source = "reflection"
	}
	id, foldedInto, _, upsertErr := s.UpsertWithOptions(withStoreTx(ctx, tx), GlobalProjectID, m.Category, m.Content, source, m.Importance, m.Tags, UpsertOptions{
		Provenance:          provenanceFromMemory(m),
		Scope:               scope,
		FoldOnly:            true,
		Retention:           tier,
		foldReplaceableOnly: consumedLinks > 0,
	})
	if upsertErr == nil && src != nil {
		absorbs := foldedInto == ""
		if !absorbs {
			absorbs, upsertErr = replaceableRowTx(ctx, tx, id)
		}
		if upsertErr == nil && absorbs {
			stamped, upsertErr = absorbSourcesTx(ctx, tx, projectID, id, m, src.folded, src.keptLive, src.consumed)
			if errors.Is(upsertErr, errDecisionRefsOverflow) {
				links, countErr := foldedDecisionLinksTx(ctx, tx, projectID, id, m.ReplacesIDs)
				if countErr != nil {
					upsertErr = countErr
				} else {
					upsertErr = &promotionOverflowError{links: links}
				}
			}
		}
	}
	if upsertErr == nil {
		if _, err := tx.ExecContext(ctx, `RELEASE `+savepoint); err != nil {
			return "", false, nil, fmt.Errorf("release global promotion: %w", err)
		}
		return id, stamped, nil, nil
	}
	if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO `+savepoint); rollbackErr != nil {
		return "", false, nil, fmt.Errorf("global promotion failed: %v; rollback savepoint: %w", upsertErr, rollbackErr)
	}
	if _, releaseErr := tx.ExecContext(ctx, `RELEASE `+savepoint); releaseErr != nil {
		return "", false, nil, fmt.Errorf("global promotion failed: %v; release savepoint: %w", upsertErr, releaseErr)
	}
	return "", false, upsertErr, nil
}

type promotionOverflowError struct{ links int }

// Error reports the overflow refusal.
func (e *promotionOverflowError) Error() string {
	return fmt.Sprintf("%v (%d decision links)", errDecisionRefsOverflow, e.links)
}

// Unwrap returns the underlying overflow error.
func (e *promotionOverflowError) Unwrap() error { return errDecisionRefsOverflow }

func provenanceFromMemory(m Memory) Provenance {
	return Provenance{
		Agent:      m.Agent,
		SessionID:  m.SessionID,
		SourceRef:  m.SourceRef,
		Confidence: m.Confidence,
	}
}

func (s *Store) upsertMemoriesTx(ctx context.Context, tx *sql.Tx, projectID string, memories []Memory) ([]Memory, error) {
	written := make([]Memory, 0, len(memories))
	for _, m := range memories {
		category := m.Category
		if !IsValidCategory(category) {
			category = "fact"
		}
		source := m.Source
		if source == "" {
			source = "reflection"
		}
		if _, _, _, err := s.UpsertWithOptions(withStoreTx(ctx, tx), projectID, category, m.Content, source, m.Importance, m.Tags, UpsertOptions{
			Provenance: provenanceFromMemory(m),
			Scope:      m.Scope,
		}); err != nil {
			return written, err
		}
		written = append(written, m)
	}
	return written, nil
}
