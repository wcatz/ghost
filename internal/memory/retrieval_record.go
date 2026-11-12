package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// RowVerdict records one memory's kept or dropped verdict in a retrieval call.
type RowVerdict struct {
	ID     string `json:"id"`
	Kept   bool   `json:"kept"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
}

// RetrievalRecord stores one retrieval call record.
type RetrievalRecord struct {
	ProjectID  string
	RowID      int64
	SessionID  string
	Source     string
	QueryHash  string
	AsOf       string
	Outcome    string
	Reason     string
	Verdicts   []RowVerdict
	RecordedAt string
}

func readableVerdicts(col string) string {
	return "CASE WHEN NOT COALESCE(" + verdictsUnreadable(col) + ", 0) THEN " + col + " ELSE '[]' END"
}

func verdictsUnreadable(col string) string {
	return "(json_valid(" + col + ") = 0 OR COALESCE(json_type(CASE WHEN json_valid(" +
		col + ") = 1 THEN " + col + " END), 'not-an-array') <> 'array')"
}

func retrievalRecordNamingMemory() string {
	return `EXISTS (
			SELECT 1 FROM json_each(` + readableVerdicts(`retrieval_record.verdicts`) + `)
			WHERE value->>'id' = ?
		) OR (COALESCE(` + verdictsUnreadable(`retrieval_record.verdicts`) + `, 0)
		     AND instr(retrieval_record.verdicts, '"id":' || json_quote(?)) > 0)`
}

var retrievalRecordRowsCap = 5000

var errRetrievalNoProject = errors.New("record retrieval: a project id is required")

func lockRecordWrite(ctx context.Context, mu *sync.RWMutex) error {
	for {
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recordLockPoll):
		}
	}
}

const recordLockPoll = time.Millisecond

func (s *Store) RecordRetrieval(ctx context.Context, rec RetrievalRecord) error {
	if rec.ProjectID == "" {
		return errRetrievalNoProject
	}
	verdicts := rec.Verdicts
	if verdicts == nil {
		verdicts = []RowVerdict{}
	}
	verdictJSON, err := json.Marshal(verdicts)
	if err != nil {
		return fmt.Errorf("record retrieval: encode verdicts: %w", err)
	}

	if err := lockRecordWrite(ctx, &s.mu); err != nil {
		return fmt.Errorf("record retrieval: store is busy: %w", err)
	}
	defer s.mu.Unlock()

	conn, lock, err := s.beginScopedWrite(ctx, "record-retrieval")
	if err != nil {
		return fmt.Errorf("record retrieval: %w", err)
	}
	defer conn.close(s, "record-retrieval") // a failed restore is logged, not swallowed

	tx := conn.tx
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var rowid int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO retrieval_record
			(project_id, session_id, source, query_hash, as_of, outcome, reason, verdicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING rowid
	`, rec.ProjectID, rec.SessionID, rec.Source, rec.QueryHash,
		rec.AsOf, rec.Outcome, rec.Reason, string(verdictJSON)).Scan(&rowid); err != nil {
		return fmt.Errorf("record retrieval: %w", err)
	}

	if rowid > int64(retrievalRecordRowsCap) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_audit WHERE record_rowid > 0 AND record_rowid <= ?`,
			rowid-int64(retrievalRecordRowsCap),
		); err != nil {
			return fmt.Errorf("record retrieval: take the verdicts of the evicted calls: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_record WHERE rowid <= ?`, rowid-int64(retrievalRecordRowsCap),
		); err != nil {
			return fmt.Errorf("record retrieval: cap table size: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record retrieval: %w", err)
	}
	lock.reportHold("record-retrieval", time.Now())
	return nil
}

func (sw *scopedWrite) close(s *Store, op string) { s.restoreScopedBusyTimeout(sw.restore, op) }

const retrievalBusyTimeoutFloor = 150 * time.Millisecond

const retrievalBusyTimeoutMargin = 40 * time.Millisecond

type scopedWrite struct {
	tx      *sql.Tx
	conn    *sql.Conn
	restore func() error
}

func (s *Store) beginScopedWrite(ctx context.Context, op string) (*scopedWrite, writeLock, error) {
	start := time.Now()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, writeLock{}, err
	}
	var previous int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&previous); err != nil {
		_ = conn.Close()
		return nil, writeLock{}, fmt.Errorf("read busy_timeout: %w", err)
	}
	budget := retrievalBusyTimeoutFloor
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline) - retrievalBusyTimeoutMargin; left > 0 && left < budget {
			budget = left
		}
	}
	if err := setScopedBusyTimeout(ctx, conn, budget); err != nil {
		_ = conn.Close()
		return nil, writeLock{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		s.restoreScopedBusyTimeout(restorePragma(conn, previous), op)
		if isLockContention(err) {
			writeLock{wait: time.Since(start)}.reportLost(op)
		}
		return nil, writeLock{}, err
	}

	sw := &scopedWrite{tx: tx, conn: conn, restore: restorePragma(conn, previous)}
	if err := s.checkStoreNotNewer(ctx, tx); err != nil {
		_ = tx.Rollback()
		s.restoreScopedBusyTimeout(sw.restore, op)
		return nil, writeLock{}, err
	}
	return sw, writeLock{wait: time.Since(start), took: time.Now()}, nil
}

func (s *Store) restoreScopedBusyTimeout(restore func() error, op string) {
	if err := restore(); err != nil {
		s.logger.Warn("the retrieval record's scoped busy_timeout could not be restored; "+
			"this connection may carry a shorter write budget for the rest of the process",
			"op", op, "error", err)
	}
}

func restorePragma(conn *sql.Conn, previous int) func() error {
	return func() error {
		err := setScopedBusyTimeout(context.Background(), conn,
			time.Duration(previous)*time.Millisecond)
		_ = conn.Close()
		return err
	}
}

func setScopedBusyTimeout(ctx context.Context, conn *sql.Conn, d time.Duration) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = `+
		strconv.Itoa(int(d.Milliseconds()))); err != nil {
		return fmt.Errorf("set the scoped busy_timeout: %w", err)
	}
	return nil
}

func (s *Store) DigestQuery(query string) (string, error) { return QueryDigest(query) }

func (s *Store) RetrievalRecords(ctx context.Context, limit int) ([]RetrievalRecord, error) {
	if limit <= 0 {
		limit = retrievalRecordRowsCap
	}
	prealloc := limit
	if prealloc > retrievalRecordRowsCap {
		prealloc = retrievalRecordRowsCap
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, project_id, session_id, source, query_hash, as_of, outcome, reason,
		       verdicts, recorded_at
		FROM retrieval_record ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read retrieval records: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]RetrievalRecord, 0, prealloc)
	for rows.Next() {
		var rec RetrievalRecord
		var verdictJSON string
		if err := rows.Scan(&rec.RowID, &rec.ProjectID, &rec.SessionID, &rec.Source,
			&rec.QueryHash, &rec.AsOf, &rec.Outcome, &rec.Reason, &verdictJSON,
			&rec.RecordedAt); err != nil {
			return nil, fmt.Errorf("read retrieval records: %w", err)
		}
		if err := json.Unmarshal([]byte(verdictJSON), &rec.Verdicts); err != nil {
			rec.Verdicts = nil
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retrieval records: %w", err)
	}
	return out, nil
}
