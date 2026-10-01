package memory

// #646 part 2: the retrieval AUDIT — one row per (call, kept memory), saying
// what the agent did with what the call admitted.
//
// The grain is the pair, not the call, and that is the one thing this table has
// to get right. retrieval_record answers "what was retrieved"; this answers "what
// happened to it", and a memory's fate is a fact about that memory in that
// session that no per-call row can hold. The pair is also what makes the
// verdict REPLACEABLE: keyed by the call's rowid, a second pass over the same
// call overwrites exactly the rows the first pass wrote, so a stop hook that
// fires after every turn does not grow the table with the length of a session.
//
// What a row may hold is decided by the issue's constraint and is enforced by the
// TYPE: an id, a bucket from a closed vocabulary, a signal from a closed
// vocabulary, a degradation reason from the scanner's own vocabulary, and an
// instant. There is no field a transcript phrase, a query or a memory's content
// could be put in — which is why TestRecordRetrievalAuditsStoresNoText can assert
// it on the bytes and keep meaning something after the next writer.
//
// Three operations mirror retrieval_record exactly, for the same three reasons:
// `ghost backup` includes it (a verdict is a fact about a past session); export
// and import EXCLUDE it (it is derived operational data about calls made on this
// machine, describing memories a portable artifact is not obliged to carry); and
// `ghost history purge` DELETEs the rows naming the purged memory, because a
// redaction is asked to remove a NAME and this table keeps one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RetrievalAuditRow is one memory's fate in one recorded call.
type RetrievalAuditRow struct {
	ProjectID string
	// RecordRowID is the rowid of the retrieval_record row this verdict belongs
	// to. Zero means "not attributable to a call", which the write accepts —
	// verdicts that are about a session rather than about one call are a real
	// thing to want to record — and the replacement below ignores, because
	// replacing "everything with rowid 0" would delete every unattributed verdict
	// in the table.
	RecordRowID int64
	SessionID   string
	Source      string
	MemoryID    string
	Outcome     string
	// Signal is what PROVED a positive verdict, and empty on the other three
	// buckets. Drawn from a closed vocabulary by the comparison, never from a
	// caller's string.
	Signal string
	// Degraded is the scanner's reason for a partial transcript read, or "". It
	// rides with the row rather than with the run because a verdict is read back
	// on its own: an "ignored" filed here is a claim about the text that was
	// read, and a reader who cannot see that caveat would read it as a claim
	// about the session.
	Degraded string
	// RecordedAt is the store's clock, stamped by the writer.
	RecordedAt string
}

// retrievalAuditRowsCap bounds the table as a whole, oldest row first.
//
// Larger than retrievalRecordRowsCap's because a call judges MANY memories and
// the report's denominator is calls: at one verdict per kept memory, a window of
// calls worth auditing is several times that many rows. The number is a var for
// the reason the record cap's is — the eviction is the policy, and a test that
// cannot lower the bound cannot assert it.
var retrievalAuditRowsCap = 50000

// The two refusals. Both are about a row that could be stored and never counted:
// a verdict naming no memory cannot be reported on or purged, and one naming no
// project answers "which project's retrieval was this" as "all of them", which
// is a claim about no call.
var (
	errRetrievalAuditNoMemory  = errors.New("record retrieval audits: a memory id is required")
	errRetrievalAuditNoProject = errors.New("record retrieval audits: a project id is required")
)

// RecordRetrievalAudits stores one pass's verdicts, replacing the verdicts it
// supersedes.
//
// Replace-then-insert, keyed by RecordRowID, in ONE transaction with the cap's
// eviction. The replacement is what makes the stop hook safe to run every turn,
// and doing it in the same transaction as the insert means the table can never
// hold both passes — not even for the duration of a statement.
//
// Only a rowid ABOVE ZERO is a replacement key. Zero is the "not attributable to
// a call" value above, and treating it as a key would make every unattributed
// verdict in the table replace every other one.
//
// The write is refused, not discouraged, on a store a newer Ghost owns (#746), for
// the reason the record write refuses: this table's vocabulary of buckets and
// signals is this build's, and a store that may since have added a bucket would
// have this build's rows filed beside it. It goes through beginWrite, so the check
// runs in the same transaction as the write it guards.
func (s *Store) RecordRetrievalAudits(ctx context.Context, rows []RetrievalAuditRow) error {
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if r.MemoryID == "" {
			return errRetrievalAuditNoMemory
		}
		if r.ProjectID == "" {
			return errRetrievalAuditNoProject
		}
	}

	tx, lock, err := s.beginWrite(ctx, "record-retrieval-audits")
	if err != nil {
		return fmt.Errorf("record retrieval audits: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	seen := map[int64]bool{}
	for _, r := range rows {
		if r.RecordRowID <= 0 || seen[r.RecordRowID] {
			continue
		}
		seen[r.RecordRowID] = true
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_audit WHERE record_rowid = ?`, r.RecordRowID); err != nil {
			return fmt.Errorf("record retrieval audits: replace the verdicts of call %d: %w", r.RecordRowID, err)
		}
	}

	var maxRowID int64
	for _, r := range rows {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO retrieval_audit
				(project_id, record_rowid, session_id, source, memory_id, outcome, signal, degraded)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING rowid
		`, r.ProjectID, r.RecordRowID, r.SessionID, r.Source, r.MemoryID,
			r.Outcome, r.Signal, r.Degraded).Scan(&maxRowID); err != nil {
			return fmt.Errorf("record retrieval audits: %w", err)
		}
	}

	// Oldest first by rowid, bounded by the row just written, so the table settles
	// AT the cap rather than cap+1 — the same arithmetic the record cap uses, for
	// the same reason: recorded_at is second-precision, so two rows written in one
	// second have no defined order by it.
	if maxRowID > int64(retrievalAuditRowsCap) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_audit WHERE rowid <= ?`,
			maxRowID-int64(retrievalAuditRowsCap)); err != nil {
			return fmt.Errorf("record retrieval audits: cap table size: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record retrieval audits: %w", err)
	}
	lock.reportHold("record-retrieval-audits", time.Now())
	return nil
}

// RetrievalAudits returns stored verdicts, oldest row first.
//
// Both filters are optional and both are exact: an empty project reads every
// project's rows and an empty outcome reads every bucket. A per-project,
// per-bucket figure is what the report is made of, and pooling two projects' rows
// would produce a number about neither.
//
// No CHECK constraint on outcome, deliberately, and the reason is the same one
// retrieval_record's verdicts column has: this table's rows are read back by
// builds that did not write them. A store a later Ghost extended with a new
// bucket must still be readable by this one — the rows are still rows to count —
// and a constraint that refused the value would only make the tolerance
// unreachable from Ghost's own writers.
func (s *Store) RetrievalAudits(ctx context.Context, projectID, outcome string) ([]RetrievalAuditRow, error) {
	query := `
		SELECT project_id, record_rowid, session_id, source, memory_id, outcome, signal,
		       degraded, recorded_at
		FROM retrieval_audit`
	var where []string
	var args []interface{}
	if projectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, projectID)
	}
	if outcome != "" {
		where = append(where, `outcome = ?`)
		args = append(args, outcome)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY rowid`

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read retrieval audits: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []RetrievalAuditRow
	for rows.Next() {
		var r RetrievalAuditRow
		if err := rows.Scan(&r.ProjectID, &r.RecordRowID, &r.SessionID, &r.Source,
			&r.MemoryID, &r.Outcome, &r.Signal, &r.Degraded, &r.RecordedAt); err != nil {
			return nil, fmt.Errorf("read retrieval audits: %w", err)
		}
		// A row naming no memory is the one shape that cannot be reported on at
		// all: there is no id to count it under, so a reader that returned it
		// would hand a caller a verdict about nothing. The WRITE refuses it too —
		// this is the read being defensive against a row written by another build
		// or by hand, which is the only way one can arrive.
		if r.MemoryID == "" {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retrieval audits: %w", err)
	}
	return out, nil
}

// RetrievalRecordsForProject returns a project's newest calls first, at most
// `limit` of them.
//
// Scoped by project because every figure in the audit is per project: a window
// over another project's calls would judge a memory against a transcript the
// agent never had in front of it and file the result under the wrong project.
//
// Ordered by rowid rather than recorded_at, and limited the same way
// RetrievalRecords limits — see the precision reason there.
func (s *Store) RetrievalRecordsForProject(ctx context.Context, projectID string, limit int) ([]RetrievalRecord, error) {
	return s.retrievalRecords(ctx, projectID, limit)
}

// retrievalRecords is the one decode behind both readers, so a row's fields are
// read the same way whichever reader asked for it.
func (s *Store) retrievalRecords(ctx context.Context, projectID string, limit int) ([]RetrievalRecord, error) {
	if limit <= 0 {
		limit = retrievalRecordRowsCap
	}
	prealloc := limit
	if prealloc > retrievalRecordRowsCap {
		prealloc = retrievalRecordRowsCap
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT rowid, project_id, session_id, source, query_hash, as_of, outcome, reason,
		       verdicts, recorded_at
		FROM retrieval_record`
	var args []interface{}
	if projectID != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
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
		// One decode settles every case the SQL guard would, for the reason
		// RetrievalRecords gives: a document that is not JSON, a JSON document
		// that is not an array, and an array of something that is not a verdict
		// all read as "this call judged nothing", which is honest, where a partial
		// parse would claim a row the tool could not read was judged clean.
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
