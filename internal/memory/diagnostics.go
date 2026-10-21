package memory

// The diagnostic read surface (#1738).
//
// Inside one process, the store's mutex is held for the whole of a write: a
// project delete, a reflection apply, a purge's checkpoint. Every reader that
// takes that mutex therefore waits for the write to finish, and a health
// report — the one thing an operator or an agent reaches for WHILE something
// is slow — was the read that waited longest, because it is the one that
// answers "is this store healthy right now".
//
// The fix is not to move every read off the write path: a reader that wants
// its own writes back (read-your-writes inside one tool call) must stay on
// the primary handle, and the vector-cache epoch reads are bound to it. The
// fix is to give the DIAGNOSTIC reads a path of their own: the store's
// read-only handle, where a WAL reader sees the last committed state and
// never waits for a writer.
//
// Two properties make the diagnostic path the right one for a report:
//
//   - It does not wait. The read handle's DSN has no _txlock, so a
//     transaction there is a plain deferred read: WAL readers do not wait on
//     a writer, which is the whole point of running the store in WAL.
//   - It is consistent. Every number in one report comes from ONE read
//     transaction, so the project count and the per-project counts describe
//     the same committed state rather than two moments.
//
// The view is a Store bound to that transaction, and the readers it runs are
// the store's own readers: one implementation, two handles. A reader that
// still reached for the primary handle would queue behind the write again,
// which is why the readers a report is made of read through queryDB.

import (
	"context"
	"database/sql"
	"fmt"
)

// DiagnosticRead is one diagnostic read: a read transaction on the store's
// read handle, and the store bound to it.
//
// The store it carries is a VIEW. Its reads go through the transaction, so
// every number in a report describes one committed state, and it takes none
// of the parent store's mutex, so a report does not wait for a write in
// flight to finish.
//
// The view SHARES the parent's handles and the parent's configuration. It
// must not be closed: its handles belong to the store it was made from.
type DiagnosticRead struct {
	store *Store
	tx    *sql.Tx
}

// Store returns the view the diagnostic reads run on.
//
// The view is not a store a caller may keep: it is valid for as long as its
// DiagnosticRead is open, and closing the DiagnosticRead ends it.
func (d *DiagnosticRead) Store() *Store {
	if d == nil {
		return nil
	}
	return d.store
}

// Close rolls the read transaction back and releases the read snapshot. The
// store's own handles are untouched: they belong to the parent store, and a
// report that closed them would take the whole server's database with it.
func (d *DiagnosticRead) Close() error {
	if d == nil || d.tx == nil {
		return nil
	}
	return d.tx.Rollback()
}

// BeginDiagnosticRead opens ONE read transaction on the store's read handle
// and returns the store bound to it.
//
// The transaction is a plain deferred read (the read handle's DSN carries no
// _txlock), so it pins a WAL read snapshot: it sees the last committed state
// and it does not wait for a writer in this process or in another one.
//
// A store with NO read handle returns (nil, nil): there is nothing to read
// through, and the caller falls back to the store's own reads — which is what
// an in-memory, a bench and a store built without NewStoreWithRead get. The
// fallback is not a failure: those stores have no second process to block.
//
// The read is taken on the store's read handle and NOT on the primary one.
// The primary handle's DSN issues BEGIN IMMEDIATE, so a transaction there
// takes the write lock for its whole lifetime, and the primary pool is pinned
// to one connection: a read on it waits for whatever write holds that
// connection, and waits for the store's mutex with it.
func (s *Store) BeginDiagnosticRead(ctx context.Context) (*DiagnosticRead, error) {
	if s.readDB == nil {
		return nil, nil
	}
	handle := s.readHandle()
	if handle == nil {
		return nil, nil
	}
	tx, err := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin diagnostic read: %w", err)
	}
	// The view carries the store's configuration across, because a report that
	// counted embeddings against a different vector identity than the store
	// uses would report coverage this process cannot search with. The identity
	// is loaded rather than read under the parent store's mutex: that mutex is
	// held for the whole of an in-flight write, and learning a value set once
	// at startup must not wait for one.
	view := &Store{
		db:            s.db,
		snapshot:      tx,
		logger:        s.logger,
		foreignWarned: s.foreignWarned,
	}
	view.embeddingIdentity.Store(identityPtr(s.embeddingIdentityValue()))
	return &DiagnosticRead{store: view, tx: tx}, nil
}
