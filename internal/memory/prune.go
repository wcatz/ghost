package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Pruning removes what the session tier promised it would: a conversation-scoped
// memory, after it has expired, after nobody has touched it for a grace period,
// and only when somebody asked for it by name.
//
// Four things this file is shaped around, all of them the reason it is a
// separate command rather than a step in a lifecycle pass:
//
//   - It is never automatic. Nothing in Ghost runs this on a timer, on a hook, or
//     at the end of a reflect; a user who has not asked has not asked. The
//     delete is the most destructive write in the product, and the default that
//     erases a user's own memories is the one failure mode the whole tier design
//     is arranged to avoid.
//   - The dry run is the default, and the preview is the SAME query the apply
//     re-runs against each batch — same predicate, same scope, same order — so a
//     preview built from a different query than the one that deletes is a
//     preview of something else. The apply itself deletes in bounded batches
//     (pruneBatchSize rows per transaction), because one unbounded BEGIN
//     IMMEDIATE held the store's single connection — and with it every
//     concurrent save — open for an entire backlog.
//   - Every removal appends a memory_history tombstone in that same transaction,
//     so the store still knows what it removed, when, and which agent's project
//     it belonged to.
//   - Only session rows are candidates. `retention = 'session'` is in the
//     predicate rather than in Go, so the tier is decided by the same statement
//     that reads the row.

// DefaultPruneGrace is DefaultSessionGrace, spelled as its own name so the
// command's default and the tier's documented margin cannot be read as two
// different numbers.
const DefaultPruneGrace = DefaultSessionGrace

// errPruneInterrupted is what the test seam reports, so the rollback path has an
// error to roll back from.
var errPruneInterrupted = errors.New("prune: interrupted before the delete")

// pruneBeforeDelete is the seam the batch-rollback tests use to fail a prune
// between its tombstones and its DELETE — once per batch, so a test can let the
// first batch commit and interrupt the second. It is a var, and it is nil in
// production, for the reason the hydration hook above it is: a failure that
// depends on timing is not something a test can cause on purpose, so the step
// itself is made reachable and the test drives it.
var pruneBeforeDelete func() error

// setPruneBeforeDelete installs the seam and returns the function that puts it
// back, so a test cannot leak it into the next one.
func setPruneBeforeDelete(fn func() error) (restore func()) {
	prev := pruneBeforeDelete
	pruneBeforeDelete = fn
	return func() { pruneBeforeDelete = prev }
}

// PruneOptions is what one prune run is doing. The zero value is a dry run over
// every project with the documented grace, which is the shape a caller gets for
// asking nothing.
type PruneOptions struct {
	// Apply writes. False is a dry run: the same transaction, the same
	// predicate, the same report — rolled back.
	Apply bool
	// Grace is how long past its expiry an untouched session row is left alone.
	// Zero is DefaultSessionGrace. A negative value is refused: it would mean
	// "expired regardless of when it was last touched", which is not a grace
	// period, and silently reading it as zero is how that ask would be granted
	// by accident.
	Grace time.Duration
	// Now is the instant the run measures against. Zero is the wall clock. It is
	// a field rather than a clock read because the boundary test needs to sit on
	// both sides of a threshold at a known instant, and because a caller that
	// computed a report for one moment should not be reviewing rows judged for
	// another.
	Now time.Time
	// ProjectID scopes the run to one project. Empty is every project, which is
	// the honest default for a store-wide maintenance pass and is reported as
	// such — a count beside a project name has to mean that project.
	ProjectID string
}

// PruneCandidate is one row a prune would remove, with everything the report
// needs to be checkable by a reader: which tier it is, when it expires, and when
// anything last touched it. The text is included because a report of rows the
// operator cannot recognise is a report they cannot approve.
type PruneCandidate struct {
	ID         string
	ProjectID  string
	Category   string
	Content    string
	Retention  string
	ExpiresAt  string
	ActivityAt string
}

// PruneReport is what one run did, or would do.
//
// Candidates is populated on both paths — it is the preview read, and every
// apply batch re-derives the same predicate, scope, and order — so a dry run's
// list and an apply's list are the same list by construction rather than by
// agreement, modulo rows that the store itself changed between the preview and a
// batch (a promoted or pinned row simply stops matching).
type PruneReport struct {
	// Applied says whether this run committed. A report that says so is the only
	// thing that distinguishes the two shapes, and a caller that printed the rest
	// of this struct without it would describe a preview as an outcome.
	Applied bool
	// Grace and Now are the run's own parameters, echoed so a saved report says
	// what it measured rather than leaving the reader to assume the defaults.
	Grace time.Duration
	Now   time.Time
	// ProjectID is the scope, empty for every project.
	ProjectID string
	// Candidates is every row the run selected, in removal order.
	Candidates []PruneCandidate
	// RemovedIDs and Removed are the same fact, and both are carried because a
	// caller needs the ids to follow the tombstones and a reader needs the count
	// at a glance; neither is derived from the other here, so they cannot drift.
	RemovedIDs []string
	Removed    int
}

// pruneActivitySQL is the row's last activity, as ONE expression, because three
// statements have to agree on it: the predicate below, the candidate's own
// ActivityAt, and the removal order. A grace measured from one expression and
// ordered by another is a report that describes a different run from the one it
// previews.
//
// A recorded access is the strongest signal and is preferred when it exists.
// Nothing in production writes last_accessed (Store.Touch has no caller), so
// what is left is the newest of the row's own stamps: the last write, or the
// expiry a write REFRESHED. max() rather than another COALESCE term because
// created_at is NOT NULL, so a trailing term is unreachable — and the newest of
// the three is the question, not the first one present. The expiry belongs there
// because a fold is a write that extends a session row's life (raiseRetentionTx)
// while deliberately leaving updated_at alone, and without this term a row
// reinforced a moment ago was prunable the instant its fresh expiry arrived: the
// grace was measured from a stamp the fold never moved (#772). Every term but
// last_accessed is in the one layout Ghost writes, so the max is a text
// comparison between like shapes; a hand-set value in another shape sorts by its
// own characters, which errs towards the newer stamp and so towards keeping the
// row.
const pruneActivitySQL = "COALESCE(last_accessed, max(updated_at, created_at, expires_at))"

// prunePredicate is the candidate predicate, and the DELETE runs the identical
// text. That is not tidiness: the tombstones are appended for the ids the SELECT
// returned, so the two statements have to name exactly the same rows. One
// transaction has held the write lock since before the SELECT (the DSN asks for
// BEGIN IMMEDIATE), so they cannot disagree by a row arriving in between — which
// is the reason to compare them by construction instead of by argument.
//
// The activity term is pruneActivitySQL above, compared through SQLite's
// datetime() rather than as text, because the columns it coalesces do not all
// hold the same shape: created_at, updated_at and expires_at are whatever
// datetime('now') wrote and sessionExpiry formats, while Store.Touch writes
// last_accessed as RFC 3339. A text comparison between the two errs only WITHIN
// one calendar day ('T' sorts above ' '), and it errs towards keeping the row — a
// prune that runs a day late, never one that removes a memory it should not — but
// a guarantee that holds only while a column has no writer is not a guarantee.
// datetime() reads both shapes, and a value it cannot read yields NULL, which
// compares false and so leaves the row in the store: the same safe direction.
//
// expires_at is compared as TEXT rather than through datetime() because it is the
// one term an index can serve (idx_memories_session_expiry is a partial index on
// expires_at WHERE retention = 'session'), and Ghost is its only writer — the
// derivation in sessionExpiry, in the one shape every other timestamp column uses.
// A value set by hand in another shape does not match, which leaves the row in the
// store rather than taking it out.
//
// The last_accessed preference is a KNOWN limit rather than a settled reading: a
// row holding an OLD non-NULL last_accessed shadows a refreshed expires_at
// entirely, so a fold's renewal would not reach the grace for that row. It is
// dormant — nothing in production writes the column (Store.Touch has no caller) —
// and it is stated here rather than left to be found by the first surface that
// records a read.
//
// pinned = 0 is the fifth term, and it sits in the predicate rather than in Go
// for the same reason the tier does: a pin is decided by the same statement that
// reads the row. A pin is an explicit user override, and the session promise was
// made for rows the user did NOT override; a pinned session row that happens to
// have an expired past is a row being kept, not garbage. Re-checking it at the
// DELETE is not paranoid — the terms that decide the DELETE are the terms that
// decide the SELECT, and their absence is how a row nobody has looked at in a
// long time looks exactly like a row about to go.
const prunePredicate = `
		retention = '` + RetentionSession + `'
		AND pinned = 0
		AND expires_at IS NOT NULL
		AND expires_at <= ?
		AND datetime(` + pruneActivitySQL + `) <= datetime(?)
`

// pruneBatchSize bounds the write lock: an apply removes candidates in batches
// of this many rows, each batch in its own BEGIN IMMEDIATE transaction with its
// own tombstones, so a prune backlog can never hold the store's write lock open
// for the whole cleanup. The batch is selected by the same predicate, in the
// same order, so it is the same window a dry run previewed.
const pruneBatchSize = 500

// PruneSessionMemories removes expired session-tier memories that nothing has
// touched for the grace period, and appends a delete tombstone for each.
//
// The preview is one read of the full candidate set — the same predicate, scope,
// and order every batch later re-derives. An apply then removes the candidates
// in batches of pruneBatchSize: each batch is selected under the write lock (the
// DSN asks for BEGIN IMMEDIATE), audited with tombstones, and deleted in one
// transaction, so every batch is internally consistent — a row cannot change
// between the SELECT that names it and the DELETE that removes it — and a batch
// that fails rolls back whole, leaving the pre-batch store state intact.
//
// Between batches the lock is dropped, so a concurrent save either happens
// entirely before a batch's SELECT or entirely after its COMMIT. A save that
// lands in between is not a lost memory: it is a row whose expiry the prune
// never saw, and it is picked up by the next run. The report carries what
// actually committed — an apply that failed partway reports the batches that
// landed and the error — never a prediction of a clean run.
func (s *Store) PruneSessionMemories(ctx context.Context, opts PruneOptions) (PruneReport, error) {
	if opts.Grace < 0 {
		return PruneReport{}, fmt.Errorf("invalid grace period %s: it must be zero (the %s default) or a positive duration",
			opts.Grace, DefaultPruneGrace)
	}
	grace := opts.Grace
	if grace == 0 {
		grace = DefaultPruneGrace
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	report := PruneReport{Applied: opts.Apply, Grace: grace, Now: now, ProjectID: opts.ProjectID}

	predArgs := []any{stampForCompare(now), stampForCompare(now.Add(-grace))}
	scope := ""
	if opts.ProjectID != "" {
		scope = "\n\t\t  AND project_id = ?"
		predArgs = append(predArgs, opts.ProjectID)
	}
	// Oldest activity first, id to break a tie: a prune that runs over a large
	// corpus should take the row nobody has looked at in the longest time, and
	// two rows with the same stamp have to come out in an order a second run
	// would repeat. The order is part of the query so a batch is the same window
	// the preview named.
	order := "\n\t\tORDER BY " + pruneActivitySQL + ", id"

	candQuery := `
		SELECT id, project_id, category, content, retention, expires_at,
		       ` + pruneActivitySQL + `
		FROM memories
		WHERE ` + prunePredicate + scope + order

	// The preview is a plain read, and it shares the predicate, scope, and order
	// with every batch the apply runs — never a second query of its own.
	s.mu.RLock()
	rows, err := s.db.QueryContext(ctx, candQuery, predArgs...)
	if err != nil {
		s.mu.RUnlock()
		return report, fmt.Errorf("find prunable memories: %w", err)
	}
	for rows.Next() {
		var c PruneCandidate
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Category, &c.Content, &c.Retention, &c.ExpiresAt, &c.ActivityAt); err != nil {
			rows.Close() //nolint:errcheck
			s.mu.RUnlock()
			return report, fmt.Errorf("scan prunable memory: %w", err)
		}
		report.Candidates = append(report.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		s.mu.RUnlock()
		return report, fmt.Errorf("iterate prunable memories: %w", err)
	}
	if err := rows.Close(); err != nil {
		s.mu.RUnlock()
		return report, fmt.Errorf("close prunable memories: %w", err)
	}
	s.mu.RUnlock()

	if !opts.Apply || len(report.Candidates) == 0 {
		// The whole of a dry run is the read above. There is no other branch
		// here that could have written.
		return report, nil
	}

	batchQuery := `
		SELECT id
		FROM memories
		WHERE ` + prunePredicate + scope + order + "\n\t\tLIMIT ?"

	for {
		s.mu.Lock()
		// The guarded seam, not a bare BeginTx: prune is a bulk delete, so a
		// server running behind a newer Ghost must refuse it the same way every
		// other write here is refused, and the check has to run in the SAME
		// transaction as the delete it guards. The seam owns BEGIN and the
		// contention retry, and rolls the transaction back on a refusal — which
		// is what keeps a refused prune from holding the write lock while the
		// newer binary runs its migration.
		tx, lock, err := s.beginGuardedWrite(ctx, "prune batch")
		if err != nil {
			s.mu.Unlock()
			return report, fmt.Errorf("begin prune batch: %w", err)
		}

		// The batch, decided under the write lock: whatever the predicate names
		// at THIS instant, in removal order, capped at one batch. A row promoted
		// to pinned or persistent by a save that landed since the preview simply
		// stops matching and is not in the batch.
		batchArgs := append(append([]any{}, predArgs...), pruneBatchSize)
		idRows, err := tx.QueryContext(ctx, batchQuery, batchArgs...)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("find prune batch: %w", err)
		}
		ids := make([]string, 0, pruneBatchSize)
		for idRows.Next() {
			var id string
			if err := idRows.Scan(&id); err != nil {
				idRows.Close() //nolint:errcheck
				tx.Rollback()  //nolint:errcheck
				s.mu.Unlock()
				return report, fmt.Errorf("scan prune batch: %w", err)
			}
			ids = append(ids, id)
		}
		if err := idRows.Err(); err != nil {
			idRows.Close() //nolint:errcheck
			tx.Rollback()  //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("iterate prune batch: %w", err)
		}
		if err := idRows.Close(); err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("close prune batch: %w", err)
		}
		if len(ids) == 0 {
			if err := tx.Commit(); err != nil {
				s.mu.Unlock()
				return report, fmt.Errorf("commit empty prune batch: %w", err)
			}
			s.mu.Unlock()
			// No hold sample: this transaction wrote nothing, and the
			// distribution reportHold feeds describes writes.
			break
		}

		// The tombstone first, from the state the row holds right now — memory_id
		// carries no foreign key, so this row is the only thing that will still know
		// the text once the DELETE lands.
		if err := appendHistoryForIDsTx(ctx, tx, ids, phaseDelete, Provenance{}); err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, err
		}

		// The seam, AFTER the audit and BEFORE the delete: that is the only position
		// from which the batch rollback is under test. Placed before the append it
		// would prove only that an error returned, which any implementation does;
		// here a failure leaves written history rows that the transaction has to
		// undo, so the test fails if the append is not in the same transaction as
		// the delete. See pruneBeforeDelete.
		if pruneBeforeDelete != nil {
			if err := pruneBeforeDelete(); err != nil {
				tx.Rollback() //nolint:errcheck
				s.mu.Unlock()
				return report, err
			}
		}

		placeholders := make([]string, len(ids))
		delArgs := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			delArgs[i] = id
		}
		del := `DELETE FROM memories WHERE id IN (` + strings.Join(placeholders, ",") + `)`
		res, err := tx.ExecContext(ctx, del, delArgs...)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("remove pruned memories: %w", err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("count pruned memories: %w", err)
		}
		if int(removed) != len(ids) {
			// The batch SELECT and the DELETE run in the same transaction, so a
			// difference here is not a race — it is a broken invariant, and it
			// must not be reported as a successful prune of a set nobody can name.
			tx.Rollback() //nolint:errcheck
			s.mu.Unlock()
			return report, fmt.Errorf("prune removed %d rows but recorded %d tombstones in one batch: the delete and the audit disagree", removed, len(ids))
		}

		if err := tx.Commit(); err != nil {
			s.mu.Unlock()
			return report, fmt.Errorf("commit prune batch: %w", err)
		}
		s.mu.Unlock()
		lock.reportHold("prune batch", time.Now())

		report.RemovedIDs = append(report.RemovedIDs, ids...)
		report.Removed += int(removed)
	}
	return report, nil
}

// stampForCompare renders an instant the way the timestamp columns store one, so
// a bound parameter and a stored value compare as the strings they are. Passing a
// time.Time would bind RFC 3339 with an offset, which does not order against
// 'YYYY-MM-DD HH:MM:SS' the way a reader expects.
func stampForCompare(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }
