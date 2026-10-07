package memory

// #648 slice 1: what the store already knows about a memory, read as INPUT by
// the two passes that maintain a corpus.
//
// retrieval_audit holds four verdicts per (call, memory): used, ignored,
// superseded_in_session and contradicted. Two of them are facts about the
// memory's own reliability — a session found it contradicted, or found it had
// already been superseded by something said in the same session — and resolve
// and reflect were never told either. They re-derived the same judgement from
// the note's text every pass, which is the expensive way to learn something the
// store already recorded.
//
// memory_flags adds the third thing, from slice 2: an agent that read this
// memory and believes it is wrong or stale said so, with a reason. The reason
// stays in the table — this reader projects COUNTS and ids and never a byte of
// an agent's own words — and the flag reaches the passes as one more number in
// the same fixed-format line, where the classifier decides what a doubt is
// worth.
//
// The other two are facts about the READ, not about the memory. `used` means a
// call returned it, which every memory in the result set has by definition;
// `ignored` is the same figure under a worse name. Either one in front of a
// classifier is the #284 popularity loop — retrieve a memory often enough and it
// starts to look reliable because it is often enough in the context, and a
// maintainer told so starts to keep it. So they are filtered in SQL rather than
// at the call sites: a reader that returned all four and left excluding to its
// callers would be one careless caller away from a boost, and closing that loop
// is a property that has to hold without anyone remembering to close it.
//
// Two bounded reads, one pass, ONE SNAPSHOT: the project's negative verdicts,
// then the content of exactly the memories those verdicts name, both inside ONE
// deferred read transaction on the read handle. The transaction is what makes
// the pair one snapshot — the shape before #879's review ran the two reads with
// nothing between them, and a verdict another process DELETED in that window
// was still counted for one pass, a figure naming a row the table no longer
// holds, which is the one direction this package is pointed away from, and no
// comment on that shape could have made it otherwise.
//
// There are two reads rather than one JOINED statement because of what a join
// PROJECTS. content is the largest field in the table and a join repeats it
// once per VERDICT row: retrieval_audit is capped at 50000 rows
// (retrievalAuditRowsCap) and memories.content is unbounded, so a joined read
// materialises a copy of the text per verdict and keeps one per memory, where
// the second read here returns one row per memory and copies each memory's text
// once. The transaction is bought with the read handle rather than the primary
// one for the reason NewStoreWithRead states: the primary DSN issues BEGIN
// IMMEDIATE, so a snapshot there would hold the write lock across the read.
//
// The pool is a single connection, so the rows are drained and CLOSED by the
// call that opened them: a cursor left open holds the connection the next query
// needs, which on this pool is not a slowdown but a deadlock. Both reads are
// bounded — by the project's own audit rows, and by the memories those rows
// name — so "two bounded reads per pass" is true rather than aspirational.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// UsefulnessEvidence is what the audit has to say against ONE memory: the counts
// of the two negative verdicts, and the stamp of the latest of them.
//
// LastSession and LastAt are the latest NEGATIVE verdict's, never the latest
// row's. On any store that also records `used` and `ignored` — that is, on every
// store this feature is for — the latest row is usually a positive verdict, so a
// reader that took the latest row would report a `used` verdict's session as the
// moment this memory was last doubted. That is the leak this type exists to stop,
// and it is why the SQL filters before it selects the latest rather than after.
type UsefulnessEvidence struct {
	Contradicted        int
	SupersededInSession int
	// Flagged is how many flags an agent has filed against this memory (#648
	// slice 2) — a COUNT and nothing else, deliberately: the reason text each
	// flag carries stays in memory_flags where a human reads it, and the row
	// never leaves the store through this struct. It is negative evidence
	// exactly like a contradicted verdict in the one way that matters (it reaches
	// the classifier as a doubt), and exactly unlike one in every other: it is
	// not a retrieval's verdict, so it never moves LastSession/LastAt, and it
	// never resolves, deletes, demotes or re-ranks anything by itself.
	Flagged int
	// LastSession and LastAt are the latest of THIS memory's negative verdicts, and
	// either may be empty: a verdict recorded without a session is a real row, and
	// a stamp the writer did not set is not worth refusing.
	LastSession string
	LastAt      string
}

// Line renders the evidence as the ONE fixed format that reaches a prompt.
//
// It is built from counts and ids, never from free text a session wrote: the
// session id is rendered by SafeToken below, and nothing else in the line comes
// from the row. The numbers are the three negative counts — the two verdict
// buckets and the flag count — and nothing else, so a line that reaches a prompt
// cannot be a popularity signal wearing a different name. The flag's REASON is
// deliberately not among them: it is free text an agent wrote, it is stored for
// the human reading the table, and it would reach a third-party model the moment
// it reached this line.
//
// The empty string means "no negative evidence", and every consumer treats it as
// "say nothing" rather than "say something blank" — a memory with no audit rows
// and no flags must reach a model as exactly the bytes it reached it as before
// this existed.
func (e UsefulnessEvidence) Line() string {
	if e.Contradicted == 0 && e.SupersededInSession == 0 && e.Flagged == 0 {
		return ""
	}
	var counts []string
	if e.Contradicted > 0 {
		counts = append(counts, fmt.Sprintf("contradicted=%d", e.Contradicted))
	}
	if e.SupersededInSession > 0 {
		counts = append(counts, fmt.Sprintf("superseded_in_session=%d", e.SupersededInSession))
	}
	// LAST, so every line a store without flags produced is byte-identical to
	// the line it produces now: the count is appended only when it is non-zero,
	// and no existing fixture has one.
	if e.Flagged > 0 {
		counts = append(counts, fmt.Sprintf("flagged=%d", e.Flagged))
	}
	line := "audit: verdicts " + strings.Join(counts, " ")
	// The id is bounded FIRST and quoted SECOND, and the order is load-bearing for
	// ONE reason: bounding SECOND would cut an ESCAPE rather than a rune, so a
	// truncation landing between a backslash and the character it escapes hands
	// back a malformed literal a reader parses as something other than an id.
	// Bounding the input is what makes the quoting well-formed.
	//
	// It is NOT what bounds the rendered length, and the factor is TEN. The bound
	// counts RUNES of the stored id; under ASCII-only quoting Go writes a BMP rune
	// as \uXXXX (6 characters) and an ASTRAL one as \UXXXXXXXX (10), so an id of
	// 64 escaping runes renders up to 64*10+2 = 642 bytes. Measured, the whole line
	// is 703 bytes against 125 for a bare id
	// (TestTheUsefulnessLineBoundsAnUnboundedId measures all three cases, and the
	// astral one is a separate fixture because a BMP one cannot express the
	// maximum).
	//
	// That ceiling is a constant whether or not a store can currently reach it, and
	// on the SHIPPED transport it cannot: the auditor copies the retrieval record's
	// own session id (audit/run.go), that comes from assemble.Request.SessionID, and
	// over stdio — which is what Ghost serves — the transport reports no session id,
	// so LastSession is always "" and Line takes the `case e.LastAt != ""` branch
	// instead. A transport that DOES assign one (streamable HTTP) records it
	// verbatim with no character validation on the way in, so this is the bound that
	// holds when such a value exists, not one currently exercised. It is kept
	// because the reader has to be safe for the store it will be given, and a bound
	// that waited for a hostile value to arrive would be a bound added after the
	// fact.
	//
	// The renderer this replaced bounded the OUTPUT instead — it stopped
	// writing once the byte index reached the limit — so the worst case one stored
	// id can add to a prompt grew from about 64 bytes to 703. It is still a
	// CONSTANT, which is what "without limit" has to mean, but it is a worse
	// constant by about ten, and this comment must not let a reader take 64 for 64
	// bytes of prompt.
	//
	// Clamping the quoted form afterwards, as assemble.PreviewLine does for display
	// text, would recover the old ceiling and was not chosen: PreviewLine's contract
	// is a PREVIEW of a value whose full text the reader has elsewhere, while this
	// is the id itself, and a clamp that cut one would report an id no session
	// holds — the one output that would be a lie.
	session := SafeToken(boundSessionID(e.LastSession))
	switch {
	case e.LastSession != "" && e.LastAt != "":
		line += fmt.Sprintf("; latest session %s on %s", session, e.LastAt[:min(10, len(e.LastAt))])
	case e.LastAt != "":
		line += fmt.Sprintf("; latest verdict on %s", e.LastAt[:min(10, len(e.LastAt))])
	case e.LastSession != "":
		line += fmt.Sprintf("; latest session %s", session)
	}
	return line
}

// usefulnessSessionMax bounds the id inside the line. It is arbitrary stored text
// and the line is a prompt fragment, so an id of any length must not be able to
// grow the prompt without limit; the bound TRUNCATES, never drops the id, because
// rendering no id at all would claim no session recorded it and one did.
//
// It counts RUNES of the STORED id, not bytes of the RENDERED one, and the two
// differ by up to TENFOLD once the id needs quoting — an astral rune escapes to
// ten characters — so "64" here is not 64 bytes of prompt. See Line, which states
// the measured ceiling rather than leaving the reader to derive it. A byte bound
// on the stored id would instead cut a multi-byte rune in half and produce a
// replacement character, which is a different id from the one stored rather than a
// shortened one.
const usefulnessSessionMax = 64

// boundSessionID truncates a session id to usefulnessSessionMax RUNES.
//
// It is deliberately a plain cut with no ellipsis and no marker. It runs BEFORE
// SafeToken, so what it cuts is a rune sequence, not an escape sequence — the
// quoting then sees well-formed input and cannot produce a malformed literal. An
// ellipsis would itself have to be quoted, and a truncated id with a suffix
// appended is no longer the id the store holds, which is the one thing the id is
// for.
func boundSessionID(id string) string {
	count := 0
	for i := range id {
		if count == usefulnessSessionMax {
			return id[:i]
		}
		count++
	}
	return id
}

// UsefulnessByMemory reads the project's negative evidence — the two verdict
// buckets and the flags — as one map keyed by memory id.
//
// The scope is REQUIRED. RetrievalAudits can read the whole table because its
// callers report on the whole table; this one's output is an input to one
// project's pass, and another project's contradiction is not a claim about this
// project's corpus. An empty project id would pool both, so it is refused here
// rather than defaulted.
//
// Read-only, ONE deferred read transaction carrying TWO bounded reads. Three
// filters narrow the answer, and all three err toward SILENCE rather than
// toward a wrong annotation: a
// memory with no qualifying evidence is simply not in the map, which is exactly
// how it read before the audit existed. (1) the outcome is one of the two
// negative buckets (or, for a flag row, one of its two kinds), for #284;
// (2) the verdict was not degraded, so a partial
// read's caveat is not silently dropped; (3) the row still describes the
// content the memory holds NOW, which is the filter #879 rebuilt and is spelled
// out on verdictDescribes below and which a flag is judged by for the same
// reason. The outcome filter is a bound parameter, for
// the reason retrieval_totals states: a hand-spelled `outcome = 'contradicted'`
// is one more place a stored value can be misspelled, and it would miss silently
// — the read returns no rows, so the map is empty and every caller behaves
// exactly as it does on a store that was never audited.
//
// The two reads run inside ONE transaction, and that is what buys the
// consistency all three filters assume: the verdicts and the content they are
// judged against are read from the same snapshot, so the window a two-statement
// shape leaves open — a verdict another process deleted between the reads, still
// counted for one pass — cannot exist here. The second read is keyed on the ids
// the first returned and returns ONE ROW PER MEMORY, and a verdict whose memory
// the store no longer holds (or one naming a memory another project owns) has
// no row to match: it is dropped in Go by the guard below rather than by a join,
// and that drop is pinned by TestAVerdictWhoseMemoryIsGoneIsDroppedRatherThanCounted.
// The four predicates are spelled ONCE, in the verdict statement, and the second
// statement never restates them — it asks for named ids inside the same project,
// so the two cannot disagree about what counts. The ids travel as bound
// parameters, never as pasted text, and each statement is rooted in a literal
// SELECT, which is what the #746 structural scan resolves to a read. Everything
// after the read is Go, because SQLite has no sha256: filter (3) cannot be
// expressed in SQL at all, and the Go side hashes each memory's content ONCE
// rather than once per verdict.
func (s *Store) UsefulnessByMemory(ctx context.Context, projectID string) (map[string]UsefulnessEvidence, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("usefulness evidence needs a project: an unscoped read would hand " +
			"one project's pass another project's contradiction as evidence about its own memories")
	}

	verdicts, contents, err := s.usefulnessRows(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// A project the audit has nothing negative to say about reads no memory
	// content at all: usefulnessRows issues the second statement only when the
	// first returned verdicts, so an unaudited project costs one statement and
	// no content read, and the map it gets back is the one every caller already
	// behaves correctly with.
	if len(verdicts) == 0 {
		return map[string]UsefulnessEvidence{}, nil
	}

	out := map[string]UsefulnessEvidence{}
	// The content hash is computed ONCE per memory, not once per verdict: the
	// digest is a property of the text, and a memory with a dozen contradictions
	// would otherwise pay for the same bytes a dozen times. The cache is keyed by
	// id and lives only for this pass.
	hashes := map[string]string{}
	// The latest verdict is tracked as a (recorded_at, rowid) TUPLE beside the
	// counts rather than read off a ranked row: there is no ranked row to read,
	// because the filter that decides whether a verdict STANDS (#879's hash rule)
	// can only run in Go, and a row ranked before it ran could name a verdict the
	// rule then withdrew. recorded_at is second-precision, so every verdict one
	// pass writes shares a timestamp and the newest among them has to be a rowid
	// tie-break rather than an answer — for the reason asof.go states.
	latest := map[string]usefulnessLatest{}

	for _, v := range verdicts {
		m, ok := contents[v.memory]
		if !ok {
			// The second read is keyed on these ids and scoped to this project, so
			// this fires exactly when the memory a verdict names is not in the store
			// (or not in this one): deleting a memory does not delete its audit rows
			// — retrieval_audit has no foreign key to memories; only
			// PurgeMemoryHistory deletes by memory_id — so the verdict arrives and
			// the row it is a claim about does not. It stays as the drop made
			// explicit in code rather than one a map miss performs silently,
			// because the alternative is reading a zero value as content and asking
			// the legacy rule about text nobody wrote — and an UNSTAMPED verdict
			// answers yes to that question, since recorded_at is never before "".
			continue
		}
		current, seen := hashes[v.memory]
		if !seen {
			current = ContentHash(m.content)
			hashes[v.memory] = current
		}
		if !verdictDescribes(v.hash, current, v.at, m.updatedAt) {
			continue
		}
		if v.source == usefulnessSourceFlag {
			// A flag counts and STOPS here, deliberately. It never touches
			// `latest`: rowids do not compare across retrieval_audit and
			// memory_flags, and the tuple is a stamp of the moment a RETRIEVAL
			// last doubted this memory — a flag is an agent's claim, not a
			// retrieval's verdict, so letting one win the tuple would report it
			// as the moment the store was last contradicted. The counts are the
			// whole of what a flag does here, and the reason text the row
			// carries is not read at all: it stays in the table for the human.
			ev := out[v.memory]
			ev.Flagged++
			out[v.memory] = ev
			continue
		}
		ev := out[v.memory]
		switch v.outcome {
		case VerdictOutcomeContradicted:
			ev.Contradicted++
		case VerdictOutcomeSuperseded:
			ev.SupersededInSession++
		}
		out[v.memory] = ev

		// The (recorded_at, rowid) tuple is compared STRICTLY and the GREATEST wins:
		// a newer stamp replaces the incumbent, and inside one second — where every
		// verdict one pass writes ties, since recorded_at is second-precision — the
		// HIGHER rowid decides, which is the row SQLite inserted later. So the LAST
		// row of a second wins, not the first one scanned. That is the pre-v22
		// answer verbatim: the old statement ranked with
		// ROW_NUMBER() OVER (PARTITION BY memory_id ORDER BY recorded_at DESC,
		// rowid DESC) and took rn = 1, so the newest verdict won and the rowid broke
		// the tie, rather than scan order answering a question nobody asked.
		best, have := latest[v.memory]
		if !have || v.at > best.at || (v.at == best.at && v.rowid > best.rowid) {
			latest[v.memory] = usefulnessLatest{at: v.at, rowid: v.rowid, session: v.session}
		}
	}
	// Every verdict that counted updated BOTH maps in the same iteration, so
	// `latest` covers exactly the keys `out` gained and no dropped verdict can
	// become the reported one. A memory whose every verdict was dropped never
	// entered `out` at all, which is the silence every filter here is pointed at.
	for id, best := range latest {
		ev := out[id]
		ev.LastSession, ev.LastAt = best.session, best.at
		out[id] = ev
	}
	return out, nil
}

// usefulnessLatest is the newest verdict that STOOD for one memory: its stamp,
// the rowid that breaks the tie inside a second, and the session the line
// reports.
type usefulnessLatest struct {
	at      string
	rowid   int64
	session string
}

// usefulnessAuditRow is one negative EVIDENCE row: a verdict off
// retrieval_audit, or a flag off memory_flags, with the rowid it needs to order
// same-second rows and the stamp it needs to be judged against its memory.
//
// `source` says which table it came from, and it is the one field the two
// spell differently in the union. It matters because the rowids of the two
// tables do not compare with each other — the "latest verdict" tuple is
// audit-only — and because the two rows count into different fields: a flag is
// not a verdict and must never be reported as the moment a retrieval last
// doubted this memory.
type usefulnessAuditRow struct {
	rowid   int64
	memory  string
	outcome string
	session string
	at      string
	hash    string
	source  string
}

// The two source literals the union's literal third column carries. Constants
// rather than pasted strings so the SQL and the Go branch on `source` cannot
// drift into disagreeing about what a row is.
const (
	usefulnessSourceAudit = "audit"
	usefulnessSourceFlag  = "flag"
)

// usefulnessContent is what the memory row has to say against one verdict: the
// text itself, and the stamp the legacy rule still reads.
type usefulnessContent struct {
	content   string
	updatedAt string
}

// verdictDescribes decides whether ONE stored verdict still describes the
// content the memory holds now. It is filter (3), and #879 is the reason it is
// a function rather than a join predicate.
//
// A verdict is a claim about the CONTENT a call admitted, and Ghost rewrites
// content in place under a stable id — ReplaceNonManual's reuse branch and
// ghost_memory_update both UPDATE `content` on the existing row — while nothing
// deletes the audit rows for a rewritten memory. So a verdict must not outlive
// the text it judged. Until schema v22 the only approximation of that available
// was `recorded_at >= m.updated_at`, and it was wrong in the SILENT direction:
// `updated_at` is not a content clock, so a retag, a re-weight or a
// `verified: true` moved it past the verdict and withheld a contradicted
// memory's evidence from every later resolve and reflect pass, with no error, no
// report line and no log. The predicate is on the VERDICT, not on the memory, so
// a memory with one pre-rewrite and one post-rewrite verdict keeps the
// post-rewrite figure and its count falls to what is still true.
//
//	stamped == ""  → THE LEGACY RULE (pre-v22): recorded_at >= updated_at
//	stamped != ""  → the hash rule: stamped == the hash of the content stored now
//
// The legacy branch is named here, in docs/invariants.md and in the test that
// pins it, because a fallback nobody names is a silent one — and this one is
// load-bearing rather than a placeholder: every verdict written before v22
// carries an empty stamp, so on every store that exists today this is the arm
// that runs. It errs toward silence (a stale row is withdrawn), which is the
// direction the reader errs in everywhere else. It is NOT a guess at the old
// behaviour being good enough; it is what those rows were already read by, kept
// so that adding the column withdraws nothing twice.
//
// A non-empty stamp that does NOT match means the text was rewritten after the
// verdict: the row's claim is about words nobody can read now, so it renders
// nothing — byte-for-byte the input the pass had before the audit existed.
// Withdrawing errs toward silence too. A bump of ContentHashVersion makes every
// stored stamp mismatch, which withdraws all evidence at once — deliberately,
// for the same reason, and stated in content_hash.go where the version lives.
func verdictDescribes(stamped, current, recordedAt, updatedAt string) bool {
	if stamped == "" {
		return recordedAt >= updatedAt
	}
	return stamped == current
}

// usefulnessContentChunk is how many ids one content statement binds. It is a
// bound on variables rather than on rows: SQLite's ceiling on bound parameters
// is what chunked id-list reads elsewhere in this package are sized against
// (getByIDsChunk, and evidence.go's batch of 200 carrying two placeholders an
// id), and a store with verdicts on tens of thousands of memories would exceed
// an unchunked list rather than return a partial one.
const usefulnessContentChunk = 500

// usefulnessRows is UsefulnessByMemory's read: TWO bounded statements inside
// ONE deferred read transaction, so both describe the same database state.
//
// The transaction is opened on readHandle, never on the primary handle, for the
// reason NewStoreWithRead gives: the primary DSN issues BEGIN IMMEDIATE, so a
// snapshot there holds the write lock for the whole read.
//
// THE READ TAKES NO s.mu, and that is a decision rather than an omission. The
// store's handles are fixed at construction (NewStore, NewStoreWithRead) and
// warnOnce is a sync.Once, so there is no in-memory state here for the lock to
// guard — every value this read consults comes out of the transaction. Taking it
// anyway would put a mutex around a transaction in whichever order, and BOTH
// orders are wrong on the single-connection pool: lock-then-transact waits for
// the connection Candidates holds while Candidates waits for that same lock,
// and transact-then-lock waits for the connection any writer holds while the
// writer waits for that same lock. Not taking it removes the edge instead of
// choosing which deadlock to risk, and nothing that completes on its own
// (a query, a rollback) can hold this read up behind a lock it does not take.
//
// Rows are drained and closed by the helper that opened them, by its defer,
// because the pool is one connection and a cursor left open would hold it.
//
// Both statements are literals used directly in QueryContext, which is what the
// #746 structural scan needs to call them reads — an unresolved statement is
// counted as a WRITE — and nothing in either is built from stored data: the
// ids travel as bound parameters in an IN list.
func (s *Store) usefulnessRows(ctx context.Context, projectID string) ([]usefulnessAuditRow, map[string]usefulnessContent, error) {
	if s.readDB == nil {
		s.warnNoReadHandle()
	}
	handle := s.readHandle()
	if handle == nil {
		return nil, nil, fmt.Errorf("usefulness evidence: store has no database handle")
	}
	tx, err := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("usefulness evidence: begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	verdicts, err := usefulnessVerdicts(ctx, tx, projectID)
	if err != nil {
		return nil, nil, err
	}
	if len(verdicts) == 0 {
		return nil, nil, nil
	}
	contents, err := usefulnessMemories(ctx, tx, projectID, usefulnessMemoryIDs(verdicts))
	if err != nil {
		return nil, nil, err
	}
	return verdicts, contents, nil
}

// usefulnessVerdicts is the first read: every piece of NEGATIVE EVIDENCE the
// project holds — the two verdict buckets off retrieval_audit, and the flags off
// memory_flags — in no particular order (the reader ranks them in Go, because it
// has to rank them after filtering them).
//
// ONE statement rather than two, by UNION ALL, because the second read this
// feeds is keyed on the ids the first returns: splitting the two would mean a
// second statement and a second place to keep the shared predicates in step.
// The union's seventh column is a LITERAL, so a row carries which table it came
// from without the reader having to infer it — and inference is exactly what it
// must not do, since rowids do not compare across the two tables and a flag is
// not a verdict.
//
// The predicates are the four the audit branch has always had — the project, the
// non-empty memory id, the absence of the scanner's degraded caveat, and one of
// the two negative outcomes — plus a fifth: the verdict names a SESSION. A verdict
// filed with an empty session was judged by a run that matched calls to the session
// it had scanned by project alone, so it compares a call with text written in
// another session and says nothing about the call it names; every verdict filed
// before the audit was session-scoped is of that kind. It is skipped here rather
// than deleted, so the table is untouched and the reader errs toward silence. The
// flag branch is NOT subject to it: a flag is an explicit act, not a comparison.
// The flag branch repeats the two that belong to
// it (project, non-empty id) and names its own closed vocabulary of kinds. The
// two vocabularies are bound parameters for the reason retrieval_totals states:
// a hand-spelled spelling is one more place a stored value can be misspelled,
// and it would miss silently — the read returns no rows, so the map is empty and
// every caller behaves exactly as it does on a store that was never audited.
//
// The content_hash column rides along because filter (3) judges the row against
// the content it was stamped with, and SQLite has no sha256 to do that
// comparison here. Flags are stamped by FlagMemory with the same digest, so one
// rule withdraws a verdict and a flag alike when the text is rewritten.
func usefulnessVerdicts(ctx context.Context, q Queryer, projectID string) ([]usefulnessAuditRow, error) {
	query := `
		SELECT rowid, memory_id, outcome, session_id, recorded_at, content_hash, 'audit' AS source
		FROM retrieval_audit
		WHERE project_id = ? AND memory_id <> '' AND degraded = '' AND session_id <> ''
		  AND outcome IN (?, ?)
		UNION ALL
		SELECT rowid, memory_id, kind, session_id, recorded_at, content_hash, 'flag' AS source
		FROM memory_flags
		WHERE project_id = ? AND memory_id <> '' AND kind IN (?, ?)`

	rows, err := q.QueryContext(ctx, query,
		projectID, VerdictOutcomeContradicted, VerdictOutcomeSuperseded,
		projectID, FlagKindWrong, FlagKindStale)
	if err != nil {
		return nil, fmt.Errorf("read usefulness evidence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var verdicts []usefulnessAuditRow
	for rows.Next() {
		var v usefulnessAuditRow
		if err := rows.Scan(&v.rowid, &v.memory, &v.outcome, &v.session, &v.at, &v.hash, &v.source); err != nil {
			return nil, fmt.Errorf("read usefulness evidence: %w", err)
		}
		verdicts = append(verdicts, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usefulness evidence: %w", err)
	}
	return verdicts, nil
}

// usefulnessMemories is the second read: the content and stamp of exactly the
// memories the verdicts name, ONE ROW PER MEMORY.
//
// That shape is the whole reason this is a second statement rather than a
// column beside the first one. A join repeats m.content once per VERDICT row,
// and retrieval_audit is capped at retrievalAuditRowsCap (50000) rows while
// memories.content is unbounded, so the joined form hands the driver a full
// copy of a memory's text per verdict about it and keeps one — where this
// statement binds each id once and copies each memory's text once. The ids are
// chunked at usefulnessContentChunk rather than bound in one statement for the
// variable ceiling, so a store at the cap is answered in several reads instead
// of failing.
//
// The project predicate is repeated here deliberately: it is not a filter that
// can drift with the verdict read (it names no outcome, no degraded caveat and
// nothing else the two reads decide differently), it is the scope of the answer
// — a verdict naming a memory another project owns must not pull that memory's
// text into this project's pass.
func usefulnessMemories(ctx context.Context, q Queryer, projectID string, ids []string) (map[string]usefulnessContent, error) {
	contents := make(map[string]usefulnessContent, len(ids))
	for start := 0; start < len(ids); start += usefulnessContentChunk {
		batch := ids[start:min(start+usefulnessContentChunk, len(ids))]
		args := make([]any, 0, len(batch)+1)
		args = append(args, projectID)
		for _, id := range batch {
			args = append(args, id)
		}
		query := `
			SELECT id, content, updated_at
			FROM memories
			WHERE project_id = ? AND id IN (` + placeholders(len(batch)) + `)`

		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read usefulness content: %w", err)
		}
		for rows.Next() {
			var id, content, updatedAt string
			if err := rows.Scan(&id, &content, &updatedAt); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("read usefulness content: %w", err)
			}
			contents[id] = usefulnessContent{content: content, updatedAt: updatedAt}
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("read usefulness content: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("read usefulness content: %w", err)
		}
	}
	return contents, nil
}

// usefulnessMemoryIDs is the distinct memory ids a verdict set names, in first
// appearance order. Distinct because a memory contradicted a dozen times is one
// row of content, not twelve, and the order is only the order the verdicts
// arrived in — the read it feeds has no ORDER BY and promises none.
func usefulnessMemoryIDs(verdicts []usefulnessAuditRow) []string {
	seen := make(map[string]struct{}, len(verdicts))
	ids := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		if _, ok := seen[v.memory]; ok {
			continue
		}
		seen[v.memory] = struct{}{}
		ids = append(ids, v.memory)
	}
	return ids
}
