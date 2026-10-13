package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ApplyReflection replaces the project-scoped reflection result and, when
// requested, writes cross-project candidates in the same transaction. A
// candidate that cannot be written to _global is inserted into the project in
// that transaction; if that recovery write also fails, the whole transaction
// rolls back rather than leaving the snapshot's candidates in neither place.
// The returned keptMems are the cross-project candidates that could not be
// promoted and were written back into the project instead. The caller needs
// them, not just a count: a summary that reports how many rows the project now
// holds cannot be built from an integer when it also has to say which rows those
// are.
//
// projectDropped and globalDropped count the emissions the #1051 corpus guard
// refused in each half — the ones whose claimed row changed or was deleted
// while the harness was answering. Keep them attributable: once the two lists
// are folded nobody can say which input a dropped emission came from, so the
// global half is guarded before the fold below. The caller needs the split to
// report only what was written (a project summary and a cross-project component
// subtract different halves) and to recognise a round that wrote nothing.
func (s *Store) ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []Memory, consolidatedSince string, promoteGlobals bool, fed []CorpusRow) (preserved []string, promoted int, keptMems []Memory, projectDropped, globalDropped int, err error) {
	if len(projectMems) == 0 && len(globalMems) == 0 {
		return nil, 0, nil, 0, 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "reflect-apply")
	if err != nil {
		return nil, 0, nil, 0, 0, fmt.Errorf("begin reflection apply tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txCtx := withStoreTx(ctx, tx)
	// The corpus guard has to cover the global half too (#1051). A cross-project
	// candidate is a claim on the project row it was consolidated from — by the
	// text it restates, or by the ids it names in ReplacesIDs — and the promotion
	// loop below writes it into _global, where the project's reuse map is never
	// consulted: without this check a concurrently deleted project row came back
	// in _global, and a global emission merging a concurrently edited project
	// row was written beside the edit. The guard is read BEFORE the project
	// replace so that half and this one answer from a single view of the project,
	// and it is the same fed corpus the CLI handed to the replace.
	//
	// The view is the map itself, shared with the replace: fedGuardFor fills it
	// with the rows saved or edited since consolidation, and dropConcurrentClaims
	// below additionally marks every source of an emission it refuses there. That
	// second marking must reach the project replace, or the unchanged source of a
	// refused cross-project merge matches no surviving emission and the replace
	// deletes it as unaccounted — the merge was refused, so NONE of its sources
	// may be taken. ReplaceNonManual seeds its own concurrent map from this one.
	sharedConcurrent := make(map[string]bool)
	globalGuard, guardErr := s.fedGuardFor(txCtx, tx, projectID, consolidatedSince, fed, sharedConcurrent)
	if guardErr != nil {
		return nil, 0, nil, 0, 0, guardErr
	}
	txCtx = withConcurrentView(txCtx, sharedConcurrent)

	// A candidate the guard drops is dropped entirely: it is neither promoted
	// nor recovered into the project, and with promotion off it is not folded
	// back for the project replace either. Writing it back into the project
	// would resurrect the row a concurrent writer deleted, which is the loss
	// the guard exists to prevent, and it is not reported as kept either — it
	// is a claim the pass is not allowed to make, not a cross-project fact that
	// failed to promote. Guarding BEFORE the fold is what keeps the two halves'
	// drop counts attributable: once the lists are merged nobody can say which
	// input a dropped emission came from, and the caller's summary subtracts
	// different halves in different flag modes.
	globalMems, globalDropped = dropConcurrentClaims(globalGuard, globalMems)

	if !promoteGlobals && len(globalMems) > 0 {
		projectMems = append(append([]Memory(nil), projectMems...), globalMems...)
		globalMems = nil
	}

	if len(projectMems) > 0 {
		var projectDrop int
		preserved, projectDrop, err = s.ReplaceNonManual(txCtx, projectID, projectMems, consolidatedSince, fed)
		if err != nil {
			return nil, 0, nil, 0, 0, fmt.Errorf("replace project memories: %w", err)
		}
		projectDropped = projectDrop
	}

	if len(globalMems) > 0 {
		if ensureErr := ensureGlobalProjectTx(ctx, tx); ensureErr != nil {
			// The global bucket is unavailable, but the project-side write may
			// still be healthy. Keep each candidate project-scoped instead of
			// turning an infrastructure error into data loss.
			var recoveryErr error
			keptMems, recoveryErr = s.upsertMemoriesTx(ctx, tx, projectID, globalMems)
			if recoveryErr != nil {
				return nil, 0, nil, 0, 0, fmt.Errorf("ensure _global: %v; return candidates to project: %w", ensureErr, recoveryErr)
			}
		} else {
			for _, m := range globalMems {
				promotedOK, kept, candidateErr := s.promoteReflectionCandidate(ctx, tx, projectID, m)
				if candidateErr != nil {
					return nil, 0, nil, 0, 0, candidateErr
				}
				if promotedOK {
					promoted++
				} else if kept {
					keptMems = append(keptMems, m)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, nil, 0, 0, fmt.Errorf("commit reflection apply tx: %w", err)
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
	return preserved, promoted, keptMems, projectDropped, globalDropped, nil
}

// dropConcurrentClaims removes the emissions the corpus guard refuses — their
// claimed row changed or was deleted while the harness was answering — and
// returns how many were refused, so the caller can report only what was written
// and can tell a round that wrote nothing from one that did.
func dropConcurrentClaims(g *fedGuard, memories []Memory) (live []Memory, dropped int) {
	if len(memories) == 0 {
		return memories, 0
	}
	live = make([]Memory, 0, len(memories))
	for _, m := range memories {
		if g.drop(m) {
			dropped++
			continue
		}
		live = append(live, m)
	}
	return live, dropped
}

// ensureGlobalProjectTx creates the bucket without taking Store.mu or opening
// a nested transaction. ApplyReflection already owns both; the global project
// has a fixed identity and never participates in path/repository merges.
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

// promoteReflectionCandidate uses the normal Upsert duplicate/link semantics
// against the open transaction. A savepoint makes a failed global upsert
// recoverable: the failed statement's partial writes are discarded before the
// candidate is inserted into the project instead.
func (s *Store) promoteReflectionCandidate(ctx context.Context, tx *sql.Tx, projectID string, m Memory) (promoted, kept bool, err error) {
	const savepoint = "ghost_reflect_candidate"
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return false, false, fmt.Errorf("savepoint global promotion: %w", err)
	}

	source := m.Source
	if source == "" {
		source = "reflection"
	}
	// FoldOnly, and only here. This is the one caller that is promoting a fact
	// _global already knows: the same cross-project fact arrives as a fresh
	// paraphrase from every project's reflect, and the default fold would store
	// each of them as its own row — which is issue #544, 68 redundant rows in 19
	// clusters, nine of them paraphrases of one gouroboros fact.
	//
	// Deliberately NOT in upsertMemoriesTx below. That is the recovery path: a
	// candidate that could not be promoted is going back into the project
	// verbatim, and folding there would silently drop the exact text the caller
	// asked to keep.
	_, _, _, upsertErr := s.UpsertWithOptions(withStoreTx(ctx, tx), "_global", m.Category, m.Content, source, m.Importance, m.Tags, UpsertOptions{
		Provenance: provenanceFromMemory(m),
		Scope:      m.Scope,
		FoldOnly:   true,
	})
	if upsertErr == nil {
		if _, err := tx.ExecContext(ctx, `RELEASE `+savepoint); err != nil {
			return false, false, fmt.Errorf("release global promotion: %w", err)
		}
		return true, false, nil
	}
	if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO `+savepoint); rollbackErr != nil {
		return false, false, fmt.Errorf("global promotion failed: %v; rollback savepoint: %w", upsertErr, rollbackErr)
	}
	if _, releaseErr := tx.ExecContext(ctx, `RELEASE `+savepoint); releaseErr != nil {
		return false, false, fmt.Errorf("global promotion failed: %v; release savepoint: %w", upsertErr, releaseErr)
	}

	if _, err := s.upsertMemoriesTx(ctx, tx, projectID, []Memory{m}); err != nil {
		return false, false, fmt.Errorf("global promotion failed: %v; return candidate to project: %w", upsertErr, err)
	}
	return false, true, nil
}

// upsertMemoriesTx uses the same duplicate probing, scope-conflict checks,
// metadata writes, and duplicate links as a normal project save while keeping
// the call inside ApplyReflection's transaction.
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
