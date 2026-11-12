package memory

import (
	"context"
	"database/sql"
	"fmt"
)

// DiagnosticRead is one diagnostic read: a read transaction on the store's
// read handle, and the store bound to it.
type DiagnosticRead struct {
	store *Store
	tx    *sql.Tx
}

func (d *DiagnosticRead) Store() *Store {
	if d == nil {
		return nil
	}
	return d.store
}

func (d *DiagnosticRead) Close() error {
	if d == nil || d.tx == nil {
		return nil
	}
	return d.tx.Rollback()
}

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

	view := &Store{
		db:            s.db,
		snapshot:      tx,
		logger:        s.logger,
		foreignWarned: s.foreignWarned,
	}
	view.embeddingIdentity.Store(identityPtr(s.embeddingIdentityValue()))
	return &DiagnosticRead{store: view, tx: tx}, nil
}
