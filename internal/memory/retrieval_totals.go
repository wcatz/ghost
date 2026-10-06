package memory

// #646 part 3: the retrieval figures as ONE aggregate per table, for the surfaces
// that report the whole store rather than one project.
//
// BuildReport reads both tables whole and counts in Go, which is the right shape for
// a per-project report and the wrong one for ghost_health: the health block used to
// call it once per project and merge, so the work — and the number of passes over the
// single pooled connection — grew with the number of projects on the machine. An agent
// checking health on a laptop with thirty checkouts paid thirty full reads of both
// tables to learn one number per source.
//
// This reader answers the same questions in a fixed number of statements: two GROUP BY
// aggregates, one per table, whose output is bounded by the size of the CLOSED
// vocabularies rather than by the size of the tables, plus one small statement that
// lists the contradicted ids as rows.
//
// The arithmetic is deliberately the arithmetic BuildReport performs, because two
// implementations of one figure is how this package already produced one bug (the
// degraded note's denominator). Where the SQL cannot state a rule as plainly as Go
// states it, the rule is spelled out at the query.
//
// #852 is why the aggregates are not a join. retrieval_audit.record_rowid is a
// rowid into a table that prunes and purges, so a JOIN could attribute a verdict to a
// DIFFERENT call than the one it was filed against. The attribution test below is a
// membership test on the rowid and reads nothing else — a verdict is either counted or
// it is not.
//
// The id list is a third statement rather than a GROUP_CONCAT column on the verdict
// aggregate, and that is not a performance choice. retrieval_audit.memory_id is
// arbitrary stored text — the column deliberately carries no shape CHECK — so no
// separator can be assumed absent from it, and any scheme that joins ids into one
// string and splits it again would be a guess about the data. Rows are the only shape
// that cannot be misread. It runs only when the aggregate found an attributed
// contradicted verdict to name, so the common store pays nothing for it.

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// RetrievalSourceTotals is one source's figures over the whole store.
//
// Every field counts rows, never projects, and the names are the SourceReport fields
// the renderers read — so a caller can hand these to a renderer without a second layer
// that re-derives anything.
type RetrievalSourceTotals struct {
	Source string
	// Calls is how many recorded calls this source made, including the ones that kept
	// nothing.
	Calls int
	// Kept is how many (call, memory) pairs this source admitted, de-duplicated
	// WITHIN each call and not across calls: a memory two calls both kept is two
	// pairs, which is the grain every figure here is counted over.
	Kept int
	// KeptNothing is how many of those calls admitted no memory at all.
	KeptNothing int
	// Scored is how many verdicts are attributed to a call this report counts, and is
	// the denominator of precision.
	//
	// A verdict naming no call, or naming a call this report is not reporting on, is
	// in Unattributed or Detached and in NO figure here. Precision is a ratio within
	// one population, and a verdict about a session rather than about a call has no
	// (call, memory) pair to belong to — so putting it in the numerator and the
	// denominator is a ratio of two populations wearing one name.
	Scored       int
	Used         int
	Ignored      int
	Superseded   int
	Contradicted int
	// ContradictedIDs is every memory id this source's calls were judged to
	// contradict, deduplicated and sorted. Ids only: the renderers have no content to
	// print and no other field could reach them. An id is stored text and is carried
	// whole — never pasted into a delimited string and cut apart again.
	ContradictedIDs []string
	// DegradedVerdicts is how many scored verdicts were filed under a partial
	// transcript read, and DegradedReasons names the reasons.
	DegradedVerdicts int
	DegradedReasons  []string
	// Unattributed is how many verdicts name NO call at all (record_rowid = 0, a
	// value the write accepts for a verdict about a session rather than about one
	// call).
	Unattributed int
	// Detached is how many name a call this report does not count: outside the
	// window, or no longer held (the call cap evicts at 5000 rows while the verdict cap
	// holds 50000, and a history purge removes a call's rows — #857).
	Detached int
}

// The three attributions of a verdict, as the aggregate reports them. Spelled as SQL
// literals in the query because a CASE arm cannot take a bound parameter in SQLite,
// and named here so the query's numbers can be read against these.
const (
	unattributedVerdict = 0
	attributedVerdict   = 1
	detachedVerdict     = 2
)

// RetrievalSourceTotals aggregates both retrieval tables per source over the whole
// store, in a fixed number of statements: two aggregates and, only when there is a
// contradicted id to name, one listing.
//
// `floor` is the window's start. The zero time means no floor — every row is counted
// — which is what ghost_health asks for, because it reports a standing state rather
// than a window.
//
// Read-only, and it holds no transaction: each statement is one pass, and the pool is
// a single connection, so wrapping them in a read transaction would reserve that
// connection across all of them for no consistency the figures do not already have.
// The attribution membership test is inside the verdict statement, so the two halves of
// every figure are decided against the same set of calls within it — and the id listing
// carries the same test, so a named memory belongs to a figure that counts it.
func (s *Store) RetrievalSourceTotals(ctx context.Context, floor time.Time) ([]RetrievalSourceTotals, error) {
	records, err := s.retrievalRecordTotals(ctx, floor)
	if err != nil {
		return nil, err
	}
	verdicts, err := s.retrievalVerdictTotals(ctx, floor)
	if err != nil {
		return nil, err
	}

	// ONE entry per source across both tables, keyed by name, so a source named by
	// only one of them still gets an entry.
	bySource := map[string]*RetrievalSourceTotals{}
	entry := func(name string) *RetrievalSourceTotals {
		t := bySource[name]
		if t == nil {
			t = &RetrievalSourceTotals{Source: name}
			bySource[name] = t
		}
		return t
	}
	for _, r := range records {
		t := entry(r.source)
		t.Calls += r.calls
		t.Kept += r.kept
		t.KeptNothing += r.keptNothing
	}
	// Whether any source has an attributed contradicted verdict worth naming. Read off
	// the aggregate rather than queried separately, because the answer is already in
	// hand and the answer decides whether the third statement runs at all.
	anyContradicted := false
	for _, v := range verdicts {
		t := entry(v.source)
		switch v.attribution {
		case unattributedVerdict:
			t.Unattributed += v.n
			continue
		case detachedVerdict:
			t.Detached += v.n
			continue
		}
		t.Scored += v.n
		// The four arms are the closed vocabulary of VerdictOutcome*, not four strings
		// typed here, and that is not tidiness. This switch once matched "superseded"
		// while the comparison stored "superseded_in_session", so every superseded
		// verdict counted in Scored and in no bucket at all: the health block printed
		// "0 superseded in session" beside a precision whose denominator included them,
		// and the four buckets stopped summing to Scored. An arm that goes missing — or a
		// fifth bucket added without one — fails here rather than in a store, in
		// internal/audit's TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore.
		switch v.outcome {
		case VerdictOutcomeUsed:
			t.Used += v.n
		case VerdictOutcomeIgnored:
			t.Ignored += v.n
		case VerdictOutcomeSuperseded:
			t.Superseded += v.n
		case VerdictOutcomeContradicted:
			t.Contradicted += v.n
			anyContradicted = true
		}
		// An outcome this build does not know is counted in Scored and in no bucket,
		// so the buckets do not sum to Scored. That gap is the honest reading: the row
		// IS a verdict, and this build cannot say which bucket it falls in. Dropping
		// it would report a stranger store's precision as better than it is.
		//
		// A gap over OUR OWN vocabulary is not that, and is a bug rather than a
		// tolerance — the four arms above are exhaustive over VerdictOutcome*, which is
		// the whole list the comparison writes from.
		if v.degraded != "" {
			t.DegradedVerdicts += v.n
			t.DegradedReasons = append(t.DegradedReasons, v.degraded)
		}
	}

	var idsBySource map[string][]string
	if anyContradicted {
		if idsBySource, err = s.retrievalContradictedIDs(ctx, floor); err != nil {
			return nil, err
		}
	}

	out := make([]RetrievalSourceTotals, 0, len(bySource))
	for name, t := range bySource {
		t.ContradictedIDs = dedupeSorted(idsBySource[name])
		t.DegradedReasons = dedupeSorted(t.DegradedReasons)
		out = append(out, *t)
	}
	// Sorted, so a caller sees one order whatever order SQLite grouped in.
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// dedupeSorted returns the distinct members of in, sorted. nil for an empty input, so
// a source with nothing to name renders as no list rather than as an empty one.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// One row of the record-side aggregate: everything retrieval_record holds about one
// source, over the window.
type recordTotalsRow struct {
	source      string
	calls       int
	kept        int
	keptNothing int
}

// retrievalRecordTotals is the calls/kept half.
//
// `kept` needs the verdicts JSON column expanded, which is why this is a subquery and
// not a bare COUNT. The three shapes that column can hold are all handled the way the
// Go reader handles them, and each is a real row shape rather than a hypothetical:
//
//   - The write always stores a JSON array, so json_each over it is the normal path.
//   - `json_valid` guards the rest. A row hand-edited or written by another build can
//     hold anything, and json_each ERRORS on a malformed document rather than returning
//     nothing — so an unguarded expand turns one unreadable row into a failed health
//     call. The guard substitutes an empty array, which is exactly what the Go reader's
//     failed Unmarshal yields (retrievalRecords sets Verdicts = nil on a decode error:
//     "this call judged nothing").
//   - A JSON document that is valid but not an array (an object, a bare number) makes
//     json_each yield no rows for an object, so it reads as "kept nothing" too.
//
// The DISTINCT is WITHIN the call and not across calls, for the reason the Go path is:
// the grain is the (call, memory) pair.
//
// Each element is guarded by its own json_valid, not only the document: a document that
// is a bare JSON string is valid, json_each yields one element, and json_extract on that
// element ERRORS rather than returning NULL. The guard substitutes an empty object,
// which has no $.kept and so contributes nothing — the same "this call judged nothing"
// the Go reader reaches when unmarshalling that document into []RowVerdict fails.
func (s *Store) retrievalRecordTotals(ctx context.Context, floor time.Time) ([]recordTotalsRow, error) {
	predicate, args := windowPredicate("r.recorded_at", floor)
	query := `
		SELECT r.source AS source,
		       COUNT(*) AS calls,
		       COALESCE(SUM(COALESCE(k.n, 0)), 0) AS kept,
		       COALESCE(SUM(CASE WHEN COALESCE(k.n, 0) = 0 THEN 1 ELSE 0 END), 0) AS kept_nothing
		FROM retrieval_record r
		LEFT JOIN (
			SELECT v.rowid AS rid,
			       COUNT(DISTINCT json_extract(j.value, '$.id')) AS n
			FROM retrieval_record v,
			     json_each(CASE WHEN json_valid(v.verdicts) THEN v.verdicts ELSE '[]' END) j
			WHERE json_valid(j.value)
			  AND json_extract(j.value, '$.kept') IN (1, 'true')
			GROUP BY v.rowid
		) k ON k.rid = r.rowid WHERE ` + predicate + `
		GROUP BY r.source`

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read retrieval totals: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []recordTotalsRow
	for rows.Next() {
		var r recordTotalsRow
		if err := rows.Scan(&r.source, &r.calls, &r.kept, &r.keptNothing); err != nil {
			return nil, fmt.Errorf("read retrieval totals: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retrieval totals: %w", err)
	}
	return out, nil
}

// One row of the verdict-side aggregate: one (source, outcome, degraded, attribution)
// cell. The contradicted memory ids that cell contributed are NOT here — they are the
// subject of their own statement, for the reason in the file comment.
type verdictTotalsRow struct {
	source      string
	outcome     string
	degraded    string
	attribution int
	n           int
}

// retrievalVerdictTotals is the verdicts half.
//
// The attribution is the point of this statement, and it is a membership test rather
// than a join: the two columns are TWO INSTANTS — retrieval_record is stamped when the
// call happened, retrieval_audit when the detached run judged it — so one window over
// both is two populations, and a call at 23:50 judged at 00:05 splits across any
// boundary. A verdict counts iff a call this report counts owns its rowid, which is the
// rule BuildReport states with its counted map.
//
// Grouping by `degraded` is what bounds the output: the column holds the scanner's own
// fail-open vocabulary, not per-session text, so the rows are sources × outcomes ×
// reasons rather than one row per verdict.
//
// `memory_id <> ”` is the reader's own skip: a verdict naming no memory cannot be
// reported on at all, so the Go reader drops it and so does this.
//
// No GROUP_CONCAT here, and that is the load-bearing omission. The aggregate used to
// carry a `GROUP_CONCAT(DISTINCT memory_id)` column and re-split it on ',', justified
// by a claim this checkout refutes: a memory id is NOT 32 hex characters. The column
// carries no shape CHECK on purpose, CheckImportedID refuses only length, control
// characters, whitespace, a backtick and «», and `ghost import` writes an artifact's
// ids verbatim — so `M1,M2` is a legal, reachable id, and the split named two memories
// that do not exist while the count beside them stayed correct. The count is right
// enough to make the wrong list look right, which is why it survived. See
// retrievalContradictedIDs for where the ids come from now.
func (s *Store) retrievalVerdictTotals(ctx context.Context, floor time.Time) ([]verdictTotalsRow, error) {
	attribution, args := verdictAttribution(floor)

	query := `
		WITH judged AS (
			SELECT a.source AS source,
			       a.outcome AS outcome,
			       a.degraded AS degraded,
			       a.memory_id AS memory_id,` + attribution + ` AS attribution
			FROM retrieval_audit a WHERE ` + auditWindowPredicate(floor) + `
			AND a.memory_id <> ''
		)
		SELECT source, outcome, degraded, attribution, COUNT(*) AS n
		FROM judged
		GROUP BY source, outcome, degraded, attribution`

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read retrieval verdict totals: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []verdictTotalsRow
	for rows.Next() {
		var r verdictTotalsRow
		if err := rows.Scan(&r.source, &r.outcome, &r.degraded, &r.attribution, &r.n); err != nil {
			return nil, fmt.Errorf("read retrieval verdict totals: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read retrieval verdict totals: %w", err)
	}
	return out, nil
}

// retrievalContradictedIDs lists, per source, the memory ids the attributed
// contradicted verdicts name — one row per (source, id), so an id is carried whole.
//
// This is the whole of the fix the GROUP_CONCAT column needed, and the reason is
// structural rather than cosmetic: there is no separator to choose safely. An id is
// stored text with no shape constraint, so any character can occur inside one, and a
// scheme that concatenates ids and splits them again has to bet that the delimiter does
// not appear in the data — a bet about TEXT, on a column whose schema comment says in
// as many words that the text is not constrained. Selecting the ids is not slower in any
// way that matters: the statement carries the same predicates as the aggregate, it
// returns one row per DISTINCT id rather than one per verdict, and the caller only runs
// it when the aggregate found something to name.
//
// The attribution and the window are pasted from the same helpers the aggregate uses, so
// a verdict named here is a verdict counted there: the ids belong to exactly the
// population the figures describe, and an unattributed or detached contradiction is
// counted in its own bucket and named by neither.
//
// DISTINCT is in the statement rather than left to the caller's dedupe, so the result
// set is one row per memory rather than one per verdict; the caller still sorts and
// dedupes, because a map keyed by source is what the renderer reads and SQLite's
// grouping order is not a promise.
//
// The outcome it filters on is BOUND, not pasted, for the reason the aggregate's switch
// names the vocabulary rather than typing it: `outcome = 'contradicted'` is one more
// place a hand-written spelling of a stored value can disagree with the comparison, and
// this one would disagree silently — the statement returns no rows at all, so the report
// would name no contradicted memory while still counting the verdicts. The argument
// goes last because that is where its placeholder sits in the text, after both window
// predicates.
func (s *Store) retrievalContradictedIDs(ctx context.Context, floor time.Time) (map[string][]string, error) {
	attribution, args := verdictAttribution(floor)
	args = append(args, VerdictOutcomeContradicted)

	query := `
		WITH judged AS (
			SELECT a.source AS source,
			       a.outcome AS outcome,
			       a.memory_id AS memory_id,` + attribution + ` AS attribution
			FROM retrieval_audit a WHERE ` + auditWindowPredicate(floor) + `
			AND a.memory_id <> ''
		)
		SELECT DISTINCT source, memory_id FROM judged
		WHERE outcome = ? AND attribution = ` + fmt.Sprint(attributedVerdict)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read contradicted ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	bySource := map[string][]string{}
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			return nil, fmt.Errorf("read contradicted ids: %w", err)
		}
		bySource[source] = append(bySource[source], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read contradicted ids: %w", err)
	}
	return bySource, nil
}

// verdictAttribution returns the attribution CASE and the args that bind it, for BOTH
// verdict statements — the aggregate and the id listing.
//
// One helper, because the two statements must agree: an id listed for a verdict the
// aggregate did not count would be a memory named under a figure it is not part of,
// which is the one thing a per-source report cannot do. Sharing the text also means
// neither statement can drift from the other on the window or the membership test.
//
// A CASE arm cannot take a bound parameter in SQLite, hence the two literal integers
// and the constants they correspond to.
//
// The args are in TEXT order, which is the only order a positional `?` can use: the
// call predicate's argument sits inside the SELECT list, ahead of the audit predicate's
// in the WHERE, so the call's binds first. The two hold the SAME string today — one
// floor drives both — so the order is not currently observable, which is exactly why it
// is stated here: bound the other way it would work by luck, and the first caller to
// give the two tables different windows would get silently mis-paired numbers.
func verdictAttribution(floor time.Time) (string, []any) {
	callPredicate, callArgs := windowPredicate("r.recorded_at", floor)
	_, auditArgs := windowPredicate("a.recorded_at", floor)
	attribution := fmt.Sprintf(`
		CASE
			WHEN a.record_rowid <= 0 THEN %d
			WHEN a.record_rowid IN (
				SELECT r.rowid FROM retrieval_record r WHERE %s
			) THEN %d
			ELSE %d
		END`, unattributedVerdict, callPredicate, attributedVerdict, detachedVerdict)
	return attribution, append(append([]any{}, callArgs...), auditArgs...)
}

// auditWindowPredicate is the verdict side's own window clause, named as a helper so
// both verdict statements read the same way and so the argument order in
// verdictAttribution is legible at the place that binds it.
func auditWindowPredicate(floor time.Time) string {
	predicate, _ := windowPredicate("a.recorded_at", floor)
	return predicate
}

// windowPredicate is the window test both aggregates share, WITHOUT a WHERE — so a
// caller pastes it into a subquery's WHERE and into the outer statement's WHERE
// alike, and neither has to know which form the other used.
//
// A row whose stamp cannot be read is KEPT rather than dropped, which is the rule
// BuildReport states in Go ("an unstamped row is kept rather than dropped from a report
// the operator did not ask to lose anything"). The zero floor yields `1`, a predicate
// true for every row, because the caller asked for a standing state rather than a
// window — spelled as a literal rather than as an empty string so the query is still
// readable when a failure prints it.
//
// `julianday` rather than a string comparison, because the stamps are written in two
// layouts (StoredStampLayout and DateStampLayout) and a bare `>=` against a
// StoredStampLayout floor would order a date-only stamp as if its missing time were
// midnight — rejecting a row the Go reader keeps. `IS NOT NULL` guards the column
// itself, because julianday(NULL) is NULL and would otherwise read as "unreadable,
// keep" and let a NULL stamp into the figures.
//
// The argument order is TEXT order and nothing else, because a positional `?` binds by
// where it appears in the statement and not by which table it filters — see
// verdictAttribution, which binds the call clause's argument and this verdict side's own,
// in that order because that is the order the two placeholders sit in the text.
func windowPredicate(col string, floor time.Time) (string, []any) {
	if floor.IsZero() {
		return "1", nil
	}
	return fmt.Sprintf("%s IS NOT NULL AND (julianday(%s) IS NULL OR julianday(%s) >= julianday(?))",
			col, col, col),
		[]any{floor.UTC().Format(StoredStampLayout)}
}
