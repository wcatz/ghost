package memory

// The write-side refusal for a store a NEWER Ghost owns (issue #746).
//
// OpenDB already refuses such a store, and that refusal is correct and useless
// for the case the issue reports: a `ghost mcp` server that is ALREADY RUNNING
// checked its version once, hours before a newer binary migrated the file
// underneath it. On a real install three v0.33.0 servers kept writing for ~40
// hours after the store went to v17 and then v18 — 32 saves with no history row,
// an edit that changed text and recorded no version, deletes that left no
// tombstone, and no evidence record anywhere. Nothing was lost; the audit trail
// was, silently, and every "same transaction" guarantee in the memory-history and
// evidence-provenance invariants was void for those writes.
//
// So the answer is not a better open-time check. It is a check on the way IN to
// every write, which is the only place a long-lived server passes through often
// enough to matter — and it takes two seams, because eighteen of those writes
// never opened a transaction at all and a guard that only watches transactions
// would miss every one of them.
//
// There is a third shape, and it is the reason the two seams are not enough on
// their own terms. A `RETURNING` clause is a WRITE that arrives through
// `QueryRowContext`, so a classifier reading method names alone routes it as a
// read and it reaches SQLite with no check at all — which is how `CreateTask`
// (behind the `ghost_task_create` tool) and `IncrementInteraction` both wrote
// into a store a newer Ghost owned, with this file's guard in place and
// everything looking correct. Both are transactions now, because a write that
// takes a transaction is the shape the check was built for.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StoreNewerError is the refusal a write gets when the store on disk is at a
// schema version NEWER than this build understands. It is a struct rather than a
// bare sentinel because the operator's next action depends on both numbers —
// which side moved, and by how much — and a message that lost them would send
// someone to `ghost upgrade` when the binary on disk is already current.
//
// Match it with errors.Is(err, ErrStoreNewer), which carries through the store's
// own wrapping exactly as ErrSecretContent does: every store method wraps with
// %w, so the type survives to the MCP tool and to the CLI. A caller that wants
// the two numbers rather than the fact unwraps it; ghost_health reads them
// through StoreVersionStatus instead, which answers without needing an error.
type StoreNewerError struct {
	// StoreVersion is the PRAGMA user_version the file carried when the write
	// was refused.
	StoreVersion int
	// BuildVersion is this binary's SchemaVersion — the newest store it can
	// write without corrupting state it cannot interpret.
	BuildVersion int
}

// Is lets errors.Is match a *StoreNewerError against a bare ErrStoreNewer
// sentinel, so a caller that only needs to know WHICH refusal this was can
// write errors.Is(err, memory.ErrStoreNewer) without holding the struct. The
// struct is still the error, so the two versions survive for a caller that
// wants to print them.
func (e *StoreNewerError) Is(target error) bool {
	return target == errStoreNewerSentinel
}

// errStoreNewerSentinel is the comparable value behind the Is method above.
var errStoreNewerSentinel = &StoreNewerError{}

// ErrStoreNewer is the sentinel for errors.Is: it matches any store-newer
// refusal, and carries no versions of its own. Unwrap it when the two numbers
// matter; ghost_health prints them.
var ErrStoreNewer error = errStoreNewerSentinel

// Error names both versions and the one remedy that fits a RUNNING server. The
// struct is the error, so the numbers are available to a caller that unwraps it
// rather than parsed back out of this string.
func (e *StoreNewerError) Error() string {
	// The remedy is deliberately NOT the one OpenDB's refusal gives. "Upgrade
	// ghost" is right when the open fails and wrong here: the binary on disk
	// may ALREADY be the newer one, and the stale half is the running server.
	// A reader who is told to upgrade will run `ghost upgrade`, watch it report
	// "already up to date", and learn nothing. The running process is what has
	// to be replaced, and only the client that started it can do that.
	//
	// Both numbers are always present and always meaningful: SchemaVersion is a
	// schema number, not a release number, so there is no build for which the
	// comparison is undefined.
	return fmt.Sprintf("store schema v%d is newer than this ghost build (v%d) writes — restart the client that runs this ghost server so it picks up the newer binary; reads keep working until then",
		e.StoreVersion, e.BuildVersion)
}

// beginGuardedWrite opens a write transaction and refuses the write if the store
// is newer than this build. It is the ONE place a store write transaction may
// start, and TestEveryWriteRefusesANewerStore walks the package to keep it that
// way.
//
// The check runs AFTER BeginTx, inside the transaction, and that placement is
// what makes it race-free rather than merely likely. The DSN asks for BEGIN
// IMMEDIATE (see OpenDB), so this transaction already holds SQLite's write lock
// from the instant it starts. A migration is itself a write — it takes the same
// lock, runs DDL, and stamps user_version in its own transaction — so no
// migration can commit between this check and this transaction's writes: the
// window is not small, it is closed. Checking BEFORE BeginTx would leave exactly
// the gap the issue describes, where a migration lands in the microseconds
// between the read and the write and every subsequent write in that transaction
// is unaudited.
//
// The BEGIN itself is beginWrite's, unchanged: one extra attempt on lock
// contention only, never a loop, with the same Wait/Retried reporting. The
// measurement is deliberately kept intact — the contention distribution it
// exists for (#671) must not change because a safety check was added in front
// of it — and the refusal rolls the transaction back, so a refused write
// contributes no hold time and blocks nothing.
func (s *Store) beginGuardedWrite(ctx context.Context, op string) (*sql.Tx, writeLock, error) {
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		return s.finishGuardedWrite(ctx, op, tx, writeLock{wait: time.Since(start), took: time.Now()})
	}
	if !isLockContention(err) || ctx.Err() != nil {
		// ctx.Err() is asked as well as the classification: a caller that gave
		// up does not get a second budget spent finding out.
		return nil, writeLock{}, err
	}
	// The refusal is reported only once the outcome is known, because a
	// transaction that gets the lock on the second attempt has one wait (both
	// attempts) and one hold, and reporting the first refusal separately would
	// count it twice in the distribution it exists to describe.
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		lost := writeLock{wait: time.Since(start), retried: true}
		lost.reportLost(op)
		return nil, writeLock{}, err
	}
	// Wait spans BOTH attempts, which is what a writer's budget actually
	// covered. Reporting only the successful attempt's would make the fleet's
	// wait distribution read as if the contention were shorter than it is,
	// which is the number this seam exists to report.
	return s.finishGuardedWrite(ctx, op, tx, writeLock{wait: time.Since(start), took: time.Now(), retried: true})
}

// finishGuardedWrite runs the newer-store check on an open write transaction and
// returns it only if the write may proceed.
//
// The transaction is ROLLED BACK on a refusal rather than committed: a refusal
// that had written would be exactly the outcome the check exists to prevent.
// Rolling back also releases the write lock immediately, so a store refusing
// writes does not lock out the newer binary that is about to run its migration
// against it.
func (s *Store) finishGuardedWrite(ctx context.Context, op string, tx *sql.Tx, lock writeLock) (*sql.Tx, writeLock, error) {
	if err := s.checkStoreNotNewer(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, writeLock{}, err
	}
	return tx, lock, nil
}

// execGuardedWrite runs ONE statement in its own guarded write transaction.
//
// It exists because the check has to live in the SAME transaction as the write it
// guards, and a store has single-statement writes that legitimately want no
// transaction of their own — an autocommit UPSERT, a bookkeeping UPDATE. Wrapping
// one is not a behaviour change: SQLite already runs a lone statement in an
// implicit transaction, so this commits exactly what the autocommit committed, at
// the same granularity. That granularity is the property callers depend on, so it
// is preserved deliberately: the two statements in StoreEmbedding stay in TWO
// transactions and keep failing safe in both directions, and a sequence that
// relied on its first statement being durable before the second ran still gets
// that.
//
// op is the write-lock seam's name for this statement, so the measurement keeps
// describing every write the store performs rather than only the ones that
// already had a transaction.
func (s *Store) execGuardedWrite(ctx context.Context, op, query string, args ...any) (sql.Result, error) {
	tx, lock, err := s.beginGuardedWrite(ctx, op)
	if err != nil {
		return nil, err
	}
	res, execErr := tx.ExecContext(ctx, query, args...)
	if execErr != nil {
		// A failed statement rolls back rather than commits: a statement that
		// reported an error may have applied a trigger, and the autocommit this
		// replaces would not have committed either.
		_ = tx.Rollback()
		return nil, execErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	lock.reportHold(op, time.Now())
	return res, nil
}

// checkStoreNotNewer is the check itself, run inside an open write transaction.
//
// It is deliberately unable to answer "fine" when it cannot read. A probe that
// fails — a closed handle, a file that is not a database, a driver that cannot
// serve the pragma — leaves the store in a state this build cannot vouch for,
// and writing into an unvouched-for store is the outcome the check exists to
// prevent. A version that cannot be read is not a licence to write, and it is
// not a licence to claim the store is newer either: that claim sends an
// operator looking for a Ghost build that may not exist.
//
// It takes a Queryer rather than a *sql.Tx so the failure branch is reachable by
// a test at all. In production the obvious failures (a file that is not a
// database, a closed handle) are refused earlier, by BEGIN — the good outcome,
// which is exactly why it would otherwise leave this branch unpinned.
//
// It must read the version THROUGH THE TRANSACTION, never through the pool.
// That placement is structural rather than behavioural, so it is worth saying
// why, because the version that looks equivalent is wrong in two ways at once.
// A pre-BEGIN read leaves the gap this whole check exists to close: a migration
// can commit between the read and the write, and every write after it is
// unaudited. And on this pool it does not even race — it DEADLOCKS, because
// MaxOpenConns(1) means the open transaction is holding the only connection the
// probe would need (measured: the pre-BEGIN variant hangs rather than failing).
func (s *Store) checkStoreNotNewer(ctx context.Context, q Queryer) error {
	version, err := s.storeVersionInTx(ctx, q)
	if err != nil {
		return err
	}
	if version > SchemaVersion() {
		return &StoreNewerError{StoreVersion: version, BuildVersion: SchemaVersion()}
	}
	return nil
}

// storeVersionInTx returns the store's user_version, read fresh every time.
//
// It is read fresh, with NO cache, and that is a decision rather than an
// omission. A cache here is the obvious optimisation and it is unsound: the
// pragma that looks like a change counter, PRAGMA data_version, is a
// per-CONNECTION counter, not a property of the file, so on a connection the
// pool replaced underneath a long-lived server it can carry the same value the
// previous connection had (measured: two handles on one file both report 2, and
// a brand-new database also reports 2). A cache keyed on it would therefore
// answer "unchanged" for exactly the stale write it was meant to prevent, and
// the cost it saved was a memory read of a page the open transaction already
// holds.
//
// The read goes through the transaction, so it observes the state as of the
// snapshot BEGIN IMMEDIATE opened — which, because that BEGIN took the write
// lock, is the latest committed state and cannot move until this transaction
// ends. That is what makes the check race-free rather than merely fresh.
func (s *Store) storeVersionInTx(ctx context.Context, q Queryer) (int, error) {
	version, err := pragmaInt(ctx, q, "user_version")
	recordStoreVersionProbe("user_version")
	if err != nil {
		return 0, err
	}
	return version, nil
}

// pragmaInt reads a PRAGMA that yields an integer. The name is a fixed literal
// at every call site — never caller input — so there is nothing to bind.
func pragmaInt(ctx context.Context, q Queryer, pragma string) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
		return 0, fmt.Errorf("read store %s: %w", pragma, err)
	}
	return v, nil
}

// StoreVersionStatus is what a reader needs in order to REPORT a store newer
// than this build: the store's own version, this build's, and whether the store
// has moved past it. ghost_health renders it; a CLI diagnostic can print the
// same line.
//
// It reads on the store's own handle and never writes. Health is a rare,
// human-triggered read whose whole purpose is to state the CURRENT answer, so it
// takes the handle's view as it is right now rather than a value captured at
// open — the very value a running server's own writes no longer trust. A zero
// Newer with a non-nil error means the question could not be answered, which a
// caller must not render as healthy.
//
// The context reaches the query, and that matters on this pool specifically:
// OpenDB pins MaxOpenConns(1), so a read issued while a write transaction holds
// the connection waits for it. Using DBUserVersion here — which goes to
// db.QueryRow, with no context — would make `ghost_health` block for the whole
// duration of a bulk import or a vacuum with no way for the caller to give up,
// which is the same hazard as the pre-BEGIN probe this check exists to avoid,
// one level up.
func (s *Store) StoreVersionStatus(ctx context.Context) (storeVersion, buildVersion int, newer bool, err error) {
	version, err := pragmaInt(ctx, s.db, "user_version")
	if err != nil {
		return 0, SchemaVersion(), false, err
	}
	return version, SchemaVersion(), version > SchemaVersion(), nil
}
