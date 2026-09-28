package memory

// The write-lock seam: the measurement, and the one bounded BEGIN retry that
// rides on it (issue #671).
//
// Why a seam at all. The contention this exists for is BETWEEN PROCESSES, so the
// measurement cannot live in a _test.go file: internal/memory's multi-process
// contract test (TestMultiProcessSharedDatabase) has to report the write-lock
// hold time per save, per edit and per maintenance batch from processes that are
// not the test binary, or #671's question — is a save lost because a transaction
// holds the lock too long, or because a writer cannot get in at all? — has no
// answer on the runner that failed. What was measurable on a saturated machine
// was the opposite of what the failure suggested: a save's transaction held the
// lock for single-digit milliseconds at the median against a five-second budget,
// while the wait that exhausted the budget belonged to a writer that other
// writers were queued ahead of.
//
// So the seam measures rather than reports. There is no log line: an observer is
// nil unless a test installs one, and the write path pays one atomic load and a
// nil check per transaction.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// WriteLockSample is one write transaction the store opened for itself,
// measured at the two instants that decide whether a concurrent writer waits.
type WriteLockSample struct {
	// Op names the write path: "upsert" for a save, "update" for an edit,
	// "replace" and "reflect-apply" for the lifecycle batches. A distribution
	// is per operation, because a save and a consolidation hold the lock for
	// different reasons and one number for both says nothing about either.
	Op string
	// Wait is how long the caller waited for the write lock: the whole of
	// BeginTx, which is where SQLite's busy handler spends the wait. It is
	// the number a starved writer lives by, and the only one that can exceed
	// busy_timeout — at which point the save is lost.
	Wait time.Duration
	// Hold is how long the write lock was held: the span from a successful
	// BeginTx to Commit. BEGIN IMMEDIATE takes the lock when the
	// transaction starts, so this covers the dedup probes, every write and
	// every history append inside the transaction, and nothing else.
	Hold time.Duration
	// Retried says the first BEGIN came back SQLITE_BUSY and a second
	// attempt took the lock. It is reported rather than asserted on: a fleet
	// that leans on the retry is a number to read, not a failure.
	Retried bool
	// Lost says the transaction never took the write lock at all: BEGIN spent
	// its whole budget and was refused, so Hold is zero. A distribution built
	// from committed transactions only would report the contention that was
	// survivable and hide the contention that was not, which is the half of
	// #671 that matters. Wait is still the whole time the caller was kept out.
	Lost bool
}

// writeLockHook boxes the callback so installing an observer cannot race a write
// already in flight. A Store is used from several goroutines, and a plain
// package-level func var would be a data race rather than a measurement.
type writeLockHook struct{ fn func(WriteLockSample) }

var writeLockObserver atomic.Pointer[writeLockHook]

// SetWriteLockObserver installs a write-lock observer and returns a function
// that restores the previous one. Passing nil removes the observer.
//
// Process-wide rather than per-Store on purpose: the multi-process test installs
// it in a child process before it opens the store, and a per-Store setter would
// have to be called at every construction site to reach the same places, which
// is the wrong reason for a measurement to be missing from one.
//
// Test-only. Nothing in production installs an observer; the value is nil there
// and the cost is one atomic load per write transaction.
func SetWriteLockObserver(fn func(WriteLockSample)) (restore func()) {
	prev := writeLockObserver.Load()
	if fn != nil {
		writeLockObserver.Store(&writeLockHook{fn: fn})
	} else {
		writeLockObserver.Store(nil)
	}
	return func() { writeLockObserver.Store(prev) }
}

// writeLock is the measurement of one write transaction's lock ACQUISITION: how
// long the caller waited, and the instant the lock was held from. The hold is
// only knowable once the transaction ends, so the two halves are joined by
// reportHold rather than reported apart — a sample carrying a wait and no hold
// would be a defect nobody outside this package could see.
type writeLock struct {
	wait    time.Duration
	took    time.Time
	retried bool
}

// reportHold records the sample for a transaction that ended at end. A no-op
// unless an observer is installed, and it is the only place the two halves meet:
// a transaction that ends by rollback is simply not reported, because contention
// is about committed ones and an uncommitted transaction released the lock just
// as fast.
func (w writeLock) reportHold(op string, end time.Time) {
	h := writeLockObserver.Load()
	if h == nil {
		return
	}
	h.fn(WriteLockSample{Op: op, Wait: w.wait, Hold: end.Sub(w.took), Retried: w.retried})
}

// reportLost records a transaction that never held the write lock at all, for
// the reason reportHold's own comment gives: a BEGIN that ran out of budget has
// no hold time, so without this the busiest and unluckiest writer in the fleet
// is missing from the distribution that is supposed to describe it. The wait it
// reports is the whole budget, which is the number the busy_timeout contract
// turns on.
func (w writeLock) reportLost(op string) {
	h := writeLockObserver.Load()
	if h == nil {
		return
	}
	h.fn(WriteLockSample{Op: op, Wait: w.wait, Lost: true, Retried: w.retried})
}

// beginWrite opens one of the store's own write transactions and measures the
// lock acquisition. On a lock refusal it makes ONE more attempt, and only one.
//
// The DSN asks for BEGIN IMMEDIATE, so the write lock is taken HERE, before the
// first statement: Wait is the whole time to get it, and there is no window in
// which this transaction is running without holding it. That is the property
// #560's in-transaction probes depend on, and it is why what is measured is a
// begin-and-hold rather than the statements inside.
//
// The retry is the fix #671's measurement supports, and the reason it is a fix
// rather than a bigger budget is in isLockContention's caller: SQLite's busy
// handler re-polls on a schedule that grows to 100 ms and keeps no queue, so a
// writer that has been asleep at the top of that schedule loses every hand-off to
// one that is awake. That is what exhausted a five-second budget on a loaded
// runner while no single transaction held the lock for more than tens of
// milliseconds. A fresh attempt restarts the handler at its first, shortest poll,
// so the waiter competes again at 1, 2 and 5 ms instead of continuing a schedule
// that had already grown past 100 ms.
//
// ONE extra attempt, never a loop. The second BEGIN is a full second budget, so
// a save that cannot get in has waited ten seconds and is running on a machine
// that is not this one; a loop would turn that into a tool call that never
// returns rather than an error the caller can report. It is also the whole of the
// cost: the transaction never started, so there is nothing to undo and nothing
// half-written, which is the only reason a retry is safe here at all.
//
// Only lock contention is retried. A BEGIN that failed because the handle is
// closed, the context is done or the file is not a database will fail the same
// way a second time, and reporting it immediately is what lets the caller say
// what is actually wrong.
func (s *Store) beginWrite(ctx context.Context, op string) (*sql.Tx, writeLock, error) {
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		return tx, writeLock{wait: time.Since(start), took: time.Now()}, nil
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
	return tx, writeLock{wait: time.Since(start), took: time.Now(), retried: true}, nil
}

// isLockContention reports whether err is SQLite refusing the write lock, which
// is the one database failure a caller may act on by trying again: the
// transaction never started, so nothing was written and nothing needs undoing.
//
// The driver's typed error is asked first and its text is the fallback, because
// the store wraps every database error with fmt.Errorf and a caller may have
// wrapped it again. Both spellings of an extended code are reduced to the
// primary one. SQLITE_BUSY_SNAPSHOT in particular is (SQLITE_BUSY | (2 << 8))
// and is a snapshot conflict rather than a queue — no amount of retrying fixes
// it — but it cannot come back from a BEGIN IMMEDIATE, which takes the write
// lock before it reads anything, so reducing the code costs nothing here and
// keeps the check from depending on which extended code the driver reported.
func isLockContention(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
		return false
	}
	// A driver that reports no typed error — one wrapped or re-spelled by a
	// caller — still says so in its text, and a save must not be lost to a
	// spelling. The spellings listed are the ones the multi-process harness
	// looks for when it classifies a child's failure, so the product and the
	// test that guards it agree on what the words mean.
	msg := err.Error()
	for _, needle := range []string{"SQLITE_BUSY", "SQLITE_LOCKED", "database is locked", "database table is locked"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
