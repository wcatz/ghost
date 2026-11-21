package memory

// Flag evidence is an agent's objection to one memory, reduced to a count; the
// reason text stays in memory_flags. Flags and the content they are judged
// against share one deferred read transaction, as two statements because a join
// would repeat the unbounded content column once per flag. Rows are closed by
// the opener — an open cursor deadlocks this single-connection pool.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// FlagEvidence is the count of flags filed against one memory, and nothing
// else: the reason text stays in memory_flags, and a flag reaches the classifier
// as a doubt without ever resolving, deleting or re-ranking by itself.
type FlagEvidence struct {
	Flagged int
}

// Line renders the count in the one fixed format that reaches a prompt, so no
// agent-written text can ride into it. Empty means no evidence: a memory with
// no flags reaches the model byte-for-byte as before. The "audit: verdicts"
// prefix is kept for byte stability.
func (e FlagEvidence) Line() string {
	if e.Flagged == 0 {
		return ""
	}
	return fmt.Sprintf("audit: verdicts flagged=%d", e.Flagged)
}

// FlagsByMemory reads the project's flags into a map keyed by memory id. The
// project is required — another project's objection is not evidence about this
// corpus. Two reads in one deferred transaction keep the flags and the content
// they are judged against on one snapshot; filters err toward silence, so a
// memory with no qualifying flag is simply absent, as before flags existed.
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
	// Hash each memory's content once, not once per flag — the digest is a
	// property of the text.
	hashes := map[string]string{}
	for _, f := range flags {
		m, ok := contents[f.memory]
		if !ok {
			// No matching row means the flag names a memory this project does
			// not hold; drop it explicitly rather than by a silent map miss.
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

// verdictDescribes reports whether a stored flag still describes the memory's
// current content: a flag must not outlive the text it judged, since Ghost
// rewrites content in place under a stable id. A stamped flag matches the
// current content hash; a legacy flag (no stamp) falls back to recorded_at vs
// updated_at. A mismatch renders nothing.
func verdictDescribes(stamped, current, recordedAt, updatedAt string) bool {
	if stamped == "" {
		return recordedAt >= updatedAt
	}
	return stamped == current
}

// flagContentChunk bounds the ids in one content query by SQLite's bound
// parameter ceiling, so a heavy store is answered in several reads, not a
// partial one.
const flagContentChunk = 500

// loadFlagRows is the first read: every flag the project holds. The kind
// values are bound parameters so a misspelling cannot silently return no rows.
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

// flagMemories is the second read: one row per flagged memory. A separate
// statement avoids repeating the unbounded content column once per flag, and
// ids are chunked at flagContentChunk for the parameter ceiling. The project
// predicate keeps another project's memory out of this project's pass.
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

// flagMemoryIDs is the distinct memory ids a flag set names, in arrival order;
// the read it feeds promises no order of its own.
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
