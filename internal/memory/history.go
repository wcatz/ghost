package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	sqlite "modernc.org/sqlite"
)

// The write paths that append history, as the phase vocabulary of
// memory_history.phase. The CHECK in initSQL (mirrored by migrateV17) is the
// authority; these constants exist so a writer cannot spell one wrong.
//
// They are deliberately about WHICH WRITE PATH CHANGED a row, not about what
// the row now says. `reflect` covers a consolidation rewrite, a reuse that
// restated a field and a fresh insert alike, because "which reflection run
// changed this" is the question the history exists to answer and the row's own
// source column already records the outcome. A re-emission that changed nothing
// writes no row at all (#727), so the question it cannot answer — which run last
// merely LOOKED at this memory — is deliberately not asked.
const (
	// phaseSave: a new memories row was inserted.
	phaseSave = "save"
	// phaseUpdate: an existing row was edited (UpdateMemory).
	phaseUpdate = "update"
	// phaseReflect: a consolidation CHANGED the row — a rewrite, a reuse that
	// restated at least one field, or a fresh insert. A verbatim re-emission
	// appends nothing (#727): it wrote nothing, and a version row records the
	// state a write left behind.
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

// allHistoryPhases is every phase the schema knows about, in the order the
// constants are declared.
//
// It exists for the two classifiers that are claims about CODE rather than rules
// about rows, and they need the full set to be checkable at all: `compactablePhases`
// says which phases the compaction may remove, and `stampMovingPhases` says which
// writers move a live memory's updated_at. Both are asked "what is not in here?",
// and a hand-written list of the remainder cannot answer it — a phase added to the
// schema and to neither list would simply be unclassified, which is the failure the
// compaction's allowlist exists to make impossible.
func allHistoryPhases() []string {
	return []string{
		phaseSave, phaseUpdate, phaseReflect, phaseMerge, phaseResolve, phaseUnresolve,
		phaseSupersede, phaseUnsupersede, phaseRestore, phaseImport, phaseDelete, phaseBaseline,
	}
}

// The growth policy for memory_history, applied inside the same transaction
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
// It was the identity function while internal/secret (#656) — the value-SHAPE
// detector every Ghost writer consults before it stores caller-supplied text —
// was still open, under a TODO naming that issue as the change that would
// install it. #656 has since landed, and history_redactor.go's init installs
// redactHistoryContent through setHistoryRedactor. The replacement, not a
// refusal, because refusing would fail the user's own write over something only
// the history can see.
//
// The cost is real and was measured rather than assumed, because the comment above
// this seam is right that crossing into Go per appended row lands on the write
// path's critical section: secret.Detect on a 2 KB memory is ~1.1 ms and at the
// 8 KB content cap ~3.6 ms, per row, inside the transaction. A batched append pays
// it once per id. It is paid on EVERY row whether or not anything is redacted,
// because the only sound way to know is to look. The obvious optimisation — a
// keyword pre-check that skips the detector — is the same trap #656's detector
// already documented once: a prefilter with a false negative is a silent leak, and
// LiteralPrefix returns empty for the \b-initial patterns these rules use, so a
// hand-written one cannot be made sound by inspection either.
//
// It is installed through setHistoryRedactor, never assigned directly, and the
// append statement asks THAT whether to call the SQL function — a second bool
// would be a second source of truth, and the wiring below would then have to set
// both or the filter would silently not apply.
//
// filter is nil until a redactor is installed, and the append path's gate is
// derived from it rather than kept beside it, so the two cannot disagree.
var historyRedactor struct {
	filter func(string) string
}

func historyRedactorInstalled() bool { return historyRedactor.filter != nil }

// historyContentExpr is the SQL expression a history row's content column reads:
// the filter when one is installed, the column itself when none is.
//
// Every statement that writes a history row reaches the content through here, and
// there are two of them (the batched append and the baseline's NOT EXISTS
// insert). They cannot each decide for themselves: one of them forgetting is
// exactly how an unredacted credential gets into the table while the append path
// claims to redact it.
//
// Calling the function when nothing is installed would cross into Go through the
// driver for every appended row to do nothing — measurable on the write path's
// critical section. Reading the gate instead is not defensive: it is the state
// #656's seam started in, with redactHistoryContent still the identity function
// and no redactor installed, and the state setHistoryRedactor(nil) still creates.
func historyContentExpr() string {
	if historyRedactorInstalled() {
		return historyContentFunc + "(content)"
	}
	return "content"
}

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
			if historyRedactor.filter == nil {
				return content, nil
			}
			return historyRedactor.filter(content), nil
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
// returns a function that restores the previous one. It is the ONLY way to
// install one, which is what keeps the append path's gate and the function it
// calls in step. The seam's own tests use it to swap a filter out (or install
// none), and history_redactor.go's init is the production call that installed the
// real one; the filter field itself has no other writer.
func setHistoryRedactor(fn func(string) string) func() {
	prev := historyRedactor.filter
	historyRedactor.filter = fn
	return func() { historyRedactor.filter = prev }
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
		if err := pruneHistoryTx(ctx, tx, batch); err != nil {
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
		INSERT INTO memory_history
			(memory_id, project_id, phase, agent, session_id, related_id, merged_content,
			 content, category, importance, resolved_at, source)
		SELECT id, project_id, ?, ?, ?, ?, ?, `+historyContentExpr()+`,
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
	// The check and the append are ONE statement, `WHERE NOT EXISTS`, rather than
	// a count followed by an insert. Two reasons, and the second is the load
	// bearing: the pair is atomic without relying on anything else in this
	// transaction, and it is one statement on the edit path instead of two —
	// which matters because every statement here runs while the write lock is
	// held, and the edit path is one of the busiest.
	//
	// A statement that inserts nothing reports zero rows affected, which is the
	// answer, not a failure: a memory this build wrote has history and keeps it.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_history
			(memory_id, project_id, phase, agent, session_id, content, category,
			 importance, resolved_at, source)
		SELECT id, project_id, ?, ?, ?, `+historyContentExpr()+`, category, importance,
		       resolved_at, source
		FROM memories
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM memory_history p WHERE p.memory_id = memories.id)
	`, phase, nullIfEmpty(prov.Agent), nullIfEmpty(prov.SessionID), memoryID)
	if err != nil {
		return fmt.Errorf("record baseline history: %w", err)
	}
	if _, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("record baseline history rows: %w", err)
	}
	return nil
}

// purgeHistoryTx erases the text this database recorded about a memory, and
// reports how many history rows it removed.
//
// It is the redaction path, and it reaches further than the history table alone
// because the history is not the only place the text was kept:
//
//   - memory_snapshots. Every applied reflection copies each non-manual memory's
//     full content into a snapshot, and memory_snapshots.memory_id deliberately
//     has no foreign key, so deleting the memory row leaves the snapshot — and
//     `ghost reflect --restore` re-inserts the row from it under the memory's
//     ORIGINAL id, text and all, with no history to show it came back. A purge
//     that left that behind would report success on a secret that is one
//     `ghost reflect --restore` away from being readable again.
//   - another memory's merge row. A FoldOnly fold records the wording it
//     discarded in `merged_content` on the TARGET's row, so this memory's text
//     can sit on a history row that names a different memory. Those cells are
//     redacted rather than their rows deleted: the event (which memory, when,
//     which agent) is worth keeping, and the text is what the purge is for.
//   - memory_provenance and memory_snapshot_evidence. The evidence records are
//     deleted outright rather than kept as a record: they carry no text of the
//     memory, but they name the agents, sessions and references that reported it,
//     and the reason a purge was asked for is this memory, not the row that quotes
//     it. The second table is the same columns a second time, held beside the
//     snapshot a restore would read, and a snapshot outlives the purge by being the
//     rollback point for every OTHER memory in the project.
//
// The texts are collected before anything is deleted, because afterwards there
// is nothing left to search for. The caller runs this in the same transaction as
// the memory DELETE, so a memory and every copy of it commit or roll back
// together.
//
// An empty memoryID is REFUSED here rather than at the exported entry point, and
// here rather than there because there are two callers (PurgeMemoryHistory and
// the delete-time purge) and only this body is both of them. The reason is the
// substring search the retrieval-record delete below performs: SQLite's instr
// returns 1 for an EMPTY needle — measured, not assumed — so an empty id matches
// every row in that table and the purge would delete the whole audit trail while
// the equality-keyed deletes around it matched nothing and reported success. A
// refusal costs a caller a check; the alternative costs an operator the evidence.
func purgeHistoryTx(ctx context.Context, tx *sql.Tx, memoryID string) (int64, error) {
	if memoryID == "" {
		return 0, errPurgeNoMemoryID
	}

	texts, err := selectIDs(ctx, tx, `
		SELECT content FROM memory_history WHERE memory_id = ? AND content IS NOT NULL
		UNION
		SELECT content FROM memories WHERE id = ?
		UNION
		SELECT content FROM memory_snapshots WHERE memory_id = ?`,
		memoryID, memoryID, memoryID)
	if err != nil {
		return 0, fmt.Errorf("collect texts to purge: %w", err)
	}

	// The snapshots first: they are the copies that can bring the row back, so
	// they are the ones whose survival would make the purge's own report a lie.
	//
	// By CONTENT as well as by id, because a pre-v13 snapshot recorded no id and
	// `ghost reflect --restore` matches those by content — so an id-keyed delete
	// would leave the one snapshot that can resurrect the row, and this purge
	// would report success on a secret one restore away. The content list is the
	// memory's own, so this cannot reach an unrelated row that merely shares a
	// word: it is an exact match on text this memory is being erased for.
	const snapshotPurgeBatch = 25
	for start := 0; start < len(texts); start += snapshotPurgeBatch {
		batch := texts[start:min(start+snapshotPurgeBatch, len(texts))]
		placeholders := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch))
		for _, text := range batch {
			if text == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, text)
		}
		if len(placeholders) == 0 {
			continue
		}
		list := strings.Join(placeholders, ",")
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM memory_snapshots
			 WHERE memory_id = ? OR content IN (`+list+`)`,
			append([]interface{}{memoryID}, args...)...,
		); err != nil {
			return 0, fmt.Errorf("purge memory snapshots: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_snapshots WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge memory snapshots: %w", err)
	}

	// The evidence records go with the history, and the delete is explicit rather
	// than left to the foreign key. The cascade covers the delete-time purge, but
	// PurgeMemoryHistory leaves the memory in place, so no cascade fires there —
	// and a purge whose completeness depended on a connection's foreign_keys
	// pragma would be a purge that can report success over rows still in the
	// table. These rows name the agents and sessions that reported a fact about a
	// memory whose text is being erased; keeping them would leave the erased
	// memory's story behind with the memory itself gone.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_provenance WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge memory evidence: %w", err)
	}
	// And the copy a snapshot holds. The snapshot row itself goes above, by id and
	// by content, but a snapshot is the rollback point for a whole PROJECT's
	// corpus, so the evidence rows are keyed on the memory alone and would outlive
	// the purge by however many reflects it takes to prune that snapshot — or
	// forever if none runs. A redaction that leaves the agent, the session and the
	// reference behind has not erased the thing it was asked to erase, whatever it
	// reports.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_snapshot_evidence WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge snapshot evidence: %w", err)
	}

	// And the retrieval records that NAME the memory (#646). It holds no text of
	// it, but a name is exactly what a redaction is asked to remove when the id
	// itself is the sensitive thing — a memory named for a customer, a
	// credential's identifier, an incident ticket. Leaving the row would report
	// success on a redaction that the audit can still read the id back out of.
	//
	// There is no foreign key to cascade: a record outlives the memory by
	// construction (it is evidence about calls, and a purge deletes history, not
	// memories), so this reach has to be written out, exactly as the two above
	// are.
	//
	// TWO predicates, because one of them cannot see every row. The parsed arm
	// goes through readableVerdicts and matches every WELL-FORMED record EXACTLY,
	// by the id the stages actually recorded. The textual arm is the reach the
	// parsed one does not have: a row whose verdicts column cannot be read at all
	// may still name the memory, and a name inside it is still a name.
	//
	// SCOPED to exactly those rows, and the scope is what makes the arm defensible
	// rather than merely cautious. verdictsUnreadable is the same expression
	// readableVerdicts substitutes on, so "the parsed arm could not see this row"
	// and "this column is unreadable" are one statement in one place. An UNSCOPED
	// OR also fuzzy-matches every well-formed row: purging the id `A_1` would
	// delete a record whose verdict names `A_1%done`, which the parsed arm — which
	// knows the exact ids — correctly left alone. Having the exact answer and then
	// searching for it approximately is strictly worse than not having it, so the
	// arm is gated on the very rows that need it.
	//
	// instr, NOT LIKE: LIKE reads % and _ in the BOUND VALUE as wildcards, and
	// `ghost import` writes an artifact's ids verbatim, so a real id can contain
	// one — the id `A_1%done` would match the unrelated `Ax1%done` row. instr is a
	// literal substring search (measured on this build's SQLite: it finds `A_1` in
	// `[{"id":"A_1"}]` and does NOT find it in `[{"id":"Ax1"}]`, which LIKE does).
	//
	// Matched in the VALUE position, and that is the whole fix. A bare
	// `instr(verdicts, ?)` searches the SERIALIZED DOCUMENT, which contains the
	// schema's own field names and vocabulary — "id", "kept", "stage", "reason",
	// "valid", "expired" — so an imported id equal to or contained in any of those
	// matched nearly every row and `ghost history purge` deleted the audit trail
	// while reporting that it erased one memory. Requiring the id to sit behind
	// its own key, and quoting it the way the column quotes it, makes a field name
	// unable to match: no document contains `"id":"id"` unless a verdict's id is
	// literally the string "id", which is a real memory being purged.
	//
	// The residual, stated rather than hidden: json_quote and Go's encoder escape
	// differently for `<`, `>` and `&` (Go writes \u003c by default), so an
	// UNREADABLE row whose id contains one of those is not reached by this arm. It
	// is a gap in a best-effort arm for malformed rows, not in the parsed one,
	// which compares ids exactly and covers every row Ghost wrote.
	//
	// TWO statements, ONE predicate (#852). The verdicts filed against the calls
	// this removes go WITH them, so the purge is keyed on the CALL that is going
	// away rather than on the memory that named it. A call that admitted MEM1 and
	// MEM2 loses its whole record row when MEM1 is purged, and its (call, MEM2)
	// verdict is that call's own: the `memory_id = ?` arm below reaches MEM1's
	// verdict and nothing else, so the rest survived pointing at a row that no
	// longer existed — and retrieval_record has no AUTOINCREMENT, so the freed
	// rowid was handed to the NEXT call, which inherited a verdict about a memory
	// it never admitted. So the sweep SELECTs the doomed rowids with exactly the
	// predicate the record delete uses; retrievalRecordNamingMemory is one function
	// for both, because two copies of this text could drift into removing the
	// record rows and keeping their verdicts, which is the orphan the pairing
	// exists to prevent.
	//
	// The sweep comes FIRST, and that order is load-bearing rather than a style
	// choice: its predicate is a subquery over retrieval_record, so once those rows
	// are gone it can no longer see them and would silently match nothing.
	//
	// Cost, since there is no index here to hide it behind: one scan of
	// retrieval_audit — what the report's own read costs anyway, on a table
	// bounded at retrievalAuditRowsCap. TestRetrievalAuditsCarryOneIndex forbids an
	// index on record_rowid because every insert would pay for it inside the write
	// lock and nothing reads it but these sweeps, so the alternative of collecting
	// the doomed rowids in Go costs the same scan plus a statement per batch.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retrieval_audit WHERE record_rowid IN (
			SELECT rowid FROM retrieval_record WHERE `+retrievalRecordNamingMemory()+`)`,
		memoryID, memoryID); err != nil {
		return 0, fmt.Errorf("purge the verdicts of the purged calls: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retrieval_record WHERE `+retrievalRecordNamingMemory(),
		memoryID, memoryID); err != nil {
		return 0, fmt.Errorf("purge retrieval records: %w", err)
	}

	// The audit's rows are REDACTED, not left behind, and for the same reason the
	// record arm above deletes rather than redacts: a verdict's subject is a memory
	// ID, and a purge that removed the memory while its verdicts kept naming it
	// would leave a report counting judgments about a memory that no longer exists.
	//
	// This arm is not redundant with the sweep above, and the difference is what
	// each is for: the sweep reaches the verdicts of calls that are going away,
	// this one reaches the verdicts whose CALL SURVIVES — every unattributed row
	// (record_rowid 0, which the replacement refuses to treat as a key) and any
	// row whose call judged the memory without naming it. A plain equality on a
	// real column rather than a scan of a JSON document, which is the same cost the
	// table's cap makes free: a redaction is rare and the table is bounded.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retrieval_audit WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge retrieval audits: %w", err)
	}

	// The flags go too, and this arm is NOT redundant the way a cascade would be:
	// a purge keeps the memory row (that is what makes it a purge rather than a
	// delete), so memory_flags' ON DELETE CASCADE never fires. A flag left behind
	// would survive its own memory's redaction — an agent's objection, with its
	// reason, still naming a memory whose recorded past the caller just paid to
	// erase, and still counting as evidence against content that may be exactly
	// what was purged. The delete is by memory_id because that is the id this
	// transaction is purging, and it runs in the same transaction for the reason
	// every statement here does: a purge that took the history and left the flags
	// would report success over the rows it was asked to remove.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_flags WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge memory flags: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM memory_history WHERE memory_id = ?`, memoryID)
	if err != nil {
		return 0, fmt.Errorf("purge memory history: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count purged history rows: %w", err)
	}

	// Another memory's discarded wording, in batches small enough that the
	// statement's text stays sane: a text is up to MaxContentLen bytes, and a
	// memory's history is bounded but not by this.
	const redactionBatch = 25
	for start := 0; start < len(texts); start += redactionBatch {
		batch := texts[start:min(start+redactionBatch, len(texts))]
		placeholders := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch))
		for _, text := range batch {
			if text == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, text)
		}
		if len(placeholders) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE memory_history SET merged_content = ?
			 WHERE memory_id <> ? AND merged_content IN (`+strings.Join(placeholders, ",")+`)`,
			append([]interface{}{purgedTextMarker, memoryID}, args...)...,
		); err != nil {
			return 0, fmt.Errorf("redact folded-in copies of the text: %w", err)
		}
	}
	return removed, nil
}

// errPurgeNoMemoryID reports a purge asked for with no memory to purge. See
// purgeHistoryTx: the retrieval-record delete reaches an unreadable verdicts
// column by substring, and an empty id is a substring of everything.
var errPurgeNoMemoryID = errors.New("purge history: a memory id is required")

// purgedTextMarker replaces a folded-in text a purge has erased. It is a marker
// rather than an empty string so a reader can tell "this fold discarded text that
// has since been purged" from "this fold discarded nothing", which an empty
// string cannot say.
const purgedTextMarker = "[purged]"

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
// It erases recorded text only, which is what distinguishes it from
// DeleteWithOptions(PurgeHistory: true) — that one is a delete and takes the row
// with it. A live memory row here is left exactly as it was,
// because "erase the history of this memory" and "delete this memory" are
// different requests and conflating them would destroy knowledge nobody asked to
// lose. What it does reach is every copy of the text the database kept, so
// nothing is left that a restore or a reader can bring back; see purgeHistoryTx.
func (s *Store) PurgeMemoryHistory(ctx context.Context, memoryID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "purge-history")
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
		UPDATE memory_history
		SET related_id = ?
		WHERE memory_id = ? AND phase = ? AND related_id IS NULL
	`, successorID, oldID, phaseDelete); err != nil {
		return fmt.Errorf("record history successor for %s: %w", oldID, err)
	}
	return nil
}

// pruneHistoryTx applies the growth policy in the caller's transaction: the
// per-memory cap for the ids just appended, and the table-wide cap.
//
// Both bounds are asked about in ONE statement, because this runs inside the
// caller's write transaction and everything it adds is time the write lock is
// held. The write lock is the single resource concurrent writers queue for, so a
// per-upsert statement that looks free in a one-process test is what tips a
// busy_timeout under contention — which is exactly how this landed: the
// multi-process test in multiproc_concurrency_test.go began failing with
// SQLITE_BUSY at BEGIN IMMEDIATE once each write carried three extra statements.
//
// The probe therefore reports the two questions in one union (which of these
// memories are over the per-memory cap, and the newest rowid in the table), and
// the DELETEs behind those answers are skipped entirely in the common case: a
// memory under the cap and a table under the global cap are the normal state, and
// then nothing is deleted.
func pruneHistoryTx(ctx context.Context, tx *sql.Tx, ids []string) error {
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

		// kind 'over' carries the memory ids past the per-memory cap; kind 'max'
		// carries the newest rowid as text, since the two answers have different
		// types and one result set carries both.
		rows, err := tx.QueryContext(ctx, `
			SELECT 'over', memory_id FROM memory_history
			WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
			GROUP BY memory_id
			HAVING count(*) > ?
			UNION ALL
			SELECT 'max', COALESCE(CAST(max(rowid) AS TEXT), '0') FROM memory_history
		`, args...)
		if err != nil {
			return fmt.Errorf("probe history growth: %w", err)
		}
		var (
			over   []string
			newest int64
			sawMax bool
		)
		for rows.Next() {
			var kind, value string
			if err := rows.Scan(&kind, &value); err != nil {
				rows.Close() //nolint:errcheck
				return fmt.Errorf("scan history growth probe: %w", err)
			}
			switch kind {
			case "over":
				over = append(over, value)
			case "max":
				v, convErr := strconv.ParseInt(value, 10, 64)
				if convErr != nil {
					rows.Close() //nolint:errcheck
					return fmt.Errorf("parse newest history rowid %q: %w", value, convErr)
				}
				newest, sawMax = v, true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return fmt.Errorf("read history growth probe: %w", err)
		}
		rows.Close() //nolint:errcheck

		// Per memory: everything past the newest historyVersionsPerMemory rows,
		// ranked by rowid. rowid rather than recorded_at because recorded_at is
		// second-precision — every write one reflection makes in a pass shares a
		// timestamp, and the newest rows would then be chosen by a tie-break.
		// Ranking by rowid DESC also means the predicate can never take a memory's
		// newest row: it is rank 1.
		if len(over) > 0 {
			overPlaceholders := make([]string, 0, len(over))
			overArgs := make([]interface{}, 0, len(over)+1)
			for _, id := range over {
				overPlaceholders = append(overPlaceholders, "?")
				overArgs = append(overArgs, id)
			}
			overArgs = append(overArgs, historyVersionsPerMemory)
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM memory_history
				WHERE rowid IN (
				    SELECT rowid FROM (
				        SELECT rowid, ROW_NUMBER() OVER (
				                   PARTITION BY memory_id ORDER BY rowid DESC) AS rank
				        FROM memory_history
				        WHERE memory_id IN (`+strings.Join(overPlaceholders, ",")+`)
				    )
				    WHERE rank > ?
				)`, overArgs...); err != nil {
				return fmt.Errorf("cap history rows per memory: %w", err)
			}
		}

		// Across the store: a rowid window rather than an ORDER BY over every row.
		// An implicit rowid is max(rowid)+1 and is never reused, so rowid order IS
		// insertion order, and after `rowid <= newest - cap` every surviving rowid
		// lies in a window of cap integer values — at most cap rows, whatever the
		// gaps. The per-memory cap cannot do this job: every created-then-dropped
		// memory is its own memory_id with a couple of rows, and one applied
		// reflection can churn the whole non-manual corpus.
		if sawMax && newest > int64(historyRowsCap) {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM memory_history WHERE rowid <= ?`, newest-int64(historyRowsCap),
			); err != nil {
				return fmt.Errorf("cap history table size: %w", err)
			}
		}
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
		FROM memory_history
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
