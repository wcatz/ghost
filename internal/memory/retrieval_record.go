package memory

// #646 part 1: the retrieval record — what a retrieval call RETRIEVED and what it
// KEPT, one row per call.
//
// The grain is the load-bearing decision. The question this table exists to
// answer is "did the agent use what Ghost injected", and the denominator of that
// ratio is CALLS. A row per (call, memory) pair records only the calls that
// happened to admit something, so the calls worth auditing — the ones that
// returned nothing — are the ones with no rows at all, and a report built over
// them cannot see the failures it is looking for.
//
// What a row may hold is decided by the issue's own constraint: verdicts and
// ids, no text. That is a property of the TYPE, not of this file's discipline —
// RowVerdict has no field a query or a memory's content could be put in, and
// QueryHash is a digest — so a later writer cannot widen it by accident. The
// one field that is text-shaped and free is Reason, which is drawn from the
// assembler's own reason vocabulary, never from a caller's string.
//
// Three operations are deliberately absent. `ghost backup` includes the table
// (it is a VACUUM INTO of the whole file, and a record is a fact about a past
// call that a restore should not silently lose); `ghost export`/`import`
// EXCLUDE it (like the embeddings, the links and the resolve cache, it is
// derived operational data about calls on the machine it was made on, and it
// describes memories that a portable artifact is not obliged to carry); and
// `ghost history purge` DELETES the rows naming the purged memory (the same
// reason it deletes memory_provenance — a redaction is asked to remove a NAME,
// and this table keeps one).

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

// RowVerdict is one memory's fate in one call: kept or dropped, and the stage
// and reason that decided it.
//
// The four fields are the whole vocabulary, and they are read rather than
// re-derived by whatever consumes the record. A reason a consumer recomputed
// would be a second implementation of the assembler's rules free to drift until
// a report contradicted it, which is the failure Result.Leaks() exists to
// prevent inside a run and which nothing here can prevent across one.
//
// A kept row may carry a reason (the assembler records `validity_unparseable`
// on a row it admitted anyway) and a dropped row always does. The JSON tags are
// the STORED shape, pinned by TestRowVerdictJSONShape, because the next part's
// reader and any future SQL reaching into this column are both written against
// it and a renamed field is silent data loss rather than a compile error.
type RowVerdict struct {
	ID     string `json:"id"`
	Kept   bool   `json:"kept"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
}

// RetrievalRecord is one retrieval call, as the audit will read it back.
//
// There is no Content and no Query. The caller's question is a digest
// (QueryHash, computed by the assembler, which never sees the store's clock and
// never stores text), the memories it touched are ids, and the only free text is
// the reason vocabulary the assembler itself produced.
type RetrievalRecord struct {
	ProjectID string
	// RowID is the record's own rowid, and it is what a verdict is filed against.
	// It is read rather than inferred, because the verdict's replacement has to
	// name the exact call it supersedes: recorded_at is second-precision so two
	// calls in one second have no defined order by it, and a record's position in
	// a read window moves as older calls are evicted.
	RowID int64
	// SessionID is "" over the shipped stdio transport, which reports no
	// session. That is why Source is a first-class column rather than something
	// inferred: a session-start injection and a search are indistinguishable by
	// session on the transport Ghost actually serves.
	SessionID string
	Source    string
	QueryHash string
	// AsOf is the instant a historical read was assembled at, RFC 3339, and
	// empty for a current one. A record with no as_of cannot be told from a
	// block assembled against the wall clock, and the two have different reasons
	// to be audited.
	AsOf    string
	Outcome string
	Reason  string
	// Verdicts is every row this call judged, kept and dropped alike. A row the
	// retrieval window CUT was never judged and is not here: the record is what
	// the stages decided, not what the retriever happened to return.
	Verdicts []RowVerdict
	// RecordedAt is the store's own clock, stamped by the writer and read back
	// by the reader. A caller does not set it: the assembler binds Now for its
	// own decisions and the store's clock is the only one that says when the row
	// was durably written, which is the instant an audit places against a
	// transcript.
	RecordedAt string
}

// readableVerdicts wraps a verdicts column expression in the one form of it that
// the purge's PREDICATE can always read, for a predicate that has to survive a
// column it cannot parse. A reader decides for itself (see RetrievalRecords,
// where Go settles the same cases in one decode); this is for the SQL that
// cannot.
//
// It guards TWO raises, and they are raised by DIFFERENT functions — measured on
// the SQLite 3.53.4 this build links, one query per case:
//
//	value             json_each(v)            json_each(v) WHERE value->>'id' = 'A'
//	[{"id":"A"}]      1 row                   1 row
//	{"id":"X"}        1 row  (it WALKS it)    RAISE
//	not json at all   RAISE                   RAISE
//	null              1 row                   0
//	[]                0 rows                  0
//	'"s"'             1 row                   0
//
// So json_each walks a valid JSON object quite happily and only refuses a
// document that is not JSON; what raises on the object is the ->> PATH ACCESSOR,
// applied to the value json_each yielded. Both have to be closed, and the two
// terms close them separately: json_valid() closes the first and json_valid() +
// json_type() = 'array' closes the second, because an array is the only shape
// whose elements are objects a path can be taken through. Either term alone
// leaves a raise behind — dropping the json_type term is caught by
// TestPurgeToleratesARowWhoseVerdictsAreNotJSON's second fixture, which is
// exactly the object row.
//
// Substituting an empty array for anything unreadable is the only form that
// covers both, and it is a function of json_valid and json_type (both total —
// they answer rather than raise) so the choice is made before either raising
// function is called. An unreadable column then reads as "this call judged
// nothing", which is the honest reading of a row no build of Ghost wrote.
func readableVerdicts(col string) string {
	return "CASE WHEN NOT COALESCE(" + verdictsUnreadable(col) + ", 0) THEN " + col + " ELSE '[]' END"
}

// verdictsUnreadable is the ONE definition of "the JSON functions cannot walk
// this column", and both the substitution above and the purge's textual arm's
// SCOPE are built from it.
//
// They have to agree, and that is the whole reason this is a function: the arm
// exists only for rows the parsed arm cannot see, so if the two disagreed about
// what "cannot see" means, the arm would either never run or — the failure mode
// that is easy to write — run against every row and turn an exact match into a
// fuzzy one for the records the parsed arm had already decided about.
//
// It is written to be TOTAL, which is not free. json_valid never raises; json_type
// RAISES on a document it cannot parse (measured on this build's SQLite), so a
// plain `json_valid(c) = 0 OR json_type(c) <> 'array'` only works by relying on
// AND/OR short-circuiting, and SQLite does not promise the order it evaluates
// the terms of a WHERE clause in — an expression that read correctly in a CASE's
// WHEN raised as soon as it appeared in a WHERE. So the inner json_type is handed
// a CASE that yields the column only once json_valid has accepted it, and
// COALESCE covers the NULL it gets otherwise. Every arm of every case, measured:
//
//	[{"id":"A"}]  readable      {"id":"X"}    unreadable
//	[]            readable      not json at all  unreadable
//	              null         unreadable
func verdictsUnreadable(col string) string {
	return "(json_valid(" + col + ") = 0 OR COALESCE(json_type(CASE WHEN json_valid(" +
		col + ") = 1 THEN " + col + " END), 'not-an-array') <> 'array')"
}

// retrievalRecordNamingMemory is the ONE predicate that answers "does this
// recorded call name this memory", and it returns it as SQL with the memory's id
// bound TWICE — once per arm.
//
// It is a function because two statements now need it and must not be able to
// disagree: `ghost history purge` deletes the record rows by it, and deletes the
// verdicts filed against exactly those rows by SELECTing their rowids with it
// (#852). Two copies of this text would drift, and the drift would be silent in
// the exact direction that matters — a record row removed and its verdicts kept,
// which is the orphan the whole arrangement exists to prevent. See
// purgeHistoryTx for what each arm reaches and why the textual one is scoped.
func retrievalRecordNamingMemory() string {
	return `EXISTS (
			SELECT 1 FROM json_each(` + readableVerdicts(`retrieval_record.verdicts`) + `)
			WHERE value->>'id' = ?
		) OR (COALESCE(` + verdictsUnreadable(`retrieval_record.verdicts`) + `, 0)
		     AND instr(retrieval_record.verdicts, '"id":' || json_quote(?)) > 0)`
}

// retrievalRecordKeepingMemory is the ONE predicate that answers "is the call on
// this rowid one that KEPT this memory", and it is the audit write's guard against
// filing a verdict under a call that never admitted the memory's subject.
//
// It is not the same question as retrievalRecordNamingMemory above, which asks
// whether the call MENTIONS the memory at all — that one serves a purge, whose
// polarity is to MISS nothing, so it carries a fuzzy textual arm for rows whose
// verdicts JSON cannot be walked. This one serves a report, whose polarity is to
// claim nothing it cannot support: an unreadable verdicts column substitutes an
// empty array (readableVerdicts), so a call Ghost cannot read about itself is a
// call that kept nothing, and a verdict filed under it would be a claim no build
// can check. A hole is the honest answer; the alternative is the exact defect
// #852 exists to remove.
//
// `kept = 1` and not merely "the id is present", because a record carries
// DROPPED verdicts too: a call that considered a memory and did not admit it must
// not be credited with what the agent did with it. Every verdict Ghost files comes
// from a kept one (audit.Run reads rec.Verdicts and skips `!v.Kept`), so this
// refuses nothing the real caller can produce.
//
// NUMERIC, not `= '1'` and not `= 'true'`. `->>` returns a JSON boolean as the
// INTEGER 1 with typeof 'integer' — it does not render the token — so both of those
// compare an integer against text, never match, and silently reduce this predicate
// to "never". Measured on this build's SQLite (3.53.4): `value->>'kept' = 1`
// answers 1 for a kept verdict and 0 for a dropped one, while `= '1'` answers 0
// for both. `value->>'id' = ?` above binds TEXT against TEXT and has no such
// hazard, which is what makes the difference easy to miss.
func retrievalRecordKeepingMemory() string {
	return `EXISTS (
			SELECT 1 FROM json_each(` + readableVerdicts(`retrieval_record.verdicts`) + `)
			WHERE value->>'id' = ? AND value->>'kept' = 1)`
}

// retrievalRecordRowsCap bounds the table as a whole, oldest row first.
//
// The cap is measured in ROWS, which is what the eviction below enforces: it
// deletes by rowid, so the table settles at the cap however its rows got there.
// Calls and rows stopped being the same unit when #850 gave the project-context
// surfaces a second read for their Global section, since one call of
// `ghost://project/{id}/context` or `recall_project` then writes two rows, and
// #581's `ghost://memories/global` is a fourth reader of that same seam. So the
// bound is a few weeks of searching for a store whose calls mostly write one row,
// and a SHORTER window in calls for a store whose project-context reads
// dominate, because each of those spends two of them. Which of the two an
// operator has is a fact about their own traffic, and the policy the bound
// exists for is the same either way: a store that runs past it loses the OLDEST
// evidence rather than the newest, so what survives is the window an operator is
// actually looking at.
//
// It is a var because the cap's policy is what the tests exercise, and a test
// that cannot lower the bound cannot assert the eviction without writing 5000
// rows.
var retrievalRecordRowsCap = 5000

// errRetrievalNoProject reports a record with no project to attribute it to.
//
// A refusal rather than a defaulted row: the report's first question is "which
// project's retrieval was this", and a row filed under nothing answers it as
// "all of them", which is a claim about a call nothing recorded.
var errRetrievalNoProject = errors.New("record retrieval: a project id is required")

// lockRecordWrite takes the store's write lock, or gives up when ctx is done.
//
// sync.RWMutex.Lock ignores a context, so a sub-deadline does NOT bound this step
// on its own: a record write queued behind another writer's long transaction would
// wait for the hand-over however long the caller had allowed, and the wait sits
// between the caller receiving its answer and the tool returning. That is the
// in-process half of the same problem the database half has, and it needs its own
// answer.
//
// TryLock in a short poll is the bounded acquire available without restructuring
// the store's locking, and the poll costs nothing in the ordinary case because the
// first attempt succeeds — a record write is not usually queued. Giving up is
// correct rather than merely defensive: the row is a measurement of a call that
// already happened, and the caller logs the loss.
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

// recordLockPoll is how often a contended record write re-tries the store's lock.
// Short enough that the budget is honoured to within a millisecond, long enough
// that a queue of searches is not spinning a core while it waits.
const recordLockPoll = time.Millisecond

// RecordRetrieval appends one call's record.
//
// The append and the cap's eviction share one transaction, so the table cannot
// exceed its bound even transiently — the same reason memory_history's prune
// rides along with its append, and the same reason the two statements are here
// rather than in a background pass. The INSERT reports its own rowid, so the
// common case costs two statements and the over-cap case three: nothing is
// probed when the row just written cannot have crossed the bound.
//
// It is bounded THREE times, and the third bound is the one that used to be
// missing. The caller's context bounds the wait for the store's own mutex
// (lockRecordWrite, because a sync.RWMutex ignores a context) and bounds the wait
// for a free connection. Neither touches the wait when ANOTHER PROCESS holds the
// write lock: that wait is SQLite's busy handler, a sleep loop inside the
// driver's C call, which no context can interrupt. The pool is opened with
// busy_timeout(5000), so a BEGIN IMMEDIATE spent five seconds inside SQLite and
// came back SQLITE_BUSY — and because this path also inherits beginWrite's one
// bounded retry, ten. A search that had already been answered then took ten
// seconds to return.
//
// So this write does not use beginWrite. It pins one connection, asks that
// connection for a busy_timeout derived from the caller's own deadline, and
// restores the value it found afterwards. busy_timeout is per-connection, which
// is why the pool's single connection is pinned for the whole transaction rather
// than set with a bare Exec: an unpinned PRAGMA could land on a connection the
// transaction does not use, and the bound would silently not apply. The
// store-wide five seconds is NOT changed — every other write keeps it.
//
// The write is refused, not merely discouraged, on a store a newer Ghost owns
// (#746). A retrieval record is the audit trail of what an agent was told, and
// a stale server's records are exactly the ones that lie — the store may since
// have gained a stage, a validity rule or a retention tier this build's verdict
// vocabulary cannot name. Recording into it would be the unaudited write the
// refusal exists to stop, and the error matches ErrStoreNewer so a caller can
// tell it from a lock contention it could retry.
func (s *Store) RecordRetrieval(ctx context.Context, rec RetrievalRecord) error {
	if rec.ProjectID == "" {
		return errRetrievalNoProject
	}
	// A nil slice marshals to "null", which is a JSON document json_each reads
	// as a scalar rather than an array — so the empty call (the one a report most
	// needs) would store a shape nothing downstream expects. [] is the array
	// with no members, and is the same fact.
	verdicts := rec.Verdicts
	if verdicts == nil {
		verdicts = []RowVerdict{}
	}
	verdictJSON, err := json.Marshal(verdicts)
	if err != nil {
		return fmt.Errorf("record retrieval: encode verdicts: %w", err)
	}

	// Bounded, because this runs on the search path and a wait here is a wait the
	// caller pays for a measurement it has already been given. See lockRecordWrite.
	if err := lockRecordWrite(ctx, &s.mu); err != nil {
		return fmt.Errorf("record retrieval: store is busy: %w", err)
	}
	defer s.mu.Unlock()

	// One pinned connection for the whole transaction, with this write's own
	// busy_timeout. See the comment above: the store-wide five seconds is what
	// this path must not inherit, and it is restored before returning.
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

	// Oldest first, by rowid: recorded_at is second-precision, so two calls
	// inside one second have no defined order by it, and the rowid is the
	// insertion order. The bound is the row just written, so the table settles at
	// the cap rather than cap+1 — a window of cap-1 after a prune is not the
	// documented policy.
	if rowid > int64(retrievalRecordRowsCap) {
		// The verdicts filed against the calls this evicts go WITH them (#852),
		// and they go by the SAME bound rather than by a memory id, because the
		// two tables number their rows in one space: a verdict names record_rowid
		// exactly, so the rows about to go are exactly the ones at or below the
		// bound. Before this, an evicted call's verdicts survived it and were
		// counted by RetrievalAudits — the report's denominator — under a call the
		// store no longer holds.
		//
		// `record_rowid > 0` is load-bearing and not decoration. Zero is the
		// deliberate "not attributable to a call" value, the bound is >= 1
		// whenever this arm runs, and a bare `<= ?` would therefore delete every
		// unattributed verdict in the table the first time the cap evicted
		// anything — the one verdict shape the replacement refuses to treat as a
		// key.
		//
		// What it costs, measured on this build's driver against a store at both
		// caps (5000 record rows, 50000 verdicts, a 7.5MiB audit table): ~5ms per
		// recorded call. retrieval_audit carries one index and it is not on
		// record_rowid — TestRetrievalAuditsCarryOneIndex refuses the second one —
		// so this is a sequential scan, and a delete keyed by record_rowid is
		// O(rows stored) in this schema whichever transaction runs it. It is NOT
		// amortised over the eviction, and it must not be read as though it were:
		// retrieval_record has no AUTOINCREMENT, so its rowid grows monotonically
		// and this arm runs on EVERY insert past the cap, not only on the one that
		// first crossed it. What the steady state evicts is exactly one record row
		// and the verdicts filed against it.
		//
		// What bounds the scan is the audit table's own size: empty for a store
		// that has never judged a call, where it measures ~16us — and a
		// `SELECT 1 FROM retrieval_audit LIMIT 1` existence guard in front of it
		// costs ~10us there, which is why there is no guard. At the other end
		// that ~5ms is ~2% of the assembler's 250ms recordWriteBudget, and it is
		// not a scan the product did not already pay: the audit pass spends an
		// identical one per distinct record_rowid when it replaces a pass's
		// verdicts (RecordRetrievalAudits). What this changes is which transaction
		// pays it.
		//
		// Scoping the sweep to the evicted calls' own project, so the one index
		// this table has carries it, was measured and not adopted: it turns the
		// steady state into ~2.4ms on a six-project store and ~10.6ms on a
		// single-project one, against ~5ms for the unscoped scan either way. The
		// index makes the DELETE seek, but it has to visit every row of the
		// project to reach the one column it does not carry, so a store with one
		// project — the case where every row is a candidate — pays twice. The
		// unscoped form is the one whose cost does not depend on how the store's
		// rows are divided up.
		//
		// Why it is here and not in that pass: a pairing deferred to the next
		// judged turn has no bound. A store that stops judging keeps the orphan
		// forever, and until it goes the report counts verdicts under a call the
		// store no longer holds, which is the whole of what #852 is about. The
		// window closes at commit or not at all.
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

// close restores the store's write budget and releases the pinned connection.
func (sw *scopedWrite) close(s *Store, op string) { s.restoreScopedBusyTimeout(sw.restore, op) }

// retrievalBusyTimeoutFloor is the busy_timeout a scoped retrieval write uses
// when the caller gave no deadline to derive one from.
//
// It is a floor rather than the only number: it is also the CEILING, so a caller
// that handed this write a minute of budget does not get a minute of waiting for
// a row that is a measurement of a call that has already been answered. The
// assembler's budget is 250ms and this is below it, so the two cannot drift into
// a state where the pragma outlasts the context that asked for it.
const retrievalBusyTimeoutFloor = 150 * time.Millisecond

// retrievalBusyTimeoutMargin is what the scoped timeout leaves of the caller's
// deadline for the statements themselves and for restoring the pragma. Without it
// the wait could consume the entire budget and the caller would see its context
// expire before the transaction was finished — bounded, but by the wrong thing.
const retrievalBusyTimeoutMargin = 40 * time.Millisecond

// scopedWrite is one write transaction on a connection whose busy_timeout is this
// write's own, and the value to put back when it is done.
type scopedWrite struct {
	tx      *sql.Tx
	conn    *sql.Conn
	restore func() error
}

// beginScopedWrite opens a write transaction on a PINNED connection whose
// busy_timeout is bounded by the caller's own deadline.
//
// Why the pin: busy_timeout is a property of a connection, and the pool holds
// exactly one (MaxOpenConns(1)). Setting the pragma through s.db would put it on
// whichever connection the pool handed out, and a pool is free to close an idle
// connection between that statement and the BEGIN — after which the transaction
// would run on a fresh connection at the store-wide five seconds and the bound
// would be quietly absent. Pinning makes the pragma and the transaction the same
// connection by construction.
//
// Why no retry: beginWrite makes one extra BEGIN attempt, which is right for a
// save — the caller's whole point is that the memory is not lost. Two attempts at
// a 150ms budget is 300ms, past the 250ms the assembler allows, and a record is
// the one write here whose loss is recoverable: the caller already has its
// answer, and a report with a hole in it beats a search that waits. A refusal is
// reported through the same seam as every other write, so the fleet's
// record-retrieval distribution still has the losing attempts in it.
func (s *Store) beginScopedWrite(ctx context.Context, op string) (*scopedWrite, writeLock, error) {
	start := time.Now()
	// Conn is context-aware, unlike the BEGIN that follows it, so a caller whose
	// budget is already spent fails here rather than inside SQLite.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, writeLock{}, err
	}
	// Read the value to put back rather than assuming the DSN's 5000: a test or a
	// future caller may have set its own, and restoring a number this function
	// hard-coded would silently change the store's write contract.
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

	// The DSN asks for BEGIN IMMEDIATE, so the write lock is taken here, before
	// the first statement, and this is the whole of the wait the budget governs.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		s.restoreScopedBusyTimeout(restorePragma(conn, previous), op)
		if isLockContention(err) {
			writeLock{wait: time.Since(start)}.reportLost(op)
		}
		return nil, writeLock{}, err
	}

	sw := &scopedWrite{tx: tx, conn: conn, restore: restorePragma(conn, previous)}
	// The newer-store refusal runs in the SAME transaction as the write it guards,
	// exactly as beginGuardedWrite does, and rolls back rather than commits.
	if err := s.checkStoreNotNewer(ctx, tx); err != nil {
		_ = tx.Rollback()
		s.restoreScopedBusyTimeout(sw.restore, op)
		return nil, writeLock{}, err
	}
	return sw, writeLock{wait: time.Since(start), took: time.Now()}, nil
}

// restoreScopedBusyTimeout puts the store's write budget back and says so when it
// cannot.
//
// This is the one error in the record write that outlives the call: every other
// failure is a lost row, which the caller already handles, but a connection left
// carrying this write's 150ms timeout shortens EVERY later write's budget for the
// life of the process. That shows up later as unrelated saves losing their lock,
// and it never names the record write that did it — so it is logged here, at the
// point where the cause is still known, rather than left to be inferred.
func (s *Store) restoreScopedBusyTimeout(restore func() error, op string) {
	if err := restore(); err != nil {
		s.logger.Warn("the retrieval record's scoped busy_timeout could not be restored; "+
			"this connection may carry a shorter write budget for the rest of the process",
			"op", op, "error", err)
	}
}

// restorePragma returns the connection's busy_timeout to what it was, and closes
// the pinned connection.
//
// It does not use the CALLER's context, because that context is the thing which
// may have just expired: the refused case below is one where the write ended
// because the caller's budget ran out, and a restore that inherited a dead context
// would depend on the driver tolerating one. This driver does (measured — a
// restore through an already-cancelled context still applied the pragma), so
// WithoutCancel is not what makes the property hold and the test does not pretend
// it is: it is here so the code does not have to be re-examined to find out.
//
// A connection left at a 150ms timeout would silently shorten every LATER write's
// budget for the life of the process, which is a far worse failure than the record
// this write lost. A failure to restore is returned so the caller can say so,
// because it is the one error here that outlives the call.
func restorePragma(conn *sql.Conn, previous int) func() error {
	return func() error {
		err := setScopedBusyTimeout(context.Background(), conn,
			time.Duration(previous)*time.Millisecond)
		_ = conn.Close()
		return err
	}
}

// setScopedBusyTimeout is the one statement in this file that is not a store write,
// and it is in its own function so the structural guard's exemption map can name
// it: a PRAGMA that sets the connection's busy timeout changes CONNECTION state —
// what this connection will wait for the write lock — and writes nothing to the
// file. It therefore has no store version to be behind, and asking would answer a
// question about a pragma rather than about the data.
func setScopedBusyTimeout(ctx context.Context, conn *sql.Conn, d time.Duration) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = `+
		strconv.Itoa(int(d.Milliseconds()))); err != nil {
		return fmt.Errorf("set the scoped busy_timeout: %w", err)
	}
	return nil
}

// DigestQuery is the retrieval record's query_hash: the same keyed digest
// QueryDigest returns, exposed as a method so the store satisfies
// assemble.RecordSink. The assembler asks the store that will HOLD the digest
// rather than resolving a per-install key itself — see that interface's comment,
// and the reason it is a method here and not a call in that package.
func (s *Store) DigestQuery(query string) (string, error) { return QueryDigest(query) }

// RetrievalRecords returns the newest calls first, at most `limit` of them.
//
// Newest first because every reader wants the recent past, and because the cap
// keeps the OLD end: a reader ordering the other way walks off the end of what
// the store still holds and reports a gap as a gap. A limit of zero or less
// returns the whole cap, which is the table's own bound rather than a second
// number to keep in step with it.
//
// A row whose verdicts column cannot be parsed is returned with NO verdicts
// rather than failing the read — see retrievalRecords, which owns the decode and
// the reason.
func (s *Store) RetrievalRecords(ctx context.Context, limit int) ([]RetrievalRecord, error) {
	return s.retrievalRecords(ctx, "", limit)
}
