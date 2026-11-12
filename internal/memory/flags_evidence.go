package memory

// The flags reader: what the store knows about a memory's FLAGS, read as INPUT
// by the two passes that maintain a corpus.
//
// A flag is an agent's own objection to ONE memory, and the count of them is
// negative evidence the classifier is told about beside the note. The reason
// text each flag carries stays in the table — this reader projects COUNTS and
// ids and never a byte of an agent's own words.
//
// ONE read, ONE snapshot: the project's flags, then the content of exactly the
// memories those flags name, both inside ONE deferred read transaction on the
// read handle. The transaction is what makes the pair one snapshot — the shape
// before #879's review ran the two reads with nothing between them, and a flag
// another process DELETED in that window was still counted for one pass, a
// figure naming a row the table no longer holds, which is the one direction
// this package is pointed away from.
//
// There are two reads rather than one JOINED statement because of what a join
// PROJECTS. content is the largest field in the table and a join repeats it
// once per FLAG row, so a joined read materialises a copy of the text per flag
// and keeps one per memory, where the second read here returns one row per
// memory and copies each memory's text once. The transaction is bought with
// the read handle rather than the primary one for the reason NewStoreWithRead
// states: the primary DSN issues BEGIN IMMEDIATE, so a snapshot there would hold
// the write lock across the read.
//
// The pool is a single connection, so the rows are drained and CLOSED by the
// call that opened them: a cursor left open holds the connection the next query
// needs, which on this pool is not a slowdown but a deadlock.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// FlagEvidence is what the store has to say against ONE memory: the count
// of the flags an agent has filed against it.
//
// It is deliberately a COUNT and nothing else: the reason text each flag
// carries stays in memory_flags where a human reads it, and the row never
// leaves the store through this struct. It is negative evidence exactly like a
// contradicted verdict in the one way that matters (it reaches the classifier
// as a doubt), and exactly unlike one in every other: it is not a retrieval's
// verdict, so it never resolves, deletes, demotes or re-ranks anything by
// itself.
type FlagEvidence struct {
	Flagged int
}

// Line renders the evidence as the ONE fixed format that reaches a prompt.
//
// It is built from a count, never from free text a session wrote, so a line
// that reaches a prompt cannot carry an agent's own words. The empty string
// means "no negative evidence", and every consumer treats it as "say nothing"
// rather than "say something blank" — a memory with no flags must reach a
// model as exactly the bytes it reached it as before this existed.
//
// The "audit: verdicts" prefix is kept deliberately, so the reflection and
// resolve text stays byte-stable: it is the wording those prompts already carry,
// the tests pin it, and the count after it is the number of flags, not the
// output of any judge (none exists any more).
func (e FlagEvidence) Line() string {
	if e.Flagged == 0 {
		return ""
	}
	return fmt.Sprintf("audit: verdicts flagged=%d", e.Flagged)
}

// FlagsByMemory reads the project's flags as one map keyed by memory id.
//
// The scope is REQUIRED. Another project's objection is not a claim about this
// project's corpus, so an empty project id is refused here rather than
// defaulted.
//
// Read-only, ONE deferred read transaction carrying TWO bounded reads. Three
// filters narrow the answer, and all three err toward SILENCE rather than
// toward a wrong annotation: a memory with no qualifying flag is simply not in
// the map, which is exactly how it read before flags existed. (1) the project,
// and the non-empty memory id; (2) the row still describes the content the
// memory holds NOW, which is the filter #879 rebuilt and is spelled out on
// verdictDescribes below; (3) the flag's kind is one of the two closed
// vocabulary values, so a row written by another build is not counted as an
// objection this build cannot weigh.
//
// The two reads run inside ONE transaction, and that is what buys the
// consistency all three filters assume: the flags and the content they are
// judged against are read from the same snapshot, so the window a
// two-statement shape leaves open — a flag another process deleted between the
// reads, still counted for one pass — cannot exist here. The second read is
// keyed on the ids the first returned and returns ONE ROW PER MEMORY, and a
// flag whose memory the store no longer holds (or one naming a memory another
// project owns) has no row to match: it is dropped in Go by the guard below
// rather than by a join, and that drop is pinned by
// TestAFlagWhoseMemoryIsGoneIsDroppedRatherThanCounted.
func (s *Store) FlagsByMemory(ctx context.Context, projectID string) (map[string]FlagEvidence, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("flag evidence needs a project: an unscoped read would hand " +
			"one project's pass another project's objection as evidence about its own memories")
	}

	if s.readDB == nil {
		s.warnNoReadHandle()
	}
	handle := s.readHandle()
	if handle == nil {
		return nil, fmt.Errorf("flag evidence: store has no database handle")
	}
	tx, err := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("flag evidence: begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	flags, err := loadFlagRows(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	if len(flags) == 0 {
		return map[string]FlagEvidence{}, nil
	}
	contents, err := flagMemories(ctx, tx, projectID, flagMemoryIDs(flags))
	if err != nil {
		return nil, err
	}

	out := map[string]FlagEvidence{}
	// The content hash is computed ONCE per memory, not once per flag: the
	// digest is a property of the text, and a memory with a dozen objections
	// would otherwise pay for the same bytes a dozen times.
	hashes := map[string]string{}
	for _, f := range flags {
		m, ok := contents[f.memory]
		if !ok {
			// The second read is keyed on these ids and scoped to this project,
			// so this fires exactly when the memory a flag names is not in the
			// store (or not in this one). It stays as the drop made explicit in
			// code rather than one a map miss performs silently.
			continue
		}
		current, seen := hashes[f.memory]
		if !seen {
			current = ContentHash(m.content)
			hashes[f.memory] = current
		}
		if !verdictDescribes(f.hash, current, f.at, m.updatedAt) {
			continue
		}
		ev := out[f.memory]
		ev.Flagged++
		out[f.memory] = ev
	}
	return out, nil
}

// flagRow is one flag row with the stamp it needs to be judged against
// its memory.
type flagRow struct {
	memory string
	at     string
	hash   string
}

// flagContent is what the memory row has to say against one flag: the
// text itself, and the stamp the legacy rule still reads.
type flagContent struct {
	content   string
	updatedAt string
}

// verdictDescribes decides whether ONE stored flag still describes the content
// the memory holds now. It is the filter #879 rebuilt, and it is a function
// rather than a join predicate.
//
// A flag is a claim about the CONTENT a call admitted, and Ghost rewrites
// content in place under a stable id — ReplaceNonManual's reuse branch and
// ghost_memory_update both UPDATE `content` on the existing row — while nothing
// deletes the flag rows for a rewritten memory. So a flag must not outlive the
// text it judged. Until schema v22 the only approximation of that available
// was `recorded_at >= m.updated_at`, and it was wrong in the SILENT direction:
// `updated_at` is not a content clock, so a retag, a re-weight or a
// `verified: true` moved it past the flag and withheld a live objection from
// every later resolve and reflect pass, with no error, no report line and no
// log. The predicate is on the FLAG, not on the memory, so a memory with one
// pre-rewrite and one post-rewrite flag keeps the post-rewrite figure and its
// count falls to what is still true.
//
//	stamped == ""  → THE LEGACY RULE (pre-v22): recorded_at >= updated_at
//	stamped != ""  → the hash rule: stamped == the hash of the content stored now
//
// A non-empty stamp that does NOT match means the text was rewritten after the
// flag: the row's claim is about words nobody can read now, so it renders
// nothing — byte-for-byte the input the pass had before flags existed.
// Withdrawing errs toward silence too.
func verdictDescribes(stamped, current, recordedAt, updatedAt string) bool {
	if stamped == "" {
		return recordedAt >= updatedAt
	}
	return stamped == current
}

// flagContentChunk is how many ids one content statement binds. It is a
// bound on variables rather than on rows: SQLite's ceiling on bound parameters
// is what chunked id-list reads elsewhere in this package are sized against
// (getByIDsChunk, and evidence.go's batch of 200 carrying two placeholders an
// id), and a store with flags on tens of thousands of memories would exceed an
// unchunked list rather than return a partial one.
const flagContentChunk = 500

// loadFlagRows is the first read: every flag the project holds, in no
// particular order.
//
// The two kind literals are bound parameters for the reason retrieval_totals
// states: a hand-spelled spelling is one more place a stored value can be
// misspelled, and it would miss silently — the read returns no rows, so the
// map is empty and every caller behaves exactly as it does on a store that was
// never flagged.
func loadFlagRows(ctx context.Context, q Queryer, projectID string) ([]flagRow, error) {
	query := `
		SELECT memory_id, recorded_at, content_hash
		FROM memory_flags
		WHERE project_id = ? AND memory_id <> '' AND kind IN (?, ?)`

	args := []any{projectID, FlagKindWrong, FlagKindStale}

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read flag evidence: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var flags []flagRow
	for rows.Next() {
		var f flagRow
		if err := rows.Scan(&f.memory, &f.at, &f.hash); err != nil {
			return nil, fmt.Errorf("read flag evidence: %w", err)
		}
		flags = append(flags, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read flag evidence: %w", err)
	}
	return flags, nil
}

// flagMemories is the second read: the content and stamp of exactly the
// memories the flags name, ONE ROW PER MEMORY.
//
// That shape is the whole reason this is a second statement rather than a
// column beside the first one. A join repeats m.content once per FLAG row,
// while memories.content is unbounded, so the joined form hands the driver a
// full copy of a memory's text per flag about it and keeps one — where this
// statement binds each id once and copies each memory's text once. The ids are
// chunked at flagContentChunk rather than bound in one statement for the
// variable ceiling, so a store with many flags is answered in several reads
// instead of failing.
//
// The project predicate is repeated here deliberately: it is not a filter that
// can drift with the flag read (it names no kind and nothing else the two reads
// decide differently), it is the scope of the answer — a flag naming a memory
// another project owns must not pull that memory's text into this project's
// pass.
func flagMemories(ctx context.Context, q Queryer, projectID string, ids []string) (map[string]flagContent, error) {
	contents := make(map[string]flagContent, len(ids))
	for start := 0; start < len(ids); start += flagContentChunk {
		batch := ids[start:min(start+flagContentChunk, len(ids))]
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
			return nil, fmt.Errorf("read flag content: %w", err)
		}
		for rows.Next() {
			var id, content, updatedAt string
			if err := rows.Scan(&id, &content, &updatedAt); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("read flag content: %w", err)
			}
			contents[id] = flagContent{content: content, updatedAt: updatedAt}
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("read flag content: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("read flag content: %w", err)
		}
	}
	return contents, nil
}

// flagMemoryIDs is the distinct memory ids a flag set names, in first
// appearance order. Distinct because a memory objected to a dozen times is one
// row of content, not twelve, and the order is only the order the flags
// arrived in — the read it feeds has no ORDER BY and promises none.
func flagMemoryIDs(flags []flagRow) []string {
	seen := make(map[string]struct{}, len(flags))
	ids := make([]string, 0, len(flags))
	for _, f := range flags {
		if _, ok := seen[f.memory]; ok {
			continue
		}
		seen[f.memory] = struct{}{}
		ids = append(ids, f.memory)
	}
	return ids
}
