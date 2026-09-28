package memory

import (
	"context"
	"errors"
	"fmt"
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
//   - The dry run is the default, and it is the SAME transaction as the apply —
//     rolled back instead of committed. A preview built from a different query
//     than the one that deletes is a preview of something else.
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

// pruneBeforeDelete is the seam TestPruneIsOneTransaction uses to fail a prune
// between its tombstones and its DELETE. It is a var, and it is nil in
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
// Candidates is populated on both paths — it is the SELECT, and the apply's
// DELETE re-derives the same set — so a dry run's list and an apply's list are
// the same list by construction rather than by agreement.
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

// prunePredicate is the candidate predicate, and the DELETE runs the identical
// text. That is not tidiness: the tombstones are appended for the ids the SELECT
// returned, so the two statements have to name exactly the same rows. One
// transaction has held the write lock since before the SELECT (the DSN asks for
// BEGIN IMMEDIATE), so they cannot disagree by a row arriving in between — which
// is the reason to compare them by construction instead of by argument.
//
// The activity term is COALESCE(last_accessed, updated_at, created_at): a
// recorded access is the stronger signal, and nothing in production writes
// last_accessed (Store.Touch has no caller), so in practice this is the row's
// last WRITE. It is compared through SQLite's datetime() rather than as text,
// because the three columns do not all hold the same shape: created_at and
// updated_at are whatever datetime('now') wrote, and Store.Touch writes
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
const prunePredicate = `
		retention = '` + RetentionSession + `'
		AND expires_at IS NOT NULL
		AND expires_at <= ?
		AND datetime(COALESCE(last_accessed, updated_at, created_at)) <= datetime(?)
`

// PruneSessionMemories removes expired session-tier memories that nothing has
// touched for the grace period, and appends a delete tombstone for each.
//
// It is one transaction from the SELECT to the COMMIT — or to the ROLLBACK that a
// dry run takes instead. The dry run is the same statement sequence rather than a
// read-only preview, so the report cannot describe rows the apply would not have
// selected, and a rollback guarantees the preview wrote nothing at all.
//
// The lock is s.mu for the whole body, and the DSN asks for BEGIN IMMEDIATE, so
// a concurrent save either happens entirely before this run's SELECT or entirely
// after its COMMIT. A save that lands in between is not a lost memory: it is a
// row whose expiry the prune never saw, and it is picked up by the next run.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		SELECT id, project_id, category, content, retention, expires_at,
		       COALESCE(last_accessed, updated_at, created_at)
		FROM memories
		WHERE ` + prunePredicate
	args := []any{stampForCompare(now), stampForCompare(now.Add(-grace))}
	if opts.ProjectID != "" {
		query += "\n\t\t  AND project_id = ?"
		args = append(args, opts.ProjectID)
	}
	// Oldest activity first, id to break a tie: a prune that runs over a large
	// corpus should take the row nobody has looked at in the longest time, and
	// two rows with the same stamp have to come out in an order a second run
	// would repeat.
	query += "\n\t\tORDER BY COALESCE(last_accessed, updated_at, created_at), id"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return report, fmt.Errorf("find prunable memories: %w", err)
	}
	for rows.Next() {
		var c PruneCandidate
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Category, &c.Content, &c.Retention, &c.ExpiresAt, &c.ActivityAt); err != nil {
			rows.Close() //nolint:errcheck
			return report, fmt.Errorf("scan prunable memory: %w", err)
		}
		report.Candidates = append(report.Candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return report, fmt.Errorf("iterate prunable memories: %w", err)
	}
	if err := rows.Close(); err != nil {
		return report, fmt.Errorf("close prunable memories: %w", err)
	}

	if !opts.Apply || len(report.Candidates) == 0 {
		// The rollback above is the whole of a dry run. There is no branch here
		// that could have written: the only statements that ran are the BEGIN and
		// the SELECT.
		return report, nil
	}

	for _, c := range report.Candidates {
		report.RemovedIDs = append(report.RemovedIDs, c.ID)
	}

	// The tombstone first, from the state the row holds right now — memory_id
	// carries no foreign key, so this row is the only thing that will still know
	// the text once the DELETE lands.
	if err := appendHistoryForIDsTx(ctx, tx, report.RemovedIDs, phaseDelete, Provenance{}); err != nil {
		return report, err
	}

	// The seam, AFTER the audit and BEFORE the delete: that is the only position
	// from which the rollback is under test. Placed before the append it would
	// prove only that an error returned, which any implementation does; here a
	// failure leaves written history rows that the transaction has to undo, so
	// the test fails if the append is not in the same transaction as the delete.
	// See pruneBeforeDelete.
	if pruneBeforeDelete != nil {
		if err := pruneBeforeDelete(); err != nil {
			return report, err
		}
	}

	del := `
		DELETE FROM memories
		WHERE ` + prunePredicate
	delArgs := args
	if opts.ProjectID != "" {
		del += "\n\t\t  AND project_id = ?"
	}
	res, err := tx.ExecContext(ctx, del, delArgs...)
	if err != nil {
		return report, fmt.Errorf("remove pruned memories: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return report, fmt.Errorf("count pruned memories: %w", err)
	}
	if int(removed) != len(report.RemovedIDs) {
		// The SELECT and the DELETE name the same rows in the same transaction,
		// so a difference here is not a race — it is a broken invariant, and it
		// must not be reported as a successful prune of a set nobody can name.
		return report, fmt.Errorf("prune removed %d rows but recorded %d tombstones: the delete and the audit disagree", removed, len(report.RemovedIDs))
	}
	report.Removed = int(removed)

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit prune: %w", err)
	}
	return report, nil
}

// stampForCompare renders an instant the way the timestamp columns store one, so
// a bound parameter and a stored value compare as the strings they are. Passing a
// time.Time would bind RFC 3339 with an offset, which does not order against
// 'YYYY-MM-DD HH:MM:SS' the way a reader expects.
func stampForCompare(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }
