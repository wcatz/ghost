package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EmissionDrops counts the emissions one replace refused to write, by reason.
// The reasons are not interchangeable and the caller treats them
// differently, which is why they are fields and not one integer (#1228):
//
//   - Changed is the #1051 corpus guard refusing a claim on a row that changed
//     or was deleted while the harness was answering. The row is still live and
//     still needs consolidating, so a later run has to revisit it, and a caller
//     that recorded a skip fingerprint over it would absorb the survivor.
//   - Overflow is a merge whose decision links alone do not fit in a source_ref.
//     That is a property of the corpus as it stands, so every later run would
//     refuse the same merge again: it is reported on its own, and the round it
//     happened in may record its skip fingerprint, because nothing was lost that
//     another run could recover.
//   - Scope is a merge whose sources' scopes disagree (inheritedScope's
//     errScopeConflict, or a reuse whose stored row carries a different scope
//     than the sources agreed on). It is permanent in the same way as Overflow —
//     the scopes are stored data, and every later run would refuse the same merge
//     for the same reason — so it is reported on its own and does not withhold
//     the skip fingerprint either (#1492).
//
// Total is the count of refused emissions for any reason, which is what "every
// proposal was refused" and "the round wrote nothing" read.
//
// Retained is a count of ROWS rather than emissions, and only a project replace
// fills it: the replaceable rows among the corpus it was handed that it leaves
// live, plus every row it inserts. A refusal writes no row of its own and takes
// none of its sources, so one refused emission is not one retained memory — an
// N-source merge the store refused leaves N rows in the project. Callers that
// report how much a round compressed read this rather than counting proposals.
type EmissionDrops struct {
	Changed  int
	Overflow int
	// OverflowLinks is how many decision links the overflowing merges stood for
	// in total. It is the reason's own number, printed beside the byte bound it
	// exceeds so an operator can see how far over the merge was.
	OverflowLinks int
	Scope         int
	// Retained is the rows a project replace leaves live from its corpus and the
	// rows it inserts. It is meaningful only when a project replace ran; a
	// global-only apply leaves it zero.
	Retained int
}

// Total is the number of emissions refused for any reason.
func (d EmissionDrops) Total() int { return d.Changed + d.Overflow + d.Scope }

// dropReason is why one emission was refused, so record can count it in the
// field that reason owns.
type dropReason int

const (
	dropChanged dropReason = iota
	dropOverflow
	dropScope
)

// record adds one refused emission under the reason it was refused for.
func (d *EmissionDrops) record(reason dropReason, links int) {
	switch reason {
	case dropOverflow:
		d.Overflow++
		d.OverflowLinks += links
	case dropScope:
		d.Scope++
	default:
		d.Changed++
	}
}

// add is the sum two halves' counts are combined with, and sub moves one half's
// refusals onto the half that produced them (see applyView.crossProjectDropped).
func (d EmissionDrops) add(o EmissionDrops) EmissionDrops {
	return EmissionDrops{
		Changed:       d.Changed + o.Changed,
		Overflow:      d.Overflow + o.Overflow,
		OverflowLinks: d.OverflowLinks + o.OverflowLinks,
		Scope:         d.Scope + o.Scope,
		Retained:      d.Retained + o.Retained,
	}
}

func (d EmissionDrops) sub(o EmissionDrops) EmissionDrops {
	return EmissionDrops{
		Changed:       d.Changed - o.Changed,
		Overflow:      d.Overflow - o.Overflow,
		OverflowLinks: d.OverflowLinks - o.OverflowLinks,
		Scope:         d.Scope - o.Scope,
		Retained:      d.Retained - o.Retained,
	}
}

// ApplyReflection replaces the project-scoped reflection result and, when
// requested, writes cross-project candidates in the same transaction. A
// candidate that cannot be written to _global is inserted into the project in
// that transaction; if that recovery write also fails, the whole transaction
// rolls back rather than leaving the snapshot's candidates in neither place.
// The returned keptMems are the cross-project candidates that could not be
// promoted and were written back into the project instead. The caller needs
// them, not just a count: a summary that reports how many rows the project now
// holds cannot be built from an integer when it also has to say which rows those
// are. A candidate whose sources disagree on scope is neither promoted nor kept:
// there is no scope to promote under, and writing it project-scoped with none
// would widen the scoped sources it replaced, so it is counted in globalDropped
// and its sources are preserved instead.
//
// projectDropped and globalDropped count the emissions each half refused, by
// reason (EmissionDrops): the ones whose claimed row changed or was deleted
// while the harness was answering, the merges whose decision links do not fit
// in a source_ref, and the merges whose sources' scopes disagree. Keep them attributable: once the two lists are folded
// nobody can say which input a dropped emission came from, so the global half is
// guarded before the fold below. The caller needs the split to report only what
// was written (a project summary and a cross-project component subtract
// different halves), to recognise a round that wrote nothing, and to tell a drop
// a later run can repair from one it cannot.
//
// deleted is the number of rows the project replace removed, and
// appliedByCategory is what it landed per category (including the kept-back
// candidates written into the project when promotion fails or _global is
// unavailable). Together with the two drop counts they let the caller tell a
// round that changed the corpus from one that did not: an all-dropped round
// that still deleted unclaimed candidates is not "nothing written", and must
// report the replace and offer its snapshot for restore.
func (s *Store) ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []Memory, consolidatedSince string, promoteGlobals bool, fed []CorpusRow) (preserved []string, promoted int, keptMems []Memory, projectDropped, globalDropped EmissionDrops, deleted int, appliedByCategory map[string]int, err error) {
	if len(projectMems) == 0 && len(globalMems) == 0 {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, nil
	}

	// The category map is a caller-visible return, so it must never come back
	// nil even when no project replace ran: a global-only apply that lands a
	// kept-back candidate records it here, and that write happens on a map the
	// replace did not build. ReplaceNonManual overwrites it with its own counts
	// when the project half runs.
	appliedByCategory = make(map[string]int)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "reflect-apply")
	if err != nil {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("begin reflection apply tx: %w", err)
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
	// The view is the shared state itself: fedGuardFor fills its concurrent map
	// with the rows saved or edited since consolidation, and dropConcurrentClaims
	// below additionally marks every source of an emission it refuses there. That
	// second marking must reach the project replace, or the unchanged source of a
	// refused cross-project merge matches no surviving emission and the replace
	// deletes it as unaccounted — the merge was refused, so NONE of its sources
	// may be taken. ReplaceNonManual seeds its own concurrent map from this one,
	// and counts back the refusals it makes about folded candidates.
	view := &applyView{concurrent: make(map[string]bool), scopeKept: make(map[string]bool), overflowKept: make(map[string]bool), crossProjectFrom: -1}
	globalGuard, guardErr := s.fedGuardFor(txCtx, tx, projectID, consolidatedSince, fed, view.concurrent)
	if guardErr != nil {
		return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, guardErr
	}
	txCtx = withApplyView(txCtx, view)

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

	// A reflect of _global itself has nowhere to promote TO: its cross-project
	// candidates already stand in the project they would be promoted into, so they
	// take the replace path, which inherits everything. Promoting them inside the
	// replace over _global would let a FoldOnly fold land on a row the same
	// replace is about to delete.
	if (!promoteGlobals || projectID == GlobalProjectID) && len(globalMems) > 0 {
		// From here on the replace's input ENDS with the folded candidates, so a
		// refusal at or after this index is a refusal of a cross-project
		// candidate even though the replace is what made it.
		view.crossProjectFrom = len(projectMems)
		projectMems = append(append([]Memory(nil), projectMems...), globalMems...)
		globalMems = nil
	}

	// Resolve the scope each surviving cross-project candidate inherits from the
	// rows it stands in for, BEFORE the project replace below deletes them. That
	// replace removes every replaceable row no project emission accounts for, and
	// a row merged into a cross-project candidate is exactly one of those, so a
	// scope read after it comes back empty and the promotion writes unscoped
	// whatever the sources said. Resolved here so the promotion half is held to
	// the same rule as the project half: an agreement is carried onto the
	// promoted row, and a disagreement (or a scoped/unscoped mix) is not a scope
	// to promote under, so the candidate is refused.
	//
	// A refusal keeps its sources, exactly as the project half's does
	// (keepRefusedSources): the merge is void, so NONE of its sources may be
	// taken, and they are marked in the shared view ReplaceNonManual seeds its
	// concurrent map from — else the replace deletes every one of them as
	// unaccounted and the knowledge is gone with them. The candidate itself is
	// written nowhere and counted as a dropped global emission: writing it into
	// the project with no scope would replace its scoped sources with an unscoped
	// row, the same silent widening one level in, and promoting it would widen
	// across every project.
	//
	// A read or decode failure is not a refusal and fails the apply, exactly as
	// the project half propagates it.
	candidateIDs := make(map[string]bool, len(globalGuard.candidates))
	for _, c := range globalGuard.candidates {
		candidateIDs[c.id] = true
	}
	promotable := make([]Memory, 0, len(globalMems))
	globalScope := make([]map[string]string, 0, len(globalMems))
	for _, m := range globalMems {
		// A merge whose decision links alone do not fit in a source_ref is refused
		// as the project half refuses it (#1228): truncating the list is the
		// silent supersede miss, and the merge is void, so its claims are kept —
		// every source it names and every row carrying its text — and recorded as
		// overflow-kept, so the replace does not report them as survivors a later
		// run could merge. A source the guard already marked moved stays a
		// survivor.
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
			// A source the guard already marked moved stays a survivor; one this
			// refusal alone keeps is recorded as scope-kept, so the project
			// replace does not report it as a row a later run has to merge (#1492).
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

	// The global bucket is ensured BEFORE the replace, because with a project
	// half the promotions run inside it (applyView.promotions): that is the one
	// point where the replace knows which sources it consumes, and the sources
	// still exist to be inherited from. When the bucket is unavailable every
	// candidate is kept project-scoped after the replace instead, as before.
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
		// Move the replace's refusals of folded candidates onto the half that
		// produced them. The caller's summary subtracts each half's drops from
		// a different component — the cross-project count only knows the global
		// half's — so a folded refusal left in projectDropped makes the Applied
		// line claim a cross-project candidate was kept project-scoped beside a
		// total that never counted it. The sum the caller adds back is
		// unchanged: this is attribution, not a second drop.
		projectDropped = projectDropped.sub(view.crossProjectDropped)
		globalDropped = globalDropped.add(view.crossProjectDropped)
		// And the promotions the replace refused at the write, which only the
		// global half ever counted.
		globalDropped = globalDropped.add(view.promotionDropped)
	}

	if len(globalMems) > 0 {
		failed := view.failedPromotions
		promoted = view.promoted
		switch {
		case ensureFailure != nil:
			// The global bucket is unavailable, but the project-side write may
			// still be healthy. Keep each candidate project-scoped instead of
			// turning an infrastructure error into data loss.
			var recoveryErr error
			keptMems, recoveryErr = s.upsertMemoriesTx(ctx, tx, projectID, globalMems)
			if recoveryErr != nil {
				return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("ensure _global: %v; return candidates to project: %w", ensureFailure, recoveryErr)
			}
			// The recovered rows landed in the project, so they belong in the
			// applied summary and its category breakdown exactly like the
			// replace's own writes.
			for _, m := range keptMems {
				appliedByCategory[m.Category]++
			}
		case len(projectMems) == 0:
			// A global-only apply runs no replace, so it consumes nothing: every
			// source stays live and the promoted row is nobody's successor. It
			// inherits nothing from them (a nil promotionSources) — a copy of a
			// live row's records or claims would count the same support twice.
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
		// A candidate whose promotion was rolled back is written into the project
		// AFTER the replace: written inside it, the upsert's duplicate fold could
		// land on a row the replace is about to delete. The recovery row keeps the
		// scope the candidate inherited, since an unscoped write-back would widen a
		// scoped fact inside the project.
		for _, f := range failed {
			back := f.m
			back.Scope = f.scope
			if _, err := s.upsertMemoriesTx(ctx, tx, projectID, []Memory{back}); err != nil {
				return nil, 0, nil, EmissionDrops{}, EmissionDrops{}, 0, nil, fmt.Errorf("global promotion failed: %v; return candidate to project: %w", f.err, err)
			}
			keptMems = append(keptMems, f.m)
			// Written back into the project (the one place a failed promotion
			// leaves the row), so it counts as applied there.
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

// dropConcurrentClaims removes the emissions the corpus guard refuses — their
// claimed row changed or was deleted while the harness was answering — and
// returns how many were refused, so the caller can report only what was written
// and can tell a round that wrote nothing from one that did.
func dropConcurrentClaims(g *fedGuard, memories []Memory) (live []Memory, dropped EmissionDrops) {
	if len(memories) == 0 {
		return memories, EmissionDrops{}
	}
	live = make([]Memory, 0, len(memories))
	for _, m := range memories {
		if g.drop(m) {
			dropped.record(dropChanged, 0)
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

// promotionSources is what a promoted merge inherits from, as the replace that
// runs the promotion splits it: folded is the candidate with ReplacesIDs cut to
// the sources the replace consumes (sourcesNotKeptLive), keptLive the
// candidates it keeps live and consumed its delete set.
type promotionSources struct {
	folded             Memory
	keptLive, consumed map[string]bool
}

// promoteReflectionCandidate uses the normal Upsert duplicate/link semantics
// against the open transaction. A savepoint makes a failed global upsert
// recoverable: the failed statement's partial writes are discarded, and the
// caller writes the candidate into the project instead (promoteErr). err is a
// failure that leaves the transaction unusable and fails the apply.
//
// src is nil for a global-only apply, which consumes nothing. Otherwise the
// promoted row is the successor of the sources the replace consumes, and it
// inherits by the project path's own rule rather than a copy of it: the tier is
// foldedRetention, handed to the upsert as its Retention (a fresh row is written
// at that tier with its expiry, a fold raises its target as any fold does), and
// the provenance stamp, records and claims are absorbSourcesTx, the reuse
// branch's own, because the promoted row is in the reused row's position. The
// claims are deliberately not passed through UpsertOptions: a stated VerifiedAt
// makes the upsert mint a verification record nobody performed, and the fold's
// COALESCE would replace a target's own decision link instead of extending it.
//
// A row the replace could not touch — manual, builtin, pinned or
// retention-exempt — takes nothing from the sources: the project path never
// merges into such a row either, and a decision link written onto it would let
// a supersede retire a row the user made or protected. When the promotion
// consumes a source carrying a decision link, its duplicate lookups skip such
// rows (UpsertOptions.foldReplaceableOnly): it folds into the best replaceable
// match, such as the carrier an earlier round inserted, or is inserted as a
// fresh _global row beside the protected one, as a project emission is
// inserted beside a manual row, so the link has a live carrier a supersede can
// retire. Any other promotion may still fold into a protected row and
// strengthen it, as before, and the consumed sources' evidence is then deleted
// with them, not carried. stamped reports that the provenance stamp wrote, so
// the replace files the version.
//
// A fold target whose own decision links leave no room for the sources' is
// only found here, at the write: the absorb is refused as boundedDecisionRefs
// refuses it, and promoteErr is a *promotionOverflowError naming the links the
// merge stood for, so the replace keeps the sources instead of consuming them.
//
// A candidate that cannot be promoted is never written here: the caller does
// that after the replace, for the reason given at its write-back.
//
// scope is the scope the candidate inherited from the project rows it stands
// in for, resolved by the caller BEFORE the project replace deleted those rows
// (the candidate carries none of its own). Passing it onto the _global upsert
// keeps a production-only fact from landing in _global unscoped and applying in
// every environment, and arms UpsertWithOptions' scope-conflict clause against
// an existing _global duplicate. A candidate whose sources disagreed is never
// promoted, so this is always a scope the sources agreed on (or nil when they
// stated none).
func (s *Store) promoteReflectionCandidate(ctx context.Context, tx *sql.Tx, projectID string, m Memory, scope map[string]string, src *promotionSources) (id string, stamped bool, promoteErr, err error) {
	// Read before the savepoint: a read failure here is the project path's
	// failure too (it aborts the replace), not a promotion the project can take
	// back instead.
	var tier string
	var consumedLinks int
	if src != nil {
		if tier, err = foldedRetention(ctx, tx, projectID, m, src.folded); err != nil {
			return "", false, nil, err
		}
		// The decision links the sources this promotion consumes carry, which
		// only the promoted row can keep live once the replace deletes them.
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
	id, foldedInto, _, upsertErr := s.UpsertWithOptions(withStoreTx(ctx, tx), GlobalProjectID, m.Category, m.Content, source, m.Importance, m.Tags, UpsertOptions{
		Provenance: provenanceFromMemory(m),
		Scope:      scope,
		FoldOnly:   true,
		Retention:  tier,
		// A row the replace could not merge into cannot carry a consumed
		// source's decision link, so the merge gets a row of its own beside it.
		foldReplaceableOnly: consumedLinks > 0,
	})
	if upsertErr == nil && src != nil {
		// A fresh row is the promotion's own; a fold target takes the sources only
		// when the replace could have merged into it.
		absorbs := foldedInto == ""
		if !absorbs {
			absorbs, upsertErr = replaceableRowTx(ctx, tx, id)
		}
		if upsertErr == nil && absorbs {
			stamped, upsertErr = absorbSourcesTx(ctx, tx, projectID, id, m, src.folded, src.keptLive, src.consumed)
			if errors.Is(upsertErr, errDecisionRefsOverflow) {
				// Counted before the rollback below, while the target's own links
				// are still the ones it held, for the drop the replace records.
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

// promotionOverflowError is a promotion refused at the write because its fold
// target's own decision links and the sources' do not fit in one source_ref.
// links is how many distinct links the merge stood for, for the overflow drop.
type promotionOverflowError struct{ links int }

func (e *promotionOverflowError) Error() string {
	return fmt.Sprintf("%v (%d decision links)", errDecisionRefsOverflow, e.links)
}

func (e *promotionOverflowError) Unwrap() error { return errDecisionRefsOverflow }

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
