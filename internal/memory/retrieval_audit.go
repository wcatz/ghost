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
//
// A fourth delete is not optional, and it is the one this table's grain creates.
// record_rowid points at a retrieval_record row, that table has no
// AUTOINCREMENT, and so a rowid freed by ANY delete is handed to the next call —
// which means a verdict left behind is not merely dangling but silently
// re-attributed to a call that never admitted its memory, and RetrievalAudits
// counts it in a report about precision. So every path that DELETEs
// retrieval_record rows deletes the verdicts whose record_rowid names them, in the
// SAME transaction: DeleteProject by project_id, `ghost history purge` by
// SELECTing the doomed rowids with the record delete's own predicate, and the cap's
// eviction by its own rowid bound. See purgeHistoryTx and RecordRetrieval, and
// TestAPurgeDoesNotLeaveAVerdictThatTheNextCallCanInherit.
//
// The pairing is TWO-SIDED, and the delete side is not the half that finishes it.
// A verdict is filed from a rowid a caller read BEFORE it began judging —
// audit.Run reads the recent calls, judges them, and only then writes — so a
// `ghost history purge` landing in that window deletes the call row, takes the
// verdicts filed so far with it, and frees a rowid the very next RecordRetrieval
// takes; the delete-side sweep cannot help the write that follows it, because by
// then the rows it would have swept are the ones being written anew.
//
// So a verdict is filed only against a call that KEPT its memory, and the check is
// on the CONTENT of the call rather than on the rowid: checking that the rowid
// still EXISTS is not enough, because the window is wide enough for the freed
// rowid to be RE-LET before the write lands, and a verdict filed against the
// successor is the same wrong pair by another route. A verdict whose call did not
// keep its memory becomes a HOLE in the report rather than a claim about a call
// that never admitted the memory. What that costs is one lost (call, memory) pair;
// what filing it would cost is a wrong one, counted in the denominator and
// indistinguishable afterwards. See RecordRetrievalAudits and
// retrievalRecordKeepingMemory, and the three tests named for each way the pairing
// can be broken.
//
// AND THE HOLE IS REPORTED, which is what makes it honest rather than merely
// defensible. The caller counted every row it handed over into the figures it is
// about to print, before this write refused any of them, so a refusal the caller
// cannot see leaves the report claiming a number this table does not hold — and a
// branch that stored nothing is then indistinguishable from a branch that stored
// everything, which is the one property a partial write cannot be allowed to lose.
// Hence the return value: the refused ROWS rather than a count, because the caller
// takes each one out of its per-source and per-outcome figures as well as its
// total. audit.Run reconciles them into Summary.Unfiled and prints the loss BESIDE
// the figures rather than folding it into them, so both numbers a reader needs —
// what the run judged, and what the table holds — are stated.
// TestTheAuditWriteReportsTheVerdictsItRefused holds the store's half and
// TestRunReconcilesWhatTheStoreRefused the call site's.
//
// AND THE GUARD GATES THE REPLACE-DELETE AS WELL AS THE INSERT, which is the half
// that is easy to leave open. A verdict is filed by replacing the call's existing
// rows, and that delete used to run for every rowid in the batch, up front and
// unconditionally — so a batch carrying a row the guard refuses still WIPED that
// rowid's verdicts on the way to refusing it. In the re-let window that destroys a
// legitimate call's evidence: a stale pass deletes the verdict a successor holding
// the freed rowid already filed, and is then refused its own row. The table loses a
// stored pair, nothing re-files it, and the successor's already-printed report
// claims a figure the table does not hold — the same wrong number, reached through
// the DELETE rather than the INSERT. So the claim is EARNED: a pass may replace a
// call's verdicts only once the guard has accepted a row for it.
// TestALatePassCannotWipeTheSuccessorsVerdicts, and
// TestAPartlyRefusedBatchStillKeepsTheRowsItFiled for the shape where one batch is
// both refused and accepted on the same call.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The outcome vocabulary retrieval_audit.outcome holds, spelled ONCE and here.
//
// The strings are here rather than in internal/audit because that package imports this
// one — its report reads a memory.Store — so the dependency runs the only direction it
// can, and both sides of the column share one declaration: internal/audit derives its
// typed Outcome constants from these, and RetrievalSourceTotals switches on these.
//
// They are spelled once because a reader that spells one of them differently counts
// nothing and says nothing. The store-wide aggregate matched `case "superseded"` against
// a stored `superseded_in_session`, so every superseded verdict was counted in Scored
// and in no bucket at all: `ghost_health` printed "0 superseded in session" beside a
// precision whose denominator included them, and the store-wide figure stopped equalling
// the sum of the per-project reports — a bucket whose reader and whose writer disagreed
// about a word.
//
// Declaring the vocabulary is NOT constraining the column, and that stays true:
// retrieval_audit.outcome carries no CHECK (see schema.go) so a row written by a build
// with a bucket this one has no name for is still countable. These are the values THIS
// build can place and match; TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore
// holds the aggregate to every one of them, and internal/audit's AllOutcomes is derived
// from the same list so a fifth bucket cannot be added by either package alone.
const (
	VerdictOutcomeUsed         = "used"
	VerdictOutcomeIgnored      = "ignored"
	VerdictOutcomeSuperseded   = "superseded_in_session"
	VerdictOutcomeContradicted = "contradicted"
)

// AllVerdictOutcomes is every value above, in the order the four outcome buckets are
// named in the figures — so it is a LIST and not a set, and the order is part of what
// the two packages agree on.
var AllVerdictOutcomes = []string{
	VerdictOutcomeUsed,
	VerdictOutcomeIgnored,
	VerdictOutcomeSuperseded,
	VerdictOutcomeContradicted,
}

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
	// ContentHash is the hash of the CONTENT this verdict judged, stamped by the
	// writer from memory.ContentHash and empty on a row written before schema
	// v22. It is what UsefulnessByMemory compares against the content stored
	// NOW, which is the only comparison that can tell a retag (the text is
	// unchanged, the verdict stands) from a rewrite (the text it judged is gone,
	// the verdict is a claim about nobody's words) — a distinction no timestamp
	// in this schema can make, because updated_at is written by metadata-only
	// edits too (#879).
	ContentHash string
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
//
// It RETURNS THE ROWS THE GUARD REFUSED, and that is the point of the signature.
// A refusal is not an error — the write did what it was asked and declined to
// store one pair it could not vouch for — but it is also not nothing, because the
// caller has already counted every row it handed over into the report it is about
// to print. A caller that cannot see the refusal prints figures describing a
// table it does not match, and a branch that stored nothing is then
// indistinguishable from one that stored everything, which is the one property a
// partial write cannot be allowed to lose. The rows and not a count, because the
// caller has to take each one out of its own per-source and per-outcome figures
// as well as from its total, and a bare number cannot say which. Empty when
// nothing was refused, so a caller that ignores the return is unaffected.
func (s *Store) RecordRetrievalAudits(ctx context.Context, rows []RetrievalAuditRow) ([]RetrievalAuditRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	for _, r := range rows {
		if r.MemoryID == "" {
			return nil, errRetrievalAuditNoMemory
		}
		if r.ProjectID == "" {
			return nil, errRetrievalAuditNoProject
		}
	}

	tx, lock, err := s.beginWrite(ctx, "record-retrieval-audits")
	if err != nil {
		return nil, fmt.Errorf("record retrieval audits: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Which calls this batch has already replaced. The replacement is keyed by
	// record_rowid, so a call's verdicts are replaced ONCE per batch however many
	// rows name it — and only once the guard below has accepted a row for it, which
	// is the whole point: see the REPLACE block after the insert.
	replaced := map[int64]bool{}

	var maxRowID int64
	// The rows the guard below refused, returned so the caller can take them out
	// of its own figures. Declared here rather than at the signature so the write
	// loop reads as the write loop.
	var refused []RetrievalAuditRow
	for _, r := range rows {
		// The pairing is TWO-SIDED, and this is its other side. A caller judged
		// its calls and is filing the verdicts now, so every rowid in this batch
		// was read BEFORE this transaction opened: audit.Run reads the recent
		// calls, judges them (a GetByIDs, then a comparison against the
		// transcript), and only then writes, keyed by the rowids it read at the
		// top of the run. A `ghost history purge` landing in that window deletes
		// the call row and — with the delete-side pairing — takes its verdicts
		// with it, which is right; and then this write files them again against a
		// rowid the store has already given away. When the purged call is the
		// newest, its rowid IS the table's maximum, so the very next
		// RecordRetrieval takes it and the re-filed verdict becomes a claim about
		// a call that never admitted the memory.
		//
		// So a verdict is filed only against a call that KEPT its memory. The
		// guard is in the statement rather than in a branch above it because this
		// transaction already holds the write lock, so it cannot go stale between
		// the check and the insert: a check outside the transaction would be a
		// check-then-write and would lose to the same race it is here to close.
		//
		// It binds IDENTITY, not the KEY, and that is the half that matters. A
		// plain `EXISTS (SELECT 1 FROM retrieval_record WHERE rowid = ?)` is not
		// enough, because the window is wide enough for the rowid to be RE-LET
		// before the pass writes: the purge that removed the newest call frees the
		// table's maximum rowid, and the next RecordRetrieval takes it. The row
		// then EXISTS, the existence check passes, and the dead call's verdict is
		// filed under its successor — which is precisely the wrong pair. So the
		// guard asks whether the call now on that rowid kept the memory THIS row
		// is about, which is the one property a report about a call's admitted
		// memories is actually entitled to assert.
		//
		// What that leaves is a row whose pair is TRUE but whose call INSTANCE may
		// be a successor that admitted the same memory in the same session — the
		// only imprecision left, and deliberately accepted: without a stable
		// per-call identity (the table's key is a reusable rowid) it cannot be
		// closed, and unlike the orphan it is not a claim about a memory a call
		// never admitted. Closing THAT needs AUTOINCREMENT on retrieval_record,
		// which is a table rebuild and a migration, and is out of scope here.
		//
		// `? <= 0 OR` is the unattributed verdict, which is accepted and needs no
		// call: rowid 0 is the deliberate "not attributable to a call" value this
		// same function refuses to replace, and a bare guard would drop every such
		// row in the table the first time a pass filed one.
		//
		// What a dropped verdict costs is ONE (call, memory) pair from the
		// report, and only when a purge lands in that window: the call is gone, so
		// the pair is gone too and nothing is miscounted — the report is a hole,
		// not a lie. The alternative is not a smaller cost, it is the defect:
		// re-filed, the row is counted in the denominator under a call that never
		// admitted the memory, and it is indistinguishable afterwards from a
		// verdict this write was entitled to make. A whole-batch refusal was
		// rejected for the same reason — it would lose the surviving calls'
		// verdicts, which are still true, to avoid losing one that is not.
		//
		// What it costs per row is one seek on retrieval_record's own b-tree plus
		// a walk of that ONE row's verdicts array: the rowid is its INTEGER
		// PRIMARY KEY, so the row is found by primary key and never scanned for,
		// and the array is a call's own kept set — tens of entries, not a table.
		// It is O(one call) per row where the delete-side sweeps in this package
		// are O(rows stored), which is also why it needs no index on
		// record_rowid and why TestRetrievalAuditsCarryOneIndex is unaffected.
		var filed int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO retrieval_audit
				(project_id, record_rowid, session_id, source, memory_id, outcome, signal, degraded, content_hash)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE ? <= 0 OR EXISTS (
				SELECT 1 FROM retrieval_record
				WHERE rowid = ? AND `+retrievalRecordKeepingMemory()+`
			)
			RETURNING rowid
		`, r.ProjectID, r.RecordRowID, r.SessionID, r.Source, r.MemoryID,
			r.Outcome, r.Signal, r.Degraded, r.ContentHash, r.RecordRowID, r.RecordRowID, r.MemoryID).Scan(&filed)
		if errors.Is(err, sql.ErrNoRows) {
			// The guard refused this row, so it wrote nothing and returned
			// nothing. Not an error: see above. It IS reported, because a
			// refusal the caller cannot see is indistinguishable from a row that
			// was written — the caller has already counted this verdict into every
			// figure it will print, and a branch that stores nothing has to be
			// distinguishable from one that stored everything, all the way out to
			// whatever the command records about it. The ROW and not a count,
			// because the caller subtracts per source and per outcome as well as
			// from the total, and a bare count cannot say which.
			refused = append(refused, r)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("record retrieval audits: %w", err)
		}
		// maxRowID stays the table's highest rowid, which is what the eviction
		// below reads. A rowid this transaction was given is higher than every
		// rowid already in the table (SQLite hands out max+1, not a freelist
		// slot), and rowids rise within the transaction, so the LAST row filed
		// is the highest — and a row this pass DROPPED took no rowid at all, so
		// skipping one moves nothing. When every row was dropped, maxRowID stays
		// 0, the table did not grow, and the eviction does not run.
		maxRowID = filed

		// REPLACE, and only now. The delete used to run for every distinct rowid
		// in the batch, up front and unconditionally, which meant a batch whose
		// rows the guard refuses still WIPED that rowid's existing verdicts on the
		// way to refusing them. In the re-let window that destroys a legitimate
		// call's evidence: a purge frees the newest call's rowid, a successor takes
		// it and files its own verdict, and a stale pass then deletes that verdict
		// and is refused its own row — so the table loses a stored pair, nothing
		// re-files it, and the successor's already-printed report claims a figure
		// the table does not hold. The wrong number, reached through the DELETE
		// rather than the INSERT.
		//
		// So the claim is earned, not assumed: a pass may replace a call's verdicts
		// only once it has filed a row for that call, and the guard is what decides.
		// `rowid <> ?` keeps the row just written, so the replacement is still a
		// replacement — a re-audit drops the previous pass's rows for the same
		// (call, memory) pair rather than doubling the table's denominators — and a
		// batch naming several memories of one call still replaces once, not once
		// per row, which is what `replaced` is for.
		//
		// It cannot be a whole-call delete on a row the guard will refuse, and it
		// cannot be narrowed to (record_rowid, memory_id) pairs either: the call
		// this batch is entitled to speak for is the one it read, and the rows
		// already in the table under that rowid may name memories THIS pass judged
		// differently or did not reach at all. Replacing them is the point.
		if r.RecordRowID > 0 && !replaced[r.RecordRowID] {
			replaced[r.RecordRowID] = true
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM retrieval_audit WHERE record_rowid = ? AND rowid <> ?`,
				r.RecordRowID, filed); err != nil {
				return nil, fmt.Errorf("record retrieval audits: replace the verdicts of call %d: %w",
					r.RecordRowID, err)
			}
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
			return nil, fmt.Errorf("record retrieval audits: cap table size: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("record retrieval audits: %w", err)
	}
	lock.reportHold("record-retrieval-audits", time.Now())
	return refused, nil
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
		       degraded, recorded_at, content_hash
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
			&r.MemoryID, &r.Outcome, &r.Signal, &r.Degraded, &r.RecordedAt,
			&r.ContentHash); err != nil {
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
	return s.retrievalRecords(ctx, projectID, "", false, limit)
}

// RetrievalRecordsForSession returns the calls one session made against a project,
// newest first, at most `limit` of them.
//
// The session predicate is in the SQL and not applied to RetrievalRecordsForProject's
// result, because the limit is applied AFTER the WHERE: a project's newest `limit`
// calls can all belong to other sessions, and a Go filter over that window would find
// this session's calls evicted from it by calls that are not its own.
//
// An EMPTY session id reads NOTHING, and that is the load-bearing line rather than a
// guard. `session_id = ”` is exactly what every legacy row and every call from a host
// whose server cannot name its session holds, so an empty argument matched literally
// would hand the audit every call that cannot be attributed to any session, to be
// judged against a session record that none of them belongs to. A call with no session
// is left unjudged, never guessed.
func (s *Store) RetrievalRecordsForSession(ctx context.Context, projectID, sessionID string, limit int) ([]RetrievalRecord, error) {
	if projectID == "" || sessionID == "" {
		return nil, nil
	}
	return s.retrievalRecords(ctx, projectID, sessionID, true, limit)
}

// retrievalRecords is the one decode behind both readers, so a row's fields are
// read the same way whichever reader asked for it.
func (s *Store) retrievalRecords(ctx context.Context, projectID, sessionID string, scoped bool, limit int) ([]RetrievalRecord, error) {
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
	var where []string
	var args []interface{}
	if projectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, projectID)
	}
	if scoped {
		where = append(where, `session_id = ?`)
		args = append(args, sessionID)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
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
