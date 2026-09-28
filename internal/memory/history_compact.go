package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Issue #730. Before #727 every applied reflection appended a byte-identical
// `reflect` version for each memory it kept and set that memory's updated_at to
// the run's own time. #727 stopped writing new ones; this is the repair for the
// ones already stored, and it is two repairs because the damage is two things:
//
//   - memory_history is mostly rows that changed nothing, which crowds real
//     events toward the per-memory and per-store retention caps, and
//   - memories.updated_at holds the time of the last reflect rather than the
//     time of the last real change, so supersede orientation and
//     --skip-unchanged's fingerprint stay wrong until each row is next edited.
//
// The first is a DELETE and the second is a WRITE, so they are two options and
// not one: a caller who wants the history back under its caps does not have to
// accept a stamp moving.

// historyVersionColumns are the columns a history VERSION records: the state the
// memory held once that write landed, which is what makes a row a version and not
// a diff (#578). The compaction's equality predicate compares these and nothing
// else, and they are a list rather than a hand-written conjunction because
// TestCompactHistoryColumnPartitionNamesEveryColumn holds the partition complete: a
// column added to the table and to none of the three lists is compared by none of
// them, which is exactly how a state change stops being one.
var historyVersionColumns = []string{"content", "category", "importance", "resolved_at", "source"}

// historyEventColumns are the columns describing the EVENT rather than the state:
// which memory it is about, which project paid for it, which write path performed
// it, who did, and the event's other end — related_id, the id that replaced a
// deleted row or whose edge claims it, and merged_content, the wording a FoldOnly
// fold dropped. None is part of "the state the memory held", which is why the
// predicate does not compare them and why the two naming ANOTHER memory are
// checked for emptiness instead: such a row is a thread a reader follows from one
// memory's history into its successor's (#648), and it records a claim, not a
// state.
var historyEventColumns = []string{
	"memory_id", "project_id", "phase", "agent", "session_id", "related_id", "merged_content",
}

// historyRowColumns are the row's own identity and the time it was recorded.
// Neither is comparable between two versions of one memory — every row has a
// distinct id by construction, and recorded_at is precisely what DIFFERS between
// two otherwise-identical rows — so neither takes part in the predicate. They are
// listed so the partition of the table's columns is complete.
var historyRowColumns = []string{"id", "recorded_at"}

// historyCompactBatchSize is how many rows one compaction batch touches: the
// delete's LIMIT and the updated_at pass's page. Bounded for the same reason
// pruneHistoryTx bounds its statements, and with more force — prune runs on the
// write path of every save, while this runs once, deliberately, by an operator
// holding the store still. One transaction over a whole store's history would
// hold the write lock for the length of it, and OpenDB pins MaxOpenConns(1), so
// every other writer in the machine queues behind it.
//
// A var only so the batching test can lower it and drive more than one batch
// against a fixture small enough to read; nothing in production assigns to it.
var historyCompactBatchSize = 500

// compactablePhases are the phases a redundant version may be removed from, and
// it is an ALLOWLIST rather than the issue's list of what must survive. The two
// are the same set over the schema's CHECK list, and the allowlist is the safer
// spelling: a phase added to memory_history later is not deletable until somebody
// classifies it here, so a new event's first redundant version is a row that
// stays rather than one that disappears because a name nobody thought about is
// missing from a list of exemptions.
//
// `save`, `update`, `reflect` and `baseline` record a state and nothing else.
// `delete` is the tombstone and the only record a deleted memory left;
// `supersede`/`unsupersede` record a claim about the memory's standing, and only
// the sequence says which claim is live; `resolve`/`unresolve` say who decided;
// `merge` records the fold; `import` and `restore` say a row arrived from
// outside. None of those is recoverable from the state it happens to record.
func compactablePhases() []string {
	return []string{phaseSave, phaseUpdate, phaseReflect, phaseBaseline}
}

// placeholders is n question marks, comma separated, for an IN list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// historyEqualPredecessorSQL is THE comparison, and the reason it is a function
// rather than a conjunction spelled into two statements is that it has three
// callers that must agree: the delete, the dry run's count, and the updated_at
// pass's "did this change anything" — and the last of those uses it NEGATED.
//
// `outer` names the row under test. The test is an EXISTS against the row before
// it of the same memory, in rowid order and never recorded_at order: recorded_at
// is second-precision, so every write one reflection makes in a pass shares a
// timestamp and ordering by it would make the sequence a coin toss (the same
// reason pruneHistoryTx ranks by rowid). A memory's FIRST version has no
// predecessor, the max is NULL, nothing matches, and the row reads as a change —
// which is the answer it has to give, since the first version is never removable.
//
// `IS` and not `=`, and the reason is load-bearing: every state column is
// nullable (a delete row is written from the last live state, and an older build
// could leave gaps), and SQL's `=` answers NULL for NULL, so two rows both holding
// no resolved_at would compare UNEQUAL and every such pair would read as a change.
// That is the opposite of the damage this repairs.
func historyEqualPredecessorSQL(outer string) string {
	eq := make([]string, 0, len(historyVersionColumns))
	for _, c := range historyVersionColumns {
		eq = append(eq, "p."+c+" IS "+outer+"."+c)
	}
	return "EXISTS (SELECT 1 FROM memory_history p WHERE p.rowid = (" +
		"SELECT max(q.rowid) FROM memory_history q WHERE q.memory_id = " + outer + ".memory_id AND q.rowid < " + outer + ".rowid" +
		") AND " + strings.Join(eq, " AND ") + ")"
}

// historyNewestVersionSQL is a memory's newest version row, by rowid.
//
// Deliberately NOT narrowed to one project, unlike the rest of the scan: a memory
// that was promoted to _global or merged into another project has its earlier
// versions under the old one, so a project-narrowed "newest" would name a row in
// the middle of the memory's past and the guard below would spare the wrong one.
// The lookup is an index seek on idx_history_memory's leading column.
func historyNewestVersionSQL(memoryID string) string {
	return "(SELECT max(n.rowid) FROM memory_history n WHERE n.memory_id = " + memoryID + ")"
}

// historyRemovableRowSQL is the full predicate, and it is what #730 deletes by:
// a row that recorded exactly what the row before it of the same memory recorded,
// is not that memory's newest version, names no other memory, and is of a phase
// whose only content is the state it recorded.
//
// The newest-version guard is the one that makes the repair safe on a store that
// keeps running the lifecycle, and it is the same rule pruneHistoryTx already
// applies to its per-memory trim: "Ranking by rowid DESC also means the predicate
// can never take a memory's newest row: it is rank 1." A memory's newest version
// is the statement of what it says now, so nothing removes it — not the growth
// policy, and not this.
//
// It is also what keeps the store and the repair from disagreeing. ONE case needs
// it, and it is the one place a CURRENT build still writes a version that
// restates the state byte for byte: ReplaceNonManual's reusePreservesAge branch
// (#623) re-tags or re-scopes a reused row, and the version it appends records
// content, category, importance, resolved_at and source — none of which moved —
// because this table has no column for tags or scope. #727 was right that the row
// is a real change and recorded on purpose, and the guard is what reconciles that
// with removing byte-identical rows: the change is the memory's newest version, so
// it is kept, and only the SECOND restatement of the same restatement is
// removable. A store running the lifecycle therefore settles at one such row per
// memory rather than re-growing the table this command just pruned, and
// --fix-updated-at leaves that row's bump alone (see restorableStamp).
func historyRemovableRowSQL(outer string) string {
	return historyEqualPredecessorSQL(outer) +
		" AND " + outer + ".rowid <> " + historyNewestVersionSQL(outer+".memory_id") +
		" AND " + outer + ".phase IN (" + placeholders(len(compactablePhases())) + ")" +
		" AND " + outer + ".related_id IS NULL AND " + outer + ".merged_content IS NULL"
}

// A compaction statement and its arguments are built by ONE function each, because
// SQLite binds positionally and a statement whose placeholder order lives apart
// from its argument list is one that can be run with the right values in the wrong
// slots — which SQLite reports as a type error, or worse accepts silently.

// compactDeleteStmt removes at most historyCompactBatchSize removable versions of
// one project's memories, oldest rowid first. The batch is the DELETE's own LIMIT
// over an ordered sub-select of rowids, so the next batch starts where the last
// stopped with no cursor argument that could disagree with it.
func compactDeleteStmt(projectID string) (string, []any) {
	sql := `DELETE FROM memory_history WHERE rowid IN (
	    SELECT h.rowid FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h") + `
	    ORDER BY h.rowid
	    LIMIT ?)`
	return sql, append(compactPhaseArgs(projectID), historyCompactBatchSize)
}

// compactCountStmt is the same predicate, counted.
func compactCountStmt(projectID string) (string, []any) {
	sql := `SELECT count(*) FROM memory_history h
	    WHERE h.project_id = ? AND ` + historyRemovableRowSQL("h")
	return sql, compactPhaseArgs(projectID)
}

// compactCandidatesStmt is the updated_at pass's read. Per LIVE memory of one
// project it returns the recorded_at of the newest version that CHANGED its state
// (c.target), the updated_at the row holds now, and the evidence the decision in
// restorableStamp needs: the newest removable rowid for that memory and how many
// rows this project held for it.
//
// The evidence is the same predicate the delete used, spelled with the same three
// functions, and it is read for EVERY memory rather than only the ones this run
// removed from — the decision is made in Go and both modes make it, so the dry
// run's count and the apply's write cannot disagree, and a memory whose rows were
// removed by an earlier batch of the same pass is still judged correctly.
//
// The join to memories is what makes this the updated_at repair rather than a
// second history pass: a deleted memory has no row to stamp, and its tombstone is
// evidence about a memory that is gone. The cursor pages the scan and is stable
// across batches, because a restore only ever moves a stamp backward and never
// changes which project a memory belongs to — so a memory cannot move between
// batches while the pass runs. A cursor rather than OFFSET, for the reason
// pruneHistoryTx uses a rowid window: an OFFSET over a result set the previous
// batch's writes could resize would skip rows. The cursor appears twice because
// the statement accepts an empty one for the first page rather than carrying a
// separate first-page spelling: `? = ” OR c.memory_id > ?` is a branch SQLite folds
// away, and two statements differing only in their WHERE clause are two things to
// keep in step.
func compactCandidatesStmt(projectID, cursor string) (string, []any) {
	// The evidence the decision needs, spelled with the delete's own predicate and
	// over the same project, so "was there a version NEWER than the last real
	// change that said nothing" is answered by the delete's rule rather than by a
	// second one. It is a correlated sub-select rather than a join because it has to
	// name the memory the GROUP BY is over, and a sub-select in a FROM clause cannot
	// see a sibling FROM item.
	evidence := `COALESCE((SELECT max(r.rowid) FROM memory_history r
	        WHERE r.memory_id = h.memory_id AND r.project_id = ? AND ` + historyRemovableRowSQL("r") + `), 0)`
	sql := `SELECT c.memory_id, c.target, t.recorded_at, m.updated_at, c.removable_last
	    FROM (
	        SELECT h.memory_id AS memory_id, max(h.rowid) AS target, ` + evidence + ` AS removable_last
	        FROM memory_history h
	        WHERE h.project_id = ? AND NOT ` + historyEqualPredecessorSQL("h") + `
	        GROUP BY h.memory_id
	    ) c
	    JOIN memory_history t ON t.rowid = c.target
	    JOIN memories m ON m.id = c.memory_id AND m.project_id = ?
	    WHERE (? = '' OR c.memory_id > ?)
	    ORDER BY c.memory_id
	    LIMIT ?`
	// Textual order: the evidence sub-select's project and phase list, then the
	// changed-state scan's project, then the live row's project, then the cursor
	// and the page.
	args := compactPhaseArgs(projectID)
	args = append(args, projectID, projectID, cursor, cursor, historyCompactBatchSize)
	return sql, args
}

// historyCompactRestoreSQL writes one memory's restored stamp. The project is bound
// in the statement as well as in the read that selected the candidate, so a memory
// another process moved between the two is not stamped by a run scoped to a project
// it no longer belongs to.
const historyCompactRestoreSQL = `UPDATE memories SET updated_at = ? WHERE id = ? AND project_id = ?`

// compactPhaseArgs binds the project predicate and then compactablePhases, in the
// order the statements name them.
func compactPhaseArgs(projectID string) []any {
	args := make([]any, 0, len(compactablePhases())+1)
	args = append(args, projectID)
	for _, p := range compactablePhases() {
		args = append(args, p)
	}
	return args
}

// HistoryCompactOptions is what one project's compaction does.
type HistoryCompactOptions struct {
	// Apply writes. Without it the call reads and reports, and a dry run is the
	// default at the command too: the first thing an operator wants to know about
	// a repair over a table they did not build is how much of it there is.
	Apply bool
	// FixUpdatedAt also restores each live memory's updated_at. It is a separate
	// flag from Apply because it is a separate repair — the redundant versions can
	// be removed on their own, and a caller who wants the history back under its
	// caps without a stamp moving does not have to accept a stamp moving.
	FixUpdatedAt bool
}

// HistoryCompactResult is what one project's compaction found and did. The counts
// are the same numbers in a dry run and in an apply, which is what makes the dry
// run a preview rather than an estimate; see the fixed-point note on CompactHistory.
type HistoryCompactResult struct {
	// ProjectID is the project the counts are for.
	ProjectID string `json:"project_id"`
	// Removed is the number of redundant versions deleted, or — in a dry run — the
	// number the apply would delete.
	Removed int64 `json:"removed"`
	// UpdatedAt is the number of live memories whose updated_at was restored, or
	// would be.
	UpdatedAt int64 `json:"updated_at_restored"`
	// StampsUnreadable is the number of memories whose stamp could not be restored
	// because one of the two timestamps no layout in StampLayouts reads. Reported
	// rather than silently skipped: a row this run could not repair is a row whose
	// supersede orientation is still wrong, and a count of zero fixes would
	// otherwise read as "nothing left to do".
	StampsUnreadable int64 `json:"stamps_unreadable"`
}

// CompactHistory repairs one project's recorded history: it removes the version
// rows that changed nothing, and with FixUpdatedAt it restores each live memory's
// updated_at to the recorded_at of the last version that did change something.
//
// The scope is one project, and the per-project count is what the result is for: a
// store's history is a whole-corpus audit, so "how much of it was noise" has a
// different answer in every project and the report is per project. An empty
// projectID is refused rather than read as "all projects": the caller iterating
// projects is the caller that knows which exist (Store.ListProjects), and a
// whole-store compaction is not something a caller should get by leaving a field
// empty.
//
// #redundantRowsAreAFixedPoint
//
// The delete is safe to run in batches, and one batch's deletions cannot change
// which later rows are removable. A removable row R records the same state as its
// predecessor P; the row after R compares against R, and once R is gone it compares
// against P — the same state, and so the same answer. The newest-version guard does
// not break that: R is never a memory's newest row, so removing it cannot promote
// a different row into being newest. The set of removable rows is therefore a fixed
// point of the deletion, which is what lets the apply loop until a batch comes back
// short and lets the dry run answer with a single count and be exact rather than a
// sample.
func (s *Store) CompactHistory(ctx context.Context, projectID string, opts HistoryCompactOptions) (HistoryCompactResult, error) {
	if projectID == "" {
		return HistoryCompactResult{}, fmt.Errorf("compact history: a project is required")
	}
	res := HistoryCompactResult{ProjectID: projectID}

	if !opts.Apply {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.compactHistoryPreview(ctx, projectID, opts, res)
	}

	// The write lock, for the reason every other writer here takes it: the batches
	// below are BEGIN IMMEDIATE, and a reader admitted between them would read a
	// history that is half compacted.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The updated_at pass runs FIRST, and the order is load-bearing rather than
	// cosmetic. Its decision asks whether a version that changed nothing sits NEWER
	// than the last real change (restorableStamp), so it has to see that version —
	// and the delete below is what removes it. Running the delete first would leave
	// the pass with no evidence at all and it would restore nothing, while a dry run
	// that deleted nothing would restore something: a preview and the run it
	// previews, disagreeing. The delete's own predicate does not depend on the pass,
	// which only ever writes memories.updated_at.
	if opts.FixUpdatedAt {
		fixed, unreadable, err := s.restoreUpdatedAt(ctx, projectID, true)
		if err != nil {
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable = fixed, unreadable
	}
	removed, err := s.deleteRemovableHistory(ctx, projectID)
	if err != nil {
		return res, err
	}
	res.Removed = removed
	return res, nil
}

// compactHistoryPreview answers a dry run without opening a transaction: nothing is
// written, so there is no write lock to take. Both counts come from the same SQL and
// the same decision the apply makes, in the same ORDER, off the same table — a
// preview and the run it previews cannot disagree, including about a row whose clock
// is ambiguous.
func (s *Store) compactHistoryPreview(ctx context.Context, projectID string, opts HistoryCompactOptions,
	res HistoryCompactResult) (HistoryCompactResult, error) {
	if opts.FixUpdatedAt {
		fixed, unreadable, err := s.restoreUpdatedAt(ctx, projectID, false)
		if err != nil {
			return res, err
		}
		res.UpdatedAt, res.StampsUnreadable = fixed, unreadable
	}
	query, args := compactCountStmt(projectID)
	var removed int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&removed); err != nil {
		return res, fmt.Errorf("count removable history versions: %w", err)
	}
	res.Removed = removed
	return res, nil
}

// deleteRemovableHistory removes this project's removable versions in bounded
// BEGIN IMMEDIATE batches, and reports how many went.
func (s *Store) deleteRemovableHistory(ctx context.Context, projectID string) (int64, error) {
	query, args := compactDeleteStmt(projectID)
	var total int64
	for {
		// beginWrite, not s.db.BeginTx: this opens a write transaction per batch
		// and a whole pass can be dozens of them, which is the shape #671 measured
		// — a writer that loses its hand-off on SQLite's growing busy-handler
		// schedule — so the bounded retry rides on the batches too, and the wait
		// and hold are reported under their own op name rather than folded into a
		// writer's.
		tx, lock, err := s.beginWrite(ctx, "history-compact")
		if err != nil {
			return total, fmt.Errorf("begin compact history: %w", err)
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("delete removable history versions: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck
			return total, fmt.Errorf("count deleted history versions: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("commit compact history: %w", err)
		}
		// The lock is free from here, so this is where the hold ends.
		lock.reportHold("history-compact", time.Now())
		total += n
		// A batch shorter than the limit means the project held no more removable
		// rows. Not an empty one: a full final batch is the ordinary shape when the
		// count divides the limit exactly, and stopping there would leave that last
		// batch for a run that had nothing else to do.
		if n < int64(historyCompactBatchSize) {
			return total, nil
		}
	}
}

// compactStamp is one live memory's candidate restore: the last version that
// changed its state (target, recorded_at), the freshness stamp the row holds now,
// and the newest rowid of this memory's history the compaction can remove.
type compactStamp struct {
	memoryID      string
	target        int64
	recorded      string
	updatedAt     string
	removableLast int64
}

// restorableStamp decides one memory's restored updated_at, and is the ONLY place
// that decision is made: the dry run's count and the apply's write both come
// through it, so a preview and the run it previews cannot disagree.
//
// The first gate is the one that keeps the repair from undoing a change the store
// made on purpose, and it is a single condition: a REMOVABLE version must be newer
// than the last version that changed state. That is the whole claim the repair
// makes — the reflect run that moved the stamp is the one whose versions said
// nothing, and it said nothing AFTER the last real change — and it covers the case
// that has no state-column answer.
//
// ReplaceNonManual's reusePreservesAge branch (#623) re-tags or re-scopes a reused
// row, and the version it appends restates content, category, importance,
// resolved_at and source byte for byte, because this table has no column for tags
// or scope. #727 was right that the row is a real change and recorded on purpose.
// The newest-version guard (historyRemovableRowSQL) keeps that row, so nothing is
// removable for a memory whose only restatement is that one, and this gate leaves
// the bump it made alone. Without the guard the compaction would delete the row and
// this would move the stamp back over a change the store deliberately made and
// recorded — the exact failure #730 is a repair for, reproduced by the repair.
//
// Then the direction. The damage moved updated_at FORWARD, to the time of a reflect
// that changed nothing, so the repair moves it BACK to the last real change and only
// ever back. A stamp already at or before that target is not damage: it may be a
// writer that moved it without filing a history row, a clock that ran ahead, or a
// row some other tool edited. Moving it FORWARD to meet a history that has stopped
// recording would invent a freshness Ghost never recorded, which is the failure this
// repair exists to undo.
//
// A stamp no layout in StampLayouts reads is reported rather than guessed at, on
// either side: a target Ghost cannot interpret is not a target, and an unreadable
// current value is not evidence that the move would be backward.
//
// The stamp is written in StoredStampLayout rather than in the shape it was read
// in, because the two columns are written by different statements: a whole-day
// value on either side is legal to read and illegal to write back, and copying the
// shape read would leave a row in a form no writer produces — which is the form a
// reader has to try twice to parse.
func restorableStamp(s compactStamp) (stamp string, restorable, unreadable bool) {
	if s.removableLast == 0 || s.removableLast <= s.target {
		return "", false, false
	}
	target, ok := ParseStamp(s.recorded)
	if !ok {
		return "", false, true
	}
	current, ok := ParseStamp(s.updatedAt)
	if !ok {
		return "", false, true
	}
	if !current.After(target) {
		return "", false, false
	}
	return target.UTC().Format(StoredStampLayout), true, false
}

// restoreUpdatedAt walks every live memory of the project that has a version which
// changed state, in bounded batches, and reports how many were restored and how many
// could not be read.
//
// The write lock is the caller's, and each batch takes the database's own write lock
// in its own BEGIN IMMEDIATE, so the lock is taken historyCompactBatchSize times over
// the pass rather than held for it. apply is the only difference between the two
// modes: a dry run reads on the pool and opens no transaction at all, because there
// is nothing to write and a transaction here would take the write lock to look at a
// table nobody asked it to change.
func (s *Store) restoreUpdatedAt(ctx context.Context, projectID string, apply bool) (fixed, unreadable int64, err error) {
	cursor := ""
	for {
		// tx is nil in a dry run and this batch's transaction in an apply, and it is
		// the ONLY difference between the two modes below: one read, one decision,
		// and a write skipped when there is no transaction to write in. Three copies
		// of that loop would be three answers.
		var (
			tx   *sql.Tx
			lock writeLock
		)
		if apply {
			// beginWrite for the reason deleteRemovableHistory gives: a pass opens
			// a transaction per batch, and #671's bounded retry is what keeps a
			// writer from losing its hand-off on the busy handler's growing poll
			// schedule.
			if tx, lock, err = s.beginWrite(ctx, "history-compact-stamp"); err != nil {
				return fixed, unreadable, fmt.Errorf("begin restore updated_at: %w", err)
			}
		}
		var q stampQuerier = s.db
		if tx != nil {
			q = tx
		}
		batch, readErr := s.stampBatchQuery(ctx, q, projectID, cursor)
		if readErr != nil {
			if tx != nil {
				_ = tx.Rollback() //nolint:errcheck
			}
			return fixed, unreadable, readErr
		}
		for _, c := range batch {
			stamp, restorable, bad := restorableStamp(c)
			if bad {
				unreadable++
			}
			if !restorable {
				continue
			}
			fixed++
			if tx == nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, historyCompactRestoreSQL, stamp, c.memoryID, projectID); err != nil {
				_ = tx.Rollback() //nolint:errcheck
				return fixed, unreadable, fmt.Errorf("restore updated_at of %s: %w", c.memoryID, err)
			}
		}
		if tx != nil {
			if err := tx.Commit(); err != nil {
				return fixed, unreadable, fmt.Errorf("commit restore updated_at: %w", err)
			}
			lock.reportHold("history-compact-stamp", time.Now())
		}
		if len(batch) < historyCompactBatchSize {
			return fixed, unreadable, nil
		}
		cursor = batch[len(batch)-1].memoryID
	}
}

// stampBatchQuery is the one shape both modes read: the candidate statement, scoped
// and paged. An apply runs it on the transaction it will write in, so a memory that
// is deleted or moved between the read and the write cannot be stamped from a row
// the run has already read.
func (s *Store) stampBatchQuery(ctx context.Context, q stampQuerier, projectID, cursor string) ([]compactStamp, error) {
	query, args := compactCandidatesStmt(projectID, cursor)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read memories to restore updated_at: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var batch []compactStamp
	for rows.Next() {
		var c compactStamp
		if err := rows.Scan(&c.memoryID, &c.target, &c.recorded, &c.updatedAt, &c.removableLast); err != nil {
			return nil, fmt.Errorf("scan memory to restore updated_at: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories to restore updated_at: %w", err)
	}
	return batch, nil
}

// stampQuerier is the pool and a transaction, so one read serves both modes.
type stampQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
