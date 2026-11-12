package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StoreNewerError reports that the store schema is newer than this build can write.
type StoreNewerError struct {
	StoreVersion int
	BuildVersion int
}

func (e *StoreNewerError) Is(target error) bool {
	return target == errStoreNewerSentinel
}

var errStoreNewerSentinel = &StoreNewerError{}

// ErrStoreNewer matches any store-newer refusal.
var ErrStoreNewer error = errStoreNewerSentinel

func (e *StoreNewerError) Error() string {
	return fmt.Sprintf("store schema v%d is newer than this ghost build (v%d) writes — restart the client that runs this ghost server so it picks up the newer binary; reads keep working until then",
		e.StoreVersion, e.BuildVersion)
}

func (s *Store) beginGuardedWrite(ctx context.Context, op string) (*sql.Tx, writeLock, error) {
	s.canon.noteWrite(op)
	start := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		return s.finishGuardedWrite(ctx, op, tx, writeLock{wait: time.Since(start), took: time.Now()})
	}
	if !isLockContention(err) || ctx.Err() != nil {
		return nil, writeLock{}, err
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		lost := writeLock{wait: time.Since(start), retried: true}
		lost.reportLost(op)
		return nil, writeLock{}, err
	}
	return s.finishGuardedWrite(ctx, op, tx, writeLock{wait: time.Since(start), took: time.Now(), retried: true})
}

func (s *Store) finishGuardedWrite(ctx context.Context, op string, tx *sql.Tx, lock writeLock) (*sql.Tx, writeLock, error) {
	if err := s.checkStoreNotNewer(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, writeLock{}, err
	}
	return tx, lock, nil
}

func (s *Store) execGuardedWrite(ctx context.Context, op, query string, args ...any) (sql.Result, error) {
	tx, lock, err := s.beginGuardedWrite(ctx, op)
	if err != nil {
		return nil, err
	}
	res, execErr := tx.ExecContext(ctx, query, args...)
	if execErr != nil {
		_ = tx.Rollback()
		return nil, execErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	lock.reportHold(op, time.Now())
	return res, nil
}

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

func (s *Store) storeVersionInTx(ctx context.Context, q Queryer) (int, error) {
	version, err := pragmaInt(ctx, q, "user_version")
	recordStoreVersionProbe("user_version")
	if err != nil {
		return 0, err
	}
	return version, nil
}

func pragmaInt(ctx context.Context, q Queryer, pragma string) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
		return 0, fmt.Errorf("read store %s: %w", pragma, err)
	}
	return v, nil
}

func (s *Store) StoreVersionStatus(ctx context.Context) (storeVersion, buildVersion int, newer bool, err error) {
	version, err := pragmaInt(ctx, s.queryDB(), "user_version")
	if err != nil {
		return 0, SchemaVersion(), false, err
	}
	return version, SchemaVersion(), version > SchemaVersion(), nil
}
