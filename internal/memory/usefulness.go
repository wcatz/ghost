package memory

// #648 slice 1: what the retrieval audit already knows about a memory, read as
// INPUT by the two passes that maintain a corpus.
//
// retrieval_audit holds four verdicts per (call, memory): used, ignored,
// superseded_in_session and contradicted. Two of them are facts about the
// memory's own reliability — a session found it contradicted, or found it had
// already been superseded by something said in the same session — and resolve
// and reflect were never told either. They re-derived the same judgement from
// the note's text every pass, which is the expensive way to learn something the
// store already recorded.
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
// One statement, one pass, no transaction. The pool is a single connection, so
// holding a read transaction would reserve it across statements for no
// consistency a per-memory figure does not already have; and the whole answer is
// a GROUP BY whose output is bounded by the number of memories with a negative
// verdict, which is what makes "one bounded read per pass" true rather than
// aspirational.

import (
	"context"
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
	// LastSession and LastAt are the latest of THIS memory's negative verdicts, and
	// either may be empty: a verdict recorded without a session is a real row, and
	// a stamp the writer did not set is not worth refusing.
	LastSession string
	LastAt      string
}

// Line renders the evidence as the ONE fixed format that reaches a prompt.
//
// It is built from counts and ids, never from free text a session wrote: the
// session id is neutralised below, and nothing else in the line comes from the
// row. The numbers are the two negative buckets and nothing else, so a line that
// reaches a prompt cannot be a popularity signal wearing a different name.
//
// The empty string means "no negative verdict", and every consumer treats it as
// "say nothing" rather than "say something blank" — a memory with no audit rows
// must reach a model as exactly the bytes it reached it as before this existed.
func (e UsefulnessEvidence) Line() string {
	if e.Contradicted == 0 && e.SupersededInSession == 0 {
		return ""
	}
	var counts []string
	if e.Contradicted > 0 {
		counts = append(counts, fmt.Sprintf("contradicted=%d", e.Contradicted))
	}
	if e.SupersededInSession > 0 {
		counts = append(counts, fmt.Sprintf("superseded_in_session=%d", e.SupersededInSession))
	}
	line := "audit: verdicts " + strings.Join(counts, " ")
	switch {
	case e.LastSession != "" && e.LastAt != "":
		line += fmt.Sprintf("; latest session %s on %s", neutralSessionID(e.LastSession), e.LastAt[:min(10, len(e.LastAt))])
	case e.LastAt != "":
		line += fmt.Sprintf("; latest verdict on %s", e.LastAt[:min(10, len(e.LastAt))])
	case e.LastSession != "":
		line += fmt.Sprintf("; latest session %s", neutralSessionID(e.LastSession))
	}
	return line
}

// usefulnessSessionMax bounds the id inside the line. It is arbitrary stored text
// and the line is a prompt fragment, so an id of any length must not be able to
// grow the prompt without limit; the bound TRUNCATES, never drops the id, because
// rendering no id at all would claim no session recorded it and one did.
const usefulnessSessionMax = 64

// usefulnessReplaced is what a character a session could have chosen becomes. It
// is a token nobody can forge and a model reads as "this character was altered",
// so the reader can see the id was changed rather than silently believe it.
const usefulnessReplaced = "<?>"

// neutralSessionID makes an arbitrary stored session id safe to put in a prompt.
//
// This is the one field of the line that came from a session, so it is the one
// field that could be an instruction. Both consumers render the line into a
// prompt where a NEWLINE ends a record — reflect lists one memory per line — so
// an id carrying "\n- id:E1 ... " would forge a second record naming an id the
// run was never given. And « closes the «...» data block resolve and reflect both
// wrap their material in, after which anything reads as the harness speaking. A
// control character could also drive a terminal that renders the transcript.
//
// So every character that could end a line, end a record, close a data block or
// drive a terminal is replaced with one fixed escape, and the result is ONE line
// of bounded length.
func neutralSessionID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for i, r := range id {
		if i >= usefulnessSessionMax {
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '«' || r == '»' || r < 0x20 || r == 0x7f:
			b.WriteString(usefulnessReplaced)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// UsefulnessByMemory reads the negative verdicts for one project, as one map
// keyed by memory id.
//
// The scope is REQUIRED. RetrievalAudits can read the whole table because its
// callers report on the whole table; this one's output is an input to one
// project's pass, and another project's contradiction is not a claim about this
// project's corpus. An empty project id would pool both, so it is refused here
// rather than defaulted.
//
// Read-only, no transaction, one statement. Three filters narrow it, and all
// three err toward SILENCE rather than toward a wrong annotation: a memory with
// no qualifying verdict is simply not in the map, which is exactly how it read
// before the audit existed. (1) the outcome is one of the two negative buckets,
// for #284; (2) the verdict was not degraded, so a partial read's caveat is not
// silently dropped; (3) the memory row still stands behind the verdict, so a
// contradiction is not reported against text that has since been rewritten. The
// outcome filter is a bound parameter, for the reason retrieval_totals states: a
// hand-spelled `outcome = 'contradicted'` is one more place a stored value can be
// misspelled, and it would miss silently — the statement returns no rows, so the
// map is empty and every caller behaves exactly as it does on a store that was
// never audited.
func (s *Store) UsefulnessByMemory(ctx context.Context, projectID string) (map[string]UsefulnessEvidence, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("usefulness evidence needs a project: an unscoped read would hand " +
			"one project's pass another project's contradiction as evidence about its own memories")
	}

	// The count and the "latest" are ranked SEPARATELY and joined, rather than
	// counting in one GROUP BY and reading bare session_id/recorded_at off it. A
	// bare column beside a plain SUM is arbitrary in SQLite — it is documented
	// only for the single min()/max() case — so that query would report SOME
	// negative verdict's session, and which one would change with the plan. The
	// rank orders by (recorded_at, rowid) for the reason asof.go states:
	// recorded_at is second-precision, so every verdict one pass writes shares a
	// timestamp, and the newest among them has to be a rowid tie-break rather
	// than an answer.
	//
	// Both halves join `memories` and require `a.recorded_at >= m.updated_at`,
	// which is the staleness guard and not a nicety. A verdict is a claim about
	// the CONTENT a call admitted, and Ghost rewrites content in place under a
	// stable id — ReplaceNonManual's reuse branch and ghost_memory_update both
	// UPDATE `content` on the existing row — while nothing deletes the audit rows
	// for a rewritten memory (retrieval_audit has no foreign key to memories, and
	// the only DELETE by memory_id is PurgeMemoryHistory's). So without the join
	// the very next pass annotates a claim written AFTER the contradiction it is
	// quoting, which is a wrong prompt annotation: the failure mode this whole
	// reader exists to prevent. The predicate is on the verdict, not on the
	// memory, so a memory with one pre-rewrite and one post-rewrite verdict keeps
	// the post-rewrite figure and its count falls to what is still true.
	//
	// THE KNOWN COST OF THIS GUARD, STATED BECAUSE IT IS SILENT: updated_at is not
	// a content clock. UpdateMemory moves it on a metadata-only edit, and
	// ApplyReflection's reusePreservesAge branch moves it after writing an
	// identical `content` back, so a retag, a re-weight or a `verified: true` on a
	// memory the audit contradicted withholds that memory's evidence from every
	// later resolve and reflect pass — with no error, no report line and no log,
	// and the contradiction the classifier was told about simply stops arriving.
	// That is the over-filter, and it is silent; the store has no column that
	// moves only with content, so a guard on one needs either a content hash
	// recorded alongside the verdict (a retrieval_audit column, and therefore a
	// schema change) or a decision to stop moving updated_at on metadata writes
	// (which is store-wide: the passive `_global` bucket's tie-break and
	// pruneActivitySQL both read it). Neither is a decision this reader may make
	// on its own, so the guard keeps erring toward silence and this paragraph is
	// the record of what that costs.
	//
	// Both halves also require `a.degraded = ''`. retrieval_audit.degraded carries
	// the scanner's reason for a partial transcript read, and the schema's own
	// comment says a reader who cannot see that caveat reads the row as a claim
	// about the session. Filtered rather than qualified, because the prompt
	// promise this reader makes is byte-for-byte: a memory whose only negative
	// verdicts are degraded must render NOTHING, not a weaker line that is still
	// a line. internal/audit prints "N of those verdicts are degraded" for the
	// same reason — a contradiction is a strong signal or it is not one.
	//
	// The two halves are SUBQUERIES rather than CTEs, so the statement opens
	// with SELECT. That is not a style choice: the #746 structural scan resolves
	// a statement's leading keyword and counts anything it cannot read as a
	// WRITE, because a `WITH` can introduce an INSERT — so a reader written as
	// a CTE needs an exemption entry claiming it is a read, and the honest way
	// to keep that claim off the record is not to write one.
	query := `
		SELECT c.memory_id, c.contradicted, c.superseded, n.session_id, n.recorded_at
		FROM (
		    SELECT a.memory_id AS memory_id,
		           SUM(CASE WHEN a.outcome = ? THEN 1 ELSE 0 END) AS contradicted,
		           SUM(CASE WHEN a.outcome = ? THEN 1 ELSE 0 END) AS superseded
		    FROM retrieval_audit a
		    JOIN memories m ON m.id = a.memory_id AND m.project_id = a.project_id
		                   AND a.recorded_at >= m.updated_at
		    WHERE a.project_id = ? AND a.memory_id <> '' AND a.degraded = '' AND a.outcome IN (?, ?)
		    GROUP BY a.memory_id
		) c
		JOIN (
		    SELECT a.memory_id AS memory_id, a.session_id AS session_id,
		           a.recorded_at AS recorded_at,
		           ROW_NUMBER() OVER (PARTITION BY a.memory_id
		                               ORDER BY a.recorded_at DESC, a.rowid DESC) AS rn
		    FROM retrieval_audit a
		    JOIN memories m ON m.id = a.memory_id AND m.project_id = a.project_id
		                   AND a.recorded_at >= m.updated_at
		    WHERE a.project_id = ? AND a.memory_id <> '' AND a.degraded = '' AND a.outcome IN (?, ?)
		) n ON n.memory_id = c.memory_id AND n.rn = 1`

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, query,
		VerdictOutcomeContradicted, VerdictOutcomeSuperseded,
		projectID, VerdictOutcomeContradicted, VerdictOutcomeSuperseded,
		projectID, VerdictOutcomeContradicted, VerdictOutcomeSuperseded)
	if err != nil {
		return nil, fmt.Errorf("read usefulness evidence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := map[string]UsefulnessEvidence{}
	for rows.Next() {
		var id string
		var contradicted, superseded int
		var session, at string
		if err := rows.Scan(&id, &contradicted, &superseded, &session, &at); err != nil {
			return nil, fmt.Errorf("read usefulness evidence: %w", err)
		}
		out[id] = UsefulnessEvidence{
			Contradicted:        contradicted,
			SupersededInSession: superseded,
			LastSession:         session,
			LastAt:              at,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usefulness evidence: %w", err)
	}
	return out, nil
}
