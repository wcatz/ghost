package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The write paths that append history, as the phase vocabulary of
// memory_provenance.phase. The CHECK in initSQL (mirrored by migrateV17) is the
// authority; these constants exist so a writer cannot spell one wrong.
//
// They are deliberately about WHICH WRITE PATH touched a row, not about what
// the row now says. `reflect` covers a consolidation rewrite, a reuse and a
// fresh insert alike, because "which reflection run changed this" is the
// question the history exists to answer and the row's own source column already
// records the outcome.
const (
	// phaseSave: a new memories row was inserted.
	phaseSave = "save"
	// phaseUpdate: an existing row was edited (UpdateMemory).
	phaseUpdate = "update"
	// phaseReflect: a consolidation wrote the row — rewrite, reuse or insert.
	phaseReflect = "reflect"
	// phaseMerge: a near-duplicate fold strengthened an existing row.
	phaseMerge = "merge"
	// phaseResolve: a resolve pass stamped resolved_at.
	phaseResolve = "resolve"
	// phaseUnresolve: resolved_at was cleared again.
	phaseUnresolve = "unresolve"
	// phaseSupersede: an active supersedes edge now points at this memory.
	phaseSupersede = "supersede"
	// phaseRestore: a snapshot restore put the row back.
	phaseRestore = "restore"
	// phaseImport: a portable artifact inserted the row.
	phaseImport = "import"
	// phaseDelete: the row is being removed. Recorded from the state the row
	// held immediately before the DELETE, which is the only chance to record
	// it.
	phaseDelete = "delete"
)

// The growth policy for memory_provenance, applied inside the same transaction
// as the append (see pruneMemoryHistoryTx and pruneHistoryTableTx). They are vars
// rather than consts only so the policy tests can lower them; nothing in
// production assigns to them.
var (
	// historyVersionsPerMemory is how many versions of ONE memory survive. A
	// memory that is edited or folded thousands of times is a single row in
	// memories and would otherwise be thousands of rows here. The oldest go:
	// the recent past is what an audit and an as_of read (issue #647) are for.
	historyVersionsPerMemory = 50
	// historyRowsCap bounds the table as a whole. The per-memory cap cannot:
	// every created-then-dropped memory is a distinct memory_id with a couple
	// of rows of its own, and a reflection replace churns them by the hundred.
	historyRowsCap = 20000
)

// historyBatchSize is how many ids one history statement covers, well under
// SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766) for the same reason the resolve
// batches are (setResolvedBatchSize).
const historyBatchSize = 500

// errHistoryNoMemory reports that an id named no live memories row, so there was
// no state to record. It is a sentinel rather than a plain error because one
// caller legitimately expects it — Delete is handed an id that may name nothing,
// and reports "memory not found" in its own words — while every other caller
// treats it as its own bug and must not swallow it.
var errHistoryNoMemory = errors.New("no live memory row")

// selectIDs runs a single-column id query in tx and returns its rows. args is
// read, never appended to, so a caller may go on reusing the same slice for the
// write the read decides.
func selectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HistoryEntry is one recorded event in a memory's history: the state the
// memory held once the write named by Phase landed, plus who performed that
// write when the write path knew.
//
// Agent and SessionID are the PERFORMER, not the memory's stored provenance.
// That is the difference the audit needs: `merge` and `save` carry the
// caller's Provenance, while the lifecycle passes (reflect, resolve, supersede)
// and Delete know no session and leave both empty — an empty field is an
// admission, not a claim that nobody acted. The state columns are read back from
// the memories row inside the appending transaction rather than passed in by the
// caller, so a history row cannot describe a state its memory never held.
type HistoryEntry struct {
	ID         string  `json:"id"`
	MemoryID   string  `json:"memory_id"`
	ProjectID  string  `json:"project_id"`
	RecordedAt string  `json:"recorded_at"`
	Phase      string  `json:"phase"`
	Agent      string  `json:"agent,omitempty"`
	SessionID  string  `json:"session_id,omitempty"`
	Content    string  `json:"content"`
	Category   string  `json:"category"`
	Importance float64 `json:"importance"`
	ResolvedAt *string `json:"resolved_at,omitempty"`
	Source     string  `json:"source"`
}

// appendHistoryTx records one event for memoryID in the caller's transaction.
//
// The state is copied out of the memories row by the INSERT ... SELECT itself,
// so there is no window between "the write landed" and "the state was read" —
// and a caller that forgot to make the row live yet (or has already removed it)
// gets an error naming the count it expected, rather than a silently empty
// history.
//
// Callers decide the ordering: a delete records BEFORE its DELETE, because
// afterwards there is nothing left to read.
func appendHistoryTx(ctx context.Context, tx *sql.Tx, memoryID, phase string, prov Provenance) error {
	return appendHistoryForIDsTx(ctx, tx, []string{memoryID}, phase, prov)
}

// appendHistoryForIDsTx is appendHistoryTx over a set of ids, in one statement
// and one prune. Every caller that has a batch of ids already selected them
// from memories inside the same transaction, so a row that is not there is a
// caller's bug rather than a race — and it is reported as one, because a
// silently dropped history row is exactly the failure this table exists to
// prevent.
func appendHistoryForIDsTx(ctx context.Context, tx *sql.Tx, ids []string, phase string, prov Provenance) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > historyBatchSize {
			batch = ids[:historyBatchSize]
		}
		ids = ids[len(batch):]

		placeholders := make([]string, 0, len(batch))
		// The three scalars come first because they appear first in the
		// statement: SQLite binds positionally in the order the ?s occur in the
		// text, not in the order a reader would expect to pass them.
		args := make([]interface{}, 0, len(batch)+3)
		args = append(args, phase, nullIfEmpty(prov.Agent), nullIfEmpty(prov.SessionID))
		for _, id := range batch {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO memory_provenance
				(memory_id, project_id, phase, agent, session_id,
				 content, category, importance, resolved_at, source)
			SELECT id, project_id, ?, ?, ?, content, category, importance, resolved_at, source
			FROM memories WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return fmt.Errorf("append %s history: %w", phase, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("append %s history rows: %w", phase, err)
		}
		if int(n) != len(batch) {
			return fmt.Errorf("append %s history: %d of %d named no live memory row: %w",
				phase, len(batch)-int(n), len(batch), errHistoryNoMemory)
		}
		for _, id := range batch {
			if err := pruneMemoryHistoryTx(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := pruneHistoryTableTx(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// pruneMemoryHistoryTx drops everything but the newest
// historyVersionsPerMemory versions of one memory. Served by
// idx_provenance_memory, so the cost is a walk of that memory's own rows —
// bounded by the cap itself, since the previous append already applied it.
func pruneMemoryHistoryTx(ctx context.Context, tx *sql.Tx, memoryID string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM memory_provenance
		WHERE memory_id = ?
		  AND rowid NOT IN (
		      SELECT rowid FROM memory_provenance
		      WHERE memory_id = ?
		      ORDER BY recorded_at DESC, rowid DESC
		      LIMIT ?
		  )`, memoryID, memoryID, historyVersionsPerMemory); err != nil {
		return fmt.Errorf("cap history rows for memory %s: %w", memoryID, err)
	}
	return nil
}

// pruneHistoryTableTx caps the table as a whole, in the appending transaction
// — so the bound cannot be lost to a crash between the write and the cleanup,
// and it needs neither a background job nor a clock.
//
// The cap is a rowid window rather than an ORDER BY over every row: an implicit
// rowid is max(rowid)+1 and is never reused, so rowid order IS insertion order,
// and after `rowid <= newest - cap` every surviving rowid lies in a window of
// cap integer values — at most cap rows, whatever the gaps. Reading max(rowid) is
// a single index lookup, so a write under the cap pays one lookup and no
// delete.
func pruneHistoryTableTx(ctx context.Context, tx *sql.Tx) error {
	var newest int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(max(rowid), 0) FROM memory_provenance`,
	).Scan(&newest); err != nil {
		return fmt.Errorf("read newest history row: %w", err)
	}
	if newest <= int64(historyRowsCap) {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_provenance WHERE rowid <= ?`, newest-int64(historyRowsCap),
	); err != nil {
		return fmt.Errorf("cap history table size: %w", err)
	}
	return nil
}

// MemoryHistory returns one memory's recorded history, oldest first — a
// changelog, not a log tail, because the question it answers ("what did this
// memory say, and who changed it?") is read forwards.
//
// A limit of zero or less returns the newest historyVersionsPerMemory rows,
// which by the growth policy is everything one memory is expected to have.
// Otherwise the newest `limit` rows are returned, still oldest first, so a
// limit never shows the beginning of a recent run of writes and hides the rest.
//
// An id with no history returns an empty slice and no error: "this memory was
// never written since the table existed" is an answer, and a reader should not
// have to tell it apart from a database failure.
func (s *Store) MemoryHistory(ctx context.Context, memoryID string, limit int) ([]HistoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = historyVersionsPerMemory
	}

	// The pool is safe here: no transaction is open on this handle. The order is
	// rowid-broken because recorded_at is second-precision — several writes in
	// one reflection share a timestamp, and an arbitrary order among them would
	// make the changelog a coin toss.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, memory_id, project_id, recorded_at, phase, agent, session_id,
		       content, category, importance, resolved_at, source
		FROM memory_provenance
		WHERE memory_id = ?
		ORDER BY recorded_at DESC, rowid DESC
		LIMIT ?`, memoryID, limit)
	if err != nil {
		return nil, fmt.Errorf("read memory history: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var desc []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var agent, sessionID, resolvedAt sql.NullString
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.ProjectID, &e.RecordedAt, &e.Phase,
			&agent, &sessionID, &e.Content, &e.Category, &e.Importance, &resolvedAt, &e.Source,
		); err != nil {
			return nil, fmt.Errorf("scan memory history: %w", err)
		}
		e.Agent = agent.String
		e.SessionID = sessionID.String
		if resolvedAt.Valid {
			v := resolvedAt.String
			e.ResolvedAt = &v
		}
		desc = append(desc, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory history: %w", err)
	}

	out := make([]HistoryEntry, len(desc))
	for i, e := range desc {
		out[len(desc)-1-i] = e
	}
	return out, nil
}
