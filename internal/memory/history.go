package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	sqlite "modernc.org/sqlite"
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
	// phaseUnsupersede: that edge was invalidated, so the claim is withdrawn.
	// Its own phase rather than a flag on the supersede row: a memory can be
	// superseded, un-superseded and superseded again, and only the sequence
	// says which claim is currently live.
	phaseUnsupersede = "unsupersede"
	// phaseRestore: a snapshot restore put the row back.
	phaseRestore = "restore"
	// phaseImport: a portable artifact inserted the row.
	phaseImport = "import"
	// phaseDelete: the row is being removed. Recorded from the state the row
	// held immediately before the DELETE, which is the only chance to record
	// it — unless the delete was asked to PURGE, which takes the history with
	// the row.
	phaseDelete = "delete"
	// phaseBaseline: what the memory said before this build ever recorded a
	// history row for it. See recordBaselineHistoryTx.
	phaseBaseline = "baseline"
)

// The growth policy for memory_provenance, applied inside the same transaction
// as the append (see pruneHistoryForIDsTx and pruneHistoryTableTx). They are vars
// rather than consts only so the policy tests can lower them; nothing in
// production assigns to them.
var (
	// historyVersionsPerMemory is how many versions of ONE memory survive. A
	// memory that is edited or folded thousands of times is a single row in
	// memories and would otherwise be thousands of rows here. The newest go
	// last: the recent past is what an audit and an as_of read (issue #647) are
	// for. The trim ranks by rowid, so a memory always keeps its newest row —
	// the one statement of what it says now — and a memory under the cap keeps
	// everything, which is why a rarely-changed memory never loses the baseline
	// its first write recorded.
	historyVersionsPerMemory = 50
	// historyRowsCap bounds the table as a whole. The per-memory cap cannot:
	// every created-then-dropped memory is a distinct memory_id with a couple
	// of rows of its own, and a reflection replace churns them by the hundred.
	historyRowsCap = 20000
)

// historyBatchSize is how many ids one history statement covers, well under
// SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766) for the same reason the resolve
// batches are (setResolvedBatchSize). Each id costs six bound parameters (the
// event's five scalars and the id itself), so a batch is deliberately far below
// the limit rather than near it.
const historyBatchSize = 500

// errHistoryNoMemory reports that an id named no live memories row, so there was
// no state to record. It is a sentinel rather than a plain error because one
// caller legitimately expects it — Delete is handed an id that may name nothing,
// and reports "memory not found" in its own words — while every other caller
// treats it as its own bug and must not swallow it.
var errHistoryNoMemory = errors.New("no live memory row")

// redactHistoryContent rewrites content on its way into a history row, and is the
// seam a credential detector plugs into.
//
// The history is the one place Ghost keeps text it no longer holds anywhere else:
// a memory row is overwritten by the next edit and gone by the next delete,
// while its earlier versions sit in this table and are printed by
// `ghost history`. A credential redacted by an edit or a delete therefore has to
// be redacted here too, or the redaction leaves a longer-lived copy of the
// secret than the row it was made to remove.
//
// It is the identity function today because internal/secret (#656) — the
// value-SHAPE detector every Ghost writer consults before it stores
// caller-supplied text — is not on main yet. Wiring it is one line, and this is
// that line: a var rather than a direct call so the plumbing is testable before
// the detector exists, which is what makes #656 a change of behaviour rather than
// a change of shape.
//
// TODO(#656): call secret.Detect here and store its replacement. The
// replacement, not a refusal: refusing would fail the user's own write over
// something the history cannot even see.
var redactHistoryContent = func(content string) string { return content }

// historyContentFunc is the SQL name the append statement calls to reach that Go
// function, so redaction happens INSIDE the one statement that copies the state
// out of memories. A second pass over the rows just written would work too, and
// would be worse: it would open a window in which an unredacted copy of a
// credential sits in the table, inside a transaction whose failure mode is "roll
// the whole thing back".
const historyContentFunc = "ghost_history_content"

func init() {
	// Registered per process rather than per connection: modernc.org/sqlite keeps
	// one global function table, and the callback reads the current filter through
	// the package var instead of capturing it, so a filter installed after this
	// runs still applies.
	if err := sqlite.RegisterDeterministicScalarFunction(historyContentFunc, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			content, ok := args[0].(string)
			if !ok || content == "" {
				return args[0], nil
			}
			return redactHistoryContent(content), nil
		}); err != nil {
		// The only realistic failure is a duplicate name, which happens when a
		// second registration reaches the same process — harmless, because the
		// first already points at the same var. Warned rather than fatal: a store
		// that cannot redact is still a working store, and the alternative is
		// refusing to open the database at all.
		slog.Warn("register history content filter", "function", historyContentFunc, "error", err)
	}
}

// setHistoryRedactor installs the filter history rows are written through and
// returns a function that restores the previous one. It exists for the seam's own
// tests and for the wiring #656 will add; nothing in production calls it.
func setHistoryRedactor(fn func(string) string) func() {
	prev := redactHistoryContent
	if fn != nil {
		redactHistoryContent = fn
	}
	return func() { redactHistoryContent = prev }
}

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
// That is the difference the audit needs: `merge` and `save` carry the caller's
// Provenance, while the lifecycle passes (reflect, resolve, supersede) and Delete
// know no session and leave both empty — an empty field is an admission, not a
// claim that nobody acted. The state columns are read back from the memories row
// inside the appending transaction rather than passed in by the caller, so a
// history row cannot describe a state its memory never held.
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

	// RelatedID is the other memory this event is about, when there is one: on
	// a delete, the id that replaced this row (a consolidation rewrite or merge
	// that gave the row a new id), and on a supersede or unsupersede, the memory
	// whose edge makes or withdraws the claim. It is what lets a reader follow
	// one memory's history into its successor's instead of stopping at the
	// rewrite (#648).
	RelatedID string `json:"related_id,omitempty"`
	// MergedContent is the incoming near-duplicate text a merge folded in, and
	// is set only when the fold did not keep it as a row of its own — the
	// FoldOnly path, where the wording is deliberately dropped. The default fold
	// stores that text as a linked copy with its own save row, so it is empty
	// there and this is not a second copy of that copy.
	MergedContent string `json:"merged_content,omitempty"`
}

// historyEvent is one event to record. The zero value records a state snapshot
// with no performer and no other end, which is most events.
type historyEvent struct {
	// phase is the event's phase; required.
	phase string
	// prov is the performing agent, when the write path knows one.
	prov Provenance
	// relatedID is the other memory the event is about, if any.
	relatedID string
	// mergedContent is the folded-in text a merge did not keep as a row.
	mergedContent string
}

// appendHistoryTx records one event for memoryID in the caller's transaction.
//
// The state is copied out of the memories row by the INSERT ... SELECT itself,
// so there is no window between "the write landed" and "the state was read" — and
// a caller that forgot to make the row live yet gets an error naming the count
// it expected, rather than a silently empty history.
//
// Callers decide the ordering. A write that records the state it changed appends
// after its statement; a delete appends BEFORE its DELETE, because afterwards
// there is nothing left to read.
func appendHistoryTx(ctx context.Context, tx *sql.Tx, memoryID, phase string, prov Provenance) error {
	return appendHistoryEventsTx(ctx, tx, []historyEvent{{phase: phase, prov: prov}}, []string{memoryID})
}

// appendHistoryForIDsTx is appendHistoryTx over a set of ids with no other end,
// in one statement and one prune. Every caller that has a batch of ids already
// selected them from memories inside the same transaction, so a row that is not
// there is a caller's bug rather than a race — and it is reported as one, because
// a silently dropped history row is exactly the failure this table exists to
// prevent.
func appendHistoryForIDsTx(ctx context.Context, tx *sql.Tx, ids []string, phase string, prov Provenance) error {
	events := make([]historyEvent, len(ids))
	for i := range events {
		events[i] = historyEvent{phase: phase, prov: prov}
	}
	return appendHistoryEventsTx(ctx, tx, events, ids)
}

// appendHistoryEventsTx appends one event per id, in one statement per batch.
// events[i] describes ids[i].
//
// One statement per batch is deliberate on both sides: the whole batch is one
// transaction, so a per-id loop would multiply the statements of a reflection
// replace by three, and the batch is also the granularity the per-memory trim
// runs at.
func appendHistoryEventsTx(ctx context.Context, tx *sql.Tx, events []historyEvent, ids []string) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > historyBatchSize {
			batch = ids[:historyBatchSize]
		}
		ids = ids[len(batch):]
		batchEvents := events[:len(batch)]
		events = events[len(batch):]

		// One statement per DISTINCT event in the batch, and the batch has one
		// event in every real caller — a phase, an agent and a folded text are
		// the same for every row a resolve pass or a replace touches. The grouping
		// is what makes that true without assuming it: a statement carries its
		// event's scalars once, so a batch that did mix events takes one statement
		// per event rather than binding the wrong scalars to a row.
		for start := 0; start < len(batch); {
			end := start + 1
			for end < len(batch) && batchEvents[end] == batchEvents[start] {
				end++
			}
			if err := appendHistoryGroupTx(ctx, tx, batchEvents[start], batch[start:end]); err != nil {
				return err
			}
			start = end
		}
		if err := pruneHistoryForIDsTx(ctx, tx, batch); err != nil {
			return err
		}
		if err := pruneHistoryTableTx(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// appendHistoryGroupTx appends one event for every id in the group, in a single
// INSERT ... SELECT so the state each row records is that row's own — read by the
// statement, at the moment it runs, rather than passed in by a caller that could
// disagree with it.
func appendHistoryGroupTx(ctx context.Context, tx *sql.Tx, e historyEvent, ids []string) error {
	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids)+5)
	args = append(args, e.phase, nullIfEmpty(e.prov.Agent), nullIfEmpty(e.prov.SessionID),
		nullIfEmpty(e.relatedID), nullIfEmpty(e.mergedContent))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(memory_id, project_id, phase, agent, session_id, related_id, merged_content,
			 content, category, importance, resolved_at, source)
		SELECT id, project_id, ?, ?, ?, ?, ?, ghost_history_content(content),
		       category, importance, resolved_at, source
		FROM memories WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return fmt.Errorf("append %s history: %w", e.phase, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("append %s history rows: %w", e.phase, err)
	}
	if int(n) != len(ids) {
		return fmt.Errorf("append %s history: %d of %d named no live memory row: %w",
			e.phase, len(ids)-int(n), len(ids), errHistoryNoMemory)
	}
	return nil
}

// recordBaselineHistoryTx records what a memory held BEFORE a write that
// destroys its text, but only when the memory has no history row yet.
//
// That condition is the whole point. migrateV17 records no starting row for the
// memories that already exist, so every pre-v17 memory reaches this build with
// no history at all — and an edit is the one write that overwrites text nothing
// else keeps. Without a baseline, the first UpdateMemory of a v16 memory files
// the NEW text and the old wording is gone: unrecoverable from the database, and
// just as unrecoverable for #647's as_of read, which would have no version to
// read before the edit.
//
// It reads the live row rather than a value the caller passed in, which is only
// correct because every caller runs it BEFORE its own write — at which point the
// row still holds exactly the state being replaced. A caller that moved its
// statement would get the new text stamped as the baseline, so the ordering is
// part of this function's contract rather than an incidental detail.
//
// It is a no-op for every memory that already has history, which is every memory
// written by this build: one indexed existence check on the edit path, nothing
// elsewhere.
func recordBaselineHistoryTx(ctx context.Context, tx *sql.Tx, memoryID, phase string, prov Provenance) error {
	var rows int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_provenance WHERE memory_id = ?`, memoryID,
	).Scan(&rows); err != nil {
		return fmt.Errorf("check for existing history: %w", err)
	}
	if rows > 0 {
		return nil
	}
	return appendHistoryTx(ctx, tx, memoryID, phase, prov)
}

// purgeHistoryTx removes every history row for a memory and reports how many went.
// It is the redaction path: this table keeps the text a memory USED to hold, so
// deleting a memory that contained a credential leaves the credential here unless
// the delete asks for this. The delete path runs it in the same transaction as
// the DELETE, so a memory and its history cannot come apart.
//
// No tombstone is written first and nothing is left behind: a purge exists
// precisely to leave nothing, which is also why it is the one delete that appends
// no history of its own.
func purgeHistoryTx(ctx context.Context, tx *sql.Tx, memoryID string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM memory_provenance WHERE memory_id = ?`, memoryID)
	if err != nil {
		return 0, fmt.Errorf("purge memory history: %w", err)
	}
	return res.RowsAffected()
}

// PurgeMemoryHistory erases a memory's recorded history without touching the
// memory row, and reports how many rows it removed.
//
// It is the second stage of the redaction path, and it exists because a purge
// asked for at DELETE time cannot cover a memory that is already gone. The
// tombstone is the point: a memory deleted an hour ago still has its text here,
// so "erase that secret" asked an hour later needs this rather than a delete
// that reports the memory as not found and leaves the text where it is. It is
// also what makes a memory id reusable — `ghost import` refuses an id that still
// has history, and this is how an operator frees one.
//
// It erases history only. A live memory is left exactly as it was, because
// "erase the history of this memory" and "delete this memory" are different
// requests and conflating them would destroy knowledge nobody asked to lose.
func (s *Store) PurgeMemoryHistory(ctx context.Context, memoryID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin purge history: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	n, err := purgeHistoryTx(ctx, tx, memoryID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit purge history: %w", err)
	}
	return n, nil
}

// linkSuccessorTx records, on the delete row of a memory a consolidation
// replaced, which row now holds its content.
//
// The chain breaks without it. A rewrite or a merge gives the row a NEW id — the
// new text is not byte-identical, so ReplaceNonManual cannot reuse the old row —
// so the old id's history simply stops. A reader following one memory's record
// (what #648 will do with a usefulness verdict) would have no way to learn that
// the memory it holds a verdict about is now a different row.
//
// Only a delete row with no successor yet is stamped: one that already names a
// successor was written by a later pass, and overwriting it would move the older
// claim onto the newer successor.
func linkSuccessorTx(ctx context.Context, tx *sql.Tx, oldID, successorID string) error {
	if oldID == "" || successorID == "" || oldID == successorID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_provenance
		SET related_id = ?
		WHERE memory_id = ? AND phase = ? AND related_id IS NULL
	`, successorID, oldID, phaseDelete); err != nil {
		return fmt.Errorf("record history successor for %s: %w", oldID, err)
	}
	return nil
}

// pruneHistoryForIDsTx applies the per-memory half of the growth policy to a
// batch in ONE statement: for each named memory, everything past its newest
// historyVersionsPerMemory rows goes.
//
// The rank is by rowid, not recorded_at. recorded_at is second-precision, so
// every write one reflection makes in a pass shares a timestamp and the order
// among them is arbitrary — an audit whose newest rows are chosen by a tie-break
// is not deterministic. rowid is monotonic and never reused, so it is insertion
// order exactly.
//
// Ranking by rowid DESC also means the trim can never remove a memory's newest
// row: it is rank 1, and the predicate only takes rank above the cap.
func pruneHistoryForIDsTx(ctx context.Context, tx *sql.Tx, ids []string) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > historyBatchSize {
			batch = ids[:historyBatchSize]
		}
		ids = ids[len(batch):]

		placeholders := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		for _, id := range batch {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		args = append(args, historyVersionsPerMemory)
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM memory_provenance
			WHERE rowid IN (
			    SELECT rowid FROM (
			        SELECT rowid, ROW_NUMBER() OVER (
			                   PARTITION BY memory_id ORDER BY rowid DESC) AS rank
			        FROM memory_provenance
			        WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
			    )
			    WHERE rank > ?
			)`, args...); err != nil {
			return fmt.Errorf("cap history rows per memory: %w", err)
		}
	}
	return nil
}

// pruneHistoryTableTx caps the table as a whole, in the appending transaction —
// so the bound cannot be lost to a crash between the write and its cleanup, and
// it needs neither a background job nor a clock.
//
// The cap is a rowid window rather than an ORDER BY over every row: an implicit
// rowid is max(rowid)+1 and is never reused, so rowid order IS insertion order,
// and after `rowid <= newest - cap` every surviving rowid lies in a window of cap
// integer values — at most cap rows, whatever the gaps. Reading max(rowid) is a
// single index lookup, so a write under the cap pays one lookup and no delete.
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
// Otherwise the newest `limit` rows are returned, still oldest first, so a limit
// never shows the beginning of a recent run of writes and hides the rest.
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
	// by rowid rather than recorded_at, because recorded_at is second-precision
	// and several writes in one reflection share a timestamp: ordering by it
	// would make the changelog a coin toss among them.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, memory_id, project_id, recorded_at, phase, agent, session_id, related_id,
		       merged_content, content, category, importance, resolved_at, source
		FROM memory_provenance
		WHERE memory_id = ?
		ORDER BY rowid DESC
		LIMIT ?`, memoryID, limit)
	if err != nil {
		return nil, fmt.Errorf("read memory history: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var desc []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var agent, sessionID, relatedID, mergedContent, resolvedAt sql.NullString
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.ProjectID, &e.RecordedAt, &e.Phase,
			&agent, &sessionID, &relatedID, &mergedContent,
			&e.Content, &e.Category, &e.Importance, &resolvedAt, &e.Source,
		); err != nil {
			return nil, fmt.Errorf("scan memory history: %w", err)
		}
		e.Agent = agent.String
		e.SessionID = sessionID.String
		e.RelatedID = relatedID.String
		e.MergedContent = mergedContent.String
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
