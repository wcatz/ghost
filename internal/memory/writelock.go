package memory

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
type WriteLockSample struct {
	Op      string
	Wait    time.Duration
	Hold    time.Duration
	Retried bool
	Lost    bool
}

type writeLockHook struct{ fn func(WriteLockSample) }

var writeLockObserver atomic.Pointer[writeLockHook]

// SetWriteLockObserver installs a write-lock observer and returns a function
func SetWriteLockObserver(fn func(WriteLockSample)) (restore func()) {
	prev := writeLockObserver.Load()
	if fn != nil {
		writeLockObserver.Store(&writeLockHook{fn: fn})
	} else {
		writeLockObserver.Store(nil)
	}
	return func() { writeLockObserver.Store(prev) }
}

type writeLock struct {
	wait    time.Duration
	took    time.Time
	retried bool
}

func (w writeLock) reportHold(op string, end time.Time) {
	h := writeLockObserver.Load()
	if h == nil {
		return
	}
	h.fn(WriteLockSample{Op: op, Wait: w.wait, Hold: end.Sub(w.took), Retried: w.retried})
}

func (w writeLock) reportLost(op string) {
	h := writeLockObserver.Load()
	if h == nil {
		return
	}
	h.fn(WriteLockSample{Op: op, Wait: w.wait, Lost: true, Retried: w.retried})
}

func (s *Store) beginWrite(ctx context.Context, op string) (*sql.Tx, writeLock, error) {
	return s.beginGuardedWrite(ctx, op)
}

func isLockContention(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
		return false
	}
	msg := err.Error()
	for _, needle := range []string{"SQLITE_BUSY", "SQLITE_LOCKED", "database is locked", "database table is locked"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
