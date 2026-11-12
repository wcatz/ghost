package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	sqlite "modernc.org/sqlite"
)

const (
	phaseSave = "save"

	phaseUpdate = "update"

	phaseReflect = "reflect"

	phaseMerge = "merge"

	phaseResolve = "resolve"

	phaseUnresolve = "unresolve"

	phaseSupersede = "supersede"

	phaseUnsupersede = "unsupersede"

	phaseRestore = "restore"

	phaseImport = "import"

	phaseDelete = "delete"

	phaseBaseline = "baseline"

	phaseMove = "move"
)

func allHistoryPhases() []string {
	return []string{
		phaseSave, phaseUpdate, phaseReflect, phaseMerge, phaseResolve, phaseUnresolve,
		phaseSupersede, phaseUnsupersede, phaseRestore, phaseImport, phaseDelete, phaseBaseline,
		phaseMove,
	}
}

var (
	historyVersionsPerMemory = 50

	historyRowsCap = 20000
)

const historyBatchSize = 500

var errHistoryNoMemory = errors.New("no live memory row")

var historyRedactor struct {
	filter func(string) string
}

func historyRedactorInstalled() bool { return historyRedactor.filter != nil }

func historyContentExpr() string {
	if historyRedactorInstalled() {
		return historyContentFunc + "(content)"
	}
	return "content"
}

const historyContentFunc = "ghost_history_content"

func init() {

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

		slog.Warn("register history content filter", "function", historyContentFunc, "error", err)
	}
}

func setHistoryRedactor(fn func(string) string) func() {
	prev := historyRedactor.filter
	historyRedactor.filter = fn
	return func() { historyRedactor.filter = prev }
}

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

	RelatedID string `json:"related_id,omitempty"`

	MergedContent string `json:"merged_content,omitempty"`

	FromProjectID string `json:"from_project_id,omitempty"`
	ToProjectID   string `json:"to_project_id,omitempty"`
}

type historyEvent struct {
	phase string

	prov Provenance

	relatedID string

	mergedContent string

	fromProjectID string
	toProjectID   string
}

func appendHistoryTx(ctx context.Context, tx *sql.Tx, memoryID, phase string, prov Provenance) error {
	return appendHistoryEventsTx(ctx, tx, []historyEvent{{phase: phase, prov: prov}}, []string{memoryID})
}

func appendHistoryForIDsTx(ctx context.Context, tx *sql.Tx, ids []string, phase string, prov Provenance) error {
	events := make([]historyEvent, len(ids))
	for i := range events {
		events[i] = historyEvent{phase: phase, prov: prov}
	}
	return appendHistoryEventsTx(ctx, tx, events, ids)
}

func appendHistoryEventsTx(ctx context.Context, tx *sql.Tx, events []historyEvent, ids []string) error {
	for len(ids) > 0 {
		batch := ids
		if len(batch) > historyBatchSize {
			batch = ids[:historyBatchSize]
		}
		ids = ids[len(batch):]
		batchEvents := events[:len(batch)]
		events = events[len(batch):]

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

func appendHistoryGroupTx(ctx context.Context, tx *sql.Tx, e historyEvent, ids []string) error {
	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids)+7)
	args = append(args, e.phase, nullIfEmpty(e.prov.Agent), nullIfEmpty(e.prov.SessionID),
		nullIfEmpty(e.relatedID), nullIfEmpty(e.mergedContent),
		nullIfEmpty(e.fromProjectID), nullIfEmpty(e.toProjectID))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}

	recordedAt := "datetime('now')"
	if slices.Contains(stampMovingPhases, e.phase) {
		recordedAt = "updated_at"
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_history
			(memory_id, project_id, phase, agent, session_id, related_id, merged_content,
			 from_project_id, to_project_id,
			 content, category, importance, resolved_at, source, recorded_at)
		SELECT id, project_id, ?, ?, ?, ?, ?, ?, ?, `+historyContentExpr()+`,
		       category, importance, resolved_at, source, `+recordedAt+`
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

func recordBaselineHistoryTx(ctx context.Context, tx *sql.Tx, memoryID, phase string, prov Provenance) error {

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

	foldedTexts, err := selectIDs(ctx, tx, `
		SELECT merged_content FROM memory_history WHERE memory_id = ? AND merged_content IS NOT NULL`,
		memoryID)
	if err != nil {
		return 0, fmt.Errorf("collect folded-in texts to purge: %w", err)
	}

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

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_provenance WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge memory evidence: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_snapshot_evidence WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge snapshot evidence: %w", err)
	}

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

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retrieval_audit WHERE memory_id = ?`, memoryID); err != nil {
		return 0, fmt.Errorf("purge retrieval audits: %w", err)
	}

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

	const redactionBatch = 25
	redactTexts := make([]string, 0, len(texts)+len(foldedTexts))
	redactTexts = append(redactTexts, texts...)
	redactTexts = append(redactTexts, foldedTexts...)
	for start := 0; start < len(redactTexts); start += redactionBatch {
		batch := redactTexts[start:min(start+redactionBatch, len(redactTexts))]
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

var errPurgeNoMemoryID = errors.New("purge history: a memory id is required")

func rebuildFTSAfterPurgeTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild the full-text index after a purge: %w", err)
	}
	return nil
}

// PurgeOutcome reports what a purge reached.
type PurgeOutcome struct {
	Rows               int64
	CheckpointComplete bool
}

func (s *Store) checkpointAfterPurge(ctx context.Context) (bool, error) {
	var busy, walFrames, folded int
	err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walFrames, &folded)
	if errors.Is(err, sql.ErrNoRows) {

		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("checkpoint after a purge: %w", err)
	}
	return busy == 0 && walFrames == folded, nil
}

func (s *Store) checkpointAfterPurgeUncancelled(ctx context.Context) bool {
	complete, err := s.checkpointAfterPurge(context.WithoutCancel(ctx))
	if err != nil {
		s.logger.Warn("checkpoint after a purge did not run; the delete has committed", "error", err)
		return false
	}
	return complete
}

const purgedTextMarker = "[purged]"

func (s *Store) PurgeMemoryHistory(ctx context.Context, memoryID string) (PurgeOutcome, error) {
	return s.purgeMemoryHistory(ctx, "", memoryID)
}

func (s *Store) PurgeDeletedMemoryHistoryInProject(ctx context.Context, projectID, memoryID string) (PurgeOutcome, error) {
	if projectID == "" {
		return PurgeOutcome{}, fmt.Errorf("purge history: project_id is required")
	}
	return s.purgeMemoryHistory(ctx, projectID, memoryID)
}

func purgeDeletedOwnerTx(ctx context.Context, tx *sql.Tx, projectID, memoryID string) error {
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT project_id FROM memories WHERE id = ?`, memoryID).Scan(&owner)
	switch {
	case err == nil && owner != projectID:
		return fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", memoryID), RefusalArg("project_id", projectID))
	case err == nil:
		return fmt.Errorf("memory %s is live again (restored since it was deleted), so it was not purged as a deleted memory and nothing was erased; delete it to erase it with its history",
			RefusalArg("memory_id", memoryID))
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("lookup memory: %w", err)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT project_id FROM memory_history WHERE memory_id = ? ORDER BY rowid DESC LIMIT 1`,
		memoryID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("memory %s not found, and no recorded history to purge", RefusalArg("memory_id", memoryID))
	case err != nil:
		return fmt.Errorf("read memory history: %w", err)
	case owner != projectID:
		return fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", memoryID), RefusalArg("project_id", projectID))
	}
	return nil
}

func (s *Store) purgeMemoryHistory(ctx context.Context, projectID, memoryID string) (PurgeOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "purge-history")
	if err != nil {
		return PurgeOutcome{}, fmt.Errorf("begin purge history: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if projectID != "" {
		if err := purgeDeletedOwnerTx(ctx, tx, projectID, memoryID); err != nil {
			return PurgeOutcome{}, err
		}
	}
	n, err := purgeHistoryTx(ctx, tx, memoryID)
	if err != nil {
		return PurgeOutcome{}, err
	}

	if err := rebuildFTSAfterPurgeTx(ctx, tx); err != nil {
		return PurgeOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return PurgeOutcome{}, fmt.Errorf("commit purge history: %w", err)
	}

	return PurgeOutcome{Rows: n, CheckpointComplete: s.checkpointAfterPurgeUncancelled(ctx)}, nil
}

func linkSuccessorTx(ctx context.Context, tx *sql.Tx, oldID, successorID string) error {
	if oldID == "" || successorID == "" || oldID == successorID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_history
		SET related_id = ?
		WHERE memory_id = ? AND phase = ? AND related_id IS NULL
	`, successorID, oldID, phaseDelete); err != nil {
		return fmt.Errorf("record history successor for %s: %w", RefusalArg("memory_id", oldID), err)
	}
	return nil
}

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

func (s *Store) MemoryHistory(ctx context.Context, memoryID string, limit int) ([]HistoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = historyVersionsPerMemory
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, memory_id, project_id, recorded_at, phase, agent, session_id, related_id,
		       merged_content, from_project_id, to_project_id,
		       content, category, importance, resolved_at, source
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
		var agent, sessionID, relatedID, mergedContent, fromProject, toProject, resolvedAt sql.NullString
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.ProjectID, &e.RecordedAt, &e.Phase,
			&agent, &sessionID, &relatedID, &mergedContent, &fromProject, &toProject,
			&e.Content, &e.Category, &e.Importance, &resolvedAt, &e.Source,
		); err != nil {
			return nil, fmt.Errorf("scan memory history: %w", err)
		}
		e.Agent = agent.String
		e.SessionID = sessionID.String
		e.RelatedID = relatedID.String
		e.MergedContent = mergedContent.String
		e.FromProjectID = fromProject.String
		e.ToProjectID = toProject.String
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
