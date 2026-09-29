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
	"encoding/json"
	"errors"
	"fmt"
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

// retrievalRecordRowsCap bounds the table as a whole, oldest row first.
//
// The cap is measured in CALLS because that is the unit the report needs: 5000
// calls is a few weeks of an agent's searching, and a store that runs past it
// loses the OLDEST evidence rather than the newest, so what survives is the
// window an operator is actually looking at. It is a var because the cap's
// policy is what the tests exercise, and a test that cannot lower the bound
// cannot assert the eviction without writing 5000 rows.
var retrievalRecordRowsCap = 5000

// errRetrievalNoProject reports a record with no project to attribute it to.
//
// A refusal rather than a defaulted row: the report's first question is "which
// project's retrieval was this", and a row filed under nothing answers it as
// "all of them", which is a claim about a call nothing recorded.
var errRetrievalNoProject = errors.New("record retrieval: a project id is required")

// RecordRetrieval appends one call's record.
//
// The append and the cap's eviction share one transaction, so the table cannot
// exceed its bound even transiently — the same reason memory_history's prune
// rides along with its append, and the same reason the two statements are here
// rather than in a background pass. The INSERT reports its own rowid, so the
// common case costs two statements and the over-cap case three: nothing is
// probed when the row just written cannot have crossed the bound.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "record-retrieval")
	if err != nil {
		return fmt.Errorf("record retrieval: %w", err)
	}
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

// RetrievalRecords returns the newest calls first, at most `limit` of them.
//
// Newest first because every reader wants the recent past, and because the cap
// keeps the OLD end: a reader ordering the other way walks off the end of what
// the store still holds and reports a gap as a gap. A limit of zero or less
// returns the whole cap, which is the table's own bound rather than a second
// number to keep in step with it.
//
// A row whose verdicts column cannot be parsed is returned with NO verdicts
// rather than failing the read. A reader that errors on one bad row cannot report
// on the store at all, which is the state an operator is in precisely when they
// need the report — and the empty list is honest, where a partial parse would
// claim a row the tool could not read was judged clean. The rest of the record
// is still returned, so the call is still countable and still attributable.
func (s *Store) RetrievalRecords(ctx context.Context, limit int) ([]RetrievalRecord, error) {
	if limit <= 0 {
		limit = retrievalRecordRowsCap
	}
	// The preallocation is clamped to the cap even though the LIMIT is not,
	// because the table cannot hold more rows than the cap: a caller asking for
	// limit=1e9 would get one 120MB allocation for at most 5000 rows. The query
	// keeps the caller's number, so a caller that wants a wider window than the
	// table holds still gets the whole table rather than a silent clamp.
	prealloc := limit
	if prealloc > retrievalRecordRowsCap {
		prealloc = retrievalRecordRowsCap
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// The pool is safe here: no transaction is open on this handle. Ordered by
	// rowid rather than recorded_at, for the precision reason above.
	//
	// The verdicts column is read RAW rather than through readableVerdicts, and
	// the guard below lives in Go instead. The purge needs the check in SQL
	// because its predicate is a predicate; a reader can decide for itself, and
	// deciding in Go costs one parse per row instead of the two json_valid and
	// json_type would each cost — which is the difference between a 200-record
	// report read and a report that is slow enough to be run less often.
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, session_id, source, query_hash, as_of, outcome, reason,
		       verdicts, recorded_at
		FROM retrieval_record
		ORDER BY rowid DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read retrieval records: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]RetrievalRecord, 0, prealloc)
	for rows.Next() {
		var rec RetrievalRecord
		var verdictJSON string
		if err := rows.Scan(&rec.ProjectID, &rec.SessionID, &rec.Source, &rec.QueryHash,
			&rec.AsOf, &rec.Outcome, &rec.Reason, &verdictJSON, &rec.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("read retrieval records: %w", err)
		}
		// One decode settles every case the SQL guard would: a document that is
		// not JSON, a valid JSON document that is not an array, and an array of
		// something that is not a verdict. All three read as "this call judged
		// nothing", which is honest, where a partial parse would claim a row the
		// tool could not read was judged clean.
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
