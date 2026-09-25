package memory

import (
	"context"
	"database/sql"
	"fmt"
)

// ApplyReflection replaces the project-scoped reflection result and, when
// requested, writes cross-project candidates in the same transaction. A
// candidate that cannot be written to _global is inserted into the project in
// that transaction; if that recovery write also fails, the whole transaction
// rolls back rather than leaving the snapshot's candidates in neither place.
func (s *Store) ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []Memory, consolidatedSince string, promoteGlobals bool) (preserved []string, promoted, keptProject int, err error) {
	if !promoteGlobals && len(globalMems) > 0 {
		projectMems = append(append([]Memory(nil), projectMems...), globalMems...)
		globalMems = nil
	}
	if len(projectMems) == 0 && len(globalMems) == 0 {
		return nil, 0, 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("begin reflection apply tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	txCtx := withStoreTx(ctx, tx)
	if len(projectMems) > 0 {
		preserved, err = s.ReplaceNonManual(txCtx, projectID, projectMems, consolidatedSince)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("replace project memories: %w", err)
		}
	}

	if len(globalMems) > 0 {
		if ensureErr := ensureGlobalProjectTx(ctx, tx); ensureErr != nil {
			// The global bucket is unavailable, but the project-side write may
			// still be healthy. Keep each candidate project-scoped instead of
			// turning an infrastructure error into data loss.
			var recoveryErr error
			keptProject, recoveryErr = s.upsertMemoriesTx(ctx, tx, projectID, globalMems)
			if recoveryErr != nil {
				return nil, 0, 0, fmt.Errorf("ensure _global: %v; return candidates to project: %w", ensureErr, recoveryErr)
			}
		} else {
			for _, m := range globalMems {
				promotedOK, kept, candidateErr := s.promoteReflectionCandidate(ctx, tx, projectID, m)
				if candidateErr != nil {
					return nil, 0, 0, candidateErr
				}
				if promotedOK {
					promoted++
				} else if kept {
					keptProject++
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, 0, fmt.Errorf("commit reflection apply tx: %w", err)
	}
	if s.onSave != nil {
		if len(projectMems) > 0 || keptProject > 0 {
			s.onSave(projectID)
		}
		if promoted > 0 {
			s.onSave("_global")
		}
	}
	return preserved, promoted, keptProject, nil
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

func (s *Store) upsertMemoriesTx(ctx context.Context, tx *sql.Tx, projectID string, memories []Memory) (int, error) {
	upserted := 0
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
			return upserted, err
		}
		upserted++
	}
	return upserted, nil
}
