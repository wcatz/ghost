package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SearchRecord is one real search call's query, the scope it ran under and the
// memory ids it ranked, in rank order. It is written to audit_log under action
// `search` so a retrieval change can be replayed against the queries agents
// actually send rather than only against the synthetic benches.
//
// The query is caller text. The write boundary refuses a token-shaped query
// (or scope value) the same way every other store write does; a refused search
// is dropped, not stored, and the refusal never quotes the text it refused.
type SearchRecord struct {
	ProjectID  string            `json:"project_id"`
	Source     string            `json:"source"`
	Query      string            `json:"query"`
	Scope      map[string]string `json:"scope,omitempty"`
	Category   string            `json:"category,omitempty"`
	Retention  string            `json:"retention,omitempty"`
	Results    []string          `json:"results"`
	RecordedAt string            `json:"recorded_at,omitempty"`
}

// searchRecordRowsCap bounds how many search calls one store keeps, counted
// over action='search' rows rather than over the audit table's rowid: another
// action's writes must not spend any of this cap. It is a var so the retention
// test can lower it without waiting for ten thousand inserts; production never
// changes it.
var searchRecordRowsCap = 10000

const searchAuditAction = "search"

// SearchAllSource is the source a whole-corpus ghost_search_all call records.
// Both the writer and the replay name it, so the gate cannot miss those rows
// by a spelling difference.
const SearchAllSource = "search_all"

// RecordSearch appends one search call to audit_log. A token-shaped query or
// scope value is refused before anything is written: the error unwraps to
// ErrSecretContent so a caller can drop the record without failing the search
// that produced it.
func (s *Store) RecordSearch(ctx context.Context, rec SearchRecord) error {
	if rec.ProjectID == "" {
		return errors.New("record search: a project id is required")
	}
	if err := rejectSecret("query", rec.Query); err != nil {
		return fmt.Errorf("record search: %w", err)
	}
	if err := rejectSearchScope(rec.Scope); err != nil {
		return fmt.Errorf("record search: %w", err)
	}
	if rec.Results == nil {
		rec.Results = []string{}
	}
	details, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("record search: encode details: %w", err)
	}

	if err := lockRecordWrite(ctx, &s.mu); err != nil {
		return fmt.Errorf("record search: store is busy: %w", err)
	}
	defer s.mu.Unlock()

	conn, lock, err := s.beginScopedWrite(ctx, "record-search")
	if err != nil {
		return fmt.Errorf("record search: %w", err)
	}
	defer conn.close(s, "record-search")

	// The explicit type is not decoration: the package's write-guard scan
	// recognises a *sql.Tx by its declared type, so this write is checked as
	// one already inside beginScopedWrite's guarded transaction.
	var tx *sql.Tx = conn.tx
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, project_id, details)
		VALUES (?, ?, ?)
	`, searchAuditAction, rec.ProjectID, string(details)); err != nil {
		return fmt.Errorf("record search: %w", err)
	}

	// Count SEARCH rows, not the table's rowid: another audit action's writes
	// move rowid on without using any of this cap. Counting first also keeps
	// the common case to one read when the log is under its bound.
	var searchRows int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE action = ?`, searchAuditAction,
	).Scan(&searchRows); err != nil {
		return fmt.Errorf("record search: count search rows: %w", err)
	}
	if excess := searchRows - searchRecordRowsCap; excess > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM audit_log WHERE rowid IN (
				SELECT rowid FROM audit_log WHERE action = ? ORDER BY rowid ASC LIMIT ?
			)`, searchAuditAction, excess); err != nil {
			return fmt.Errorf("record search: cap table size: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record search: %w", err)
	}
	lock.reportHold("record-search", time.Now())
	return nil
}

func rejectSearchScope(scope map[string]string) error {
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := rejectSecret("scope["+k+"]", k+"="+scope[k]); err != nil {
			return err
		}
	}
	return nil
}

// SearchRecords reads the newest search calls first. A details column that is
// not a document Ghost can decode yields an empty record rather than failing
// the read: an audit must stay readable even when one row was written by a
// build with a different shape.
func (s *Store) SearchRecords(ctx context.Context, limit int) ([]SearchRecord, error) {
	if limit <= 0 || limit > searchRecordRowsCap {
		limit = searchRecordRowsCap
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(project_id, ''), COALESCE(details, ''), COALESCE(timestamp, '') FROM audit_log
		WHERE action = ? ORDER BY rowid DESC LIMIT ?
	`, searchAuditAction, limit)
	if err != nil {
		return nil, fmt.Errorf("read search records: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]SearchRecord, 0, limit)
	for rows.Next() {
		var projectID, details, recordedAt string
		if err := rows.Scan(&projectID, &details, &recordedAt); err != nil {
			return nil, fmt.Errorf("read search record: %w", err)
		}
		var rec SearchRecord
		if err := json.Unmarshal([]byte(details), &rec); err != nil {
			rec = SearchRecord{}
		}
		if rec.ProjectID == "" {
			rec.ProjectID = projectID
		}
		rec.RecordedAt = recordedAt
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search records: %w", err)
	}
	return out, nil
}
