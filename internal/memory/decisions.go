package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Decision represents an architectural or design decision.
type Decision struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	Title        string   `json:"title"`
	Decision     string   `json:"decision"`
	Alternatives []string `json:"alternatives"`
	Rationale    string   `json:"rationale"`
	Status       string   `json:"status"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	Tags         []string `json:"tags"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// RecordDecision creates a decision and also saves it as a memory. Both
// writes are atomic — if either fails, neither is committed. It returns both
// the decision's own ID (for ListDecisions/status lookups) and the companion
// memory row's ID (needed for ghost_memory_pin/ghost_memory_update, which
// operate on memories, not decisions) — the two are different rows and
// different IDs.
//
// The companion content is the composition "title: decision. Rationale:
// rationale", so it is clamped through ClampContent right before the INSERT:
// two individually sub-cap fields can concatenate over the cap. The returned
// companionClamped reports that composition cut, letting the MCP handler
// warn the caller even when neither field itself was truncated.
func (s *Store) RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, companionClamped bool, err error) {
	// Before the lock and the transaction: a decision writes its text twice
	// (the decisions row and the companion memory built from the same three
	// fields), so a refusal that landed after the first INSERT would have to be
	// undone by the rollback anyway — cheaper and clearer to never start.
	// alternatives is checked as a list because ghost_decisions_list renders it
	// back to the agent, which makes an entry as replayable as the rationale.
	if err := rejectSecretFields(
		secretField{"title", title},
		secretField{"decision", decision},
		secretField{"rationale", rationale},
	); err != nil {
		return "", "", false, err
	}
	if err := rejectSecretList("alternatives", alternatives); err != nil {
		return "", "", false, err
	}
	// And the tags, which are not a lesser field for being short: a decision's
	// tag list is marshalled into BOTH the decisions row and the companion memory
	// row built from the same three fields, and that companion is an ordinary
	// memory — assembled into every search row and quoted into the next reflect
	// prompt. So a token pasted as a tag here is exposed on exactly the two paths
	// the guard above exists for.
	if err := rejectSecretList("tags", tags); err != nil {
		return "", "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	altJSON, _ := json.Marshal(alternatives)
	tagJSON, _ := json.Marshal(tags)

	// A decision writes a memory — the companion row below is an ordinary memory,
	// assembled into every search row and quoted into the next reflect prompt — so
	// losing this transaction to a lock refusal loses a memory, and
	// `ghost_decision_record` is a live tool. Bounded BEGIN retry for the same
	// reason a save has one (issue #671).
	tx, lock, err := s.beginWrite(ctx, "decision-record")
	if err != nil {
		return "", "", false, fmt.Errorf("record decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	err = tx.QueryRowContext(ctx, `
		INSERT INTO decisions (project_id, title, decision, alternatives, rationale, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, title, decision, string(altJSON), rationale, string(tagJSON)).Scan(&decisionID)
	if err != nil {
		return "", "", false, fmt.Errorf("record decision: %w", err)
	}

	// Clamp the COMPOSITION at the canonical site: the field-level clamps in
	// the MCP handler cannot see glue text pushing two sub-cap fields over
	// the cap, and this is the last writer-side stop before the INSERT.
	content, cut := decisionCompanionContent(title, decision, rationale)
	err = tx.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags, source_ref)
		VALUES (?, 'decision', ?, 'decision_log', 0.9, ?, ?)
		RETURNING id
	`, projectID, content, string(tagJSON), decisionCompanionRef(decisionID)).Scan(&memoryID)
	if err != nil {
		return "", "", false, fmt.Errorf("record decision memory: %w", err)
	}

	// The companion memory is a memory like any other, so its first history row
	// is written in the transaction that created it — otherwise a decision log
	// would be the one part of the corpus with no recorded origin.
	if err := appendHistoryTx(ctx, tx, memoryID, phaseSave, Provenance{}); err != nil {
		return "", "", false, fmt.Errorf("record decision memory history: %w", err)
	}
	// And its evidence, in the same transaction for the same reason. The companion
	// is an ordinary memory: search returns it, the next reflect prompt quotes it,
	// and a later save folds against it — so an observation of it is what it is,
	// and leaving it out made the corpus report "no recorded evidence" about a row
	// Ghost itself wrote in the transaction that recorded its origin.
	//
	// The provenance is empty because the decision tool reports none: its arguments
	// carry no agent, session or reference, and inventing one here is the thing
	// these columns exist to prevent. The record is still appended, because the
	// write DID happen.
	if err := appendEvidenceTx(ctx, tx, memoryID, evidenceObserved, Provenance{}, false); err != nil {
		return "", "", false, fmt.Errorf("record decision memory evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", false, fmt.Errorf("record decision: commit: %w", err)
	}
	lock.reportHold("decision-record", time.Now())

	if s.onSave != nil {
		s.onSave(projectID)
	}

	return decisionID, memoryID, cut, nil
}

// decisionCompanionContent is the text RecordDecision writes into the companion
// memory that carries a decision into search and session start — the
// composition "title: decision. Rationale: rationale", clamped through
// ClampContent.
//
// It is a pure function of the three fields the decisions row holds. It is
// NOT how a decision finds its memory: that is the source_ref link
// (decisionCompanionRef), which survives an edit to the memory's text. The
// composition is used only to find a companion written before that link
// existed, and only when the match is unambiguous. Clamping here rather than at
// the field level matters for the same reason it does at the write: two
// individually sub-cap fields can concatenate over the cap.
func decisionCompanionContent(title, decision, rationale string) (content string, cut bool) {
	content, cut = ClampContent(fmt.Sprintf("%s: %s. Rationale: %s", title, decision, rationale))
	return content, cut
}

// DecisionRetirement is what a supersede did to the old decision's companion
// memory, so a caller can say so instead of reporting a decisions-row flip as
// if it were the whole of the change.
type DecisionRetirement struct {
	// Retired are the companion memory ids this call stamped resolved.
	Retired []string
	// Declined are live companion memory ids that were found and left standing
	// because the resolve guard refuses them (pinned, or retention 'persistent').
	// Such a memory keeps being returned by search and session start.
	Declined []string
}

// SupersedeDecision marks oldID as superseded by newID within one project and
// retires the old decision's companion memory in the same transaction. It is
// SupersedeDecisionReport without the report.
func (s *Store) SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error {
	_, err := s.SupersedeDecisionReport(ctx, projectID, oldID, newID)
	return err
}

// SupersedeDecisionReport marks oldID as superseded by newID within one project.
// The decisions table has carried `status` and `superseded_by` columns since
// the schema was written, but nothing ever wrote them — so a reversed decision
// stayed `status: active` forever and ranked alongside the decision that
// replaced it. This is the writer.
//
// It also retires the old decision's companion memory in the same transaction.
// The companion is an ordinary memory, so search and session start kept
// returning the reversed decision as current long after `ghost_decisions_list`
// had dropped it. See retireDecisionCompanionTx for what retiring means and how
// the two rows are matched.
//
// Both IDs must belong to projectID and must differ; a decision cannot
// supersede itself. Superseding an already-superseded decision just repoints
// it, so re-running is safe.
func (s *Store) SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (DecisionRetirement, error) {
	var out DecisionRetirement
	if oldID == newID {
		return out, fmt.Errorf("supersede decision: a decision cannot supersede itself (%s)", oldID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// The status flip and the retirement are ONE transaction, because they are
	// one fact — "this decision is no longer current" — and a reader that saw
	// the first without the second would keep being handed the reversed
	// decision by search and session start. A failure anywhere rolls back both.
	tx, lock, err := s.beginWrite(ctx, "supersede-decision")
	if err != nil {
		return out, fmt.Errorf("supersede decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	// Verify the superseding decision exists in this project before pointing
	// at it — superseded_by is ON DELETE SET NULL, not enforced on insert.
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE id = ? AND project_id = ?`,
		newID, projectID).Scan(&exists); err != nil {
		return out, fmt.Errorf("supersede decision: lookup %s: %w", newID, err)
	}
	if exists == 0 {
		return out, fmt.Errorf("supersede decision: superseding decision %s not found in project %s", newID, projectID)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE decisions
		SET status = 'superseded', superseded_by = ?, updated_at = datetime('now')
		WHERE id = ? AND project_id = ?
	`, newID, oldID, projectID)
	if err != nil {
		return out, fmt.Errorf("supersede decision: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return out, fmt.Errorf("supersede decision: rows affected: %w", err)
	}
	if n == 0 {
		return out, fmt.Errorf("supersede decision: decision %s not found in project %s", oldID, projectID)
	}

	out, err = retireDecisionCompanionTx(ctx, tx, projectID, oldID, newID)
	if err != nil {
		return DecisionRetirement{}, fmt.Errorf("supersede decision: retire companion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DecisionRetirement{}, fmt.Errorf("supersede decision: commit: %w", err)
	}
	lock.reportHold("supersede-decision", time.Now())
	return out, nil
}

// decisionCompanionRef is the stable link from a companion memory back to the
// decision it carries: written into memories.source_ref by RecordDecision, in
// the same INSERT, so it cannot be absent on a row this build wrote and cannot
// drift when the memory's text is later edited. Decision ids survive a portable
// export/import (ImportDecision keeps the artifact's id, and the memory row
// carries source_ref). The lookup does not filter on source, because a default
// import downgrades every memory's source and would otherwise lose the link;
// it requires the link to name exactly one memory instead.
func decisionCompanionRef(decisionID string) string { return "decision:" + decisionID }

// findDecisionCompanionsTx returns the LIVE companion memory ids of decisionID.
//
// The link is memories.source_ref. A companion written before that link
// existed carries none, so for a decision with no linked companion at all the
// lookup falls back to the composed text — but only when it is unambiguous: one
// live unlinked decision_log memory with that text AND exactly one decisions row
// in the project composing to it. Two decisions with identical text, or an
// edited memory, match nothing rather than the wrong row: a stale decision left
// live is recoverable, a wrong memory retired is not.
func findDecisionCompanionsTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string) ([]string, error) {
	// source_ref is a caller-writable column, so the link is trusted only when
	// it is unambiguous: a copied value gives two rows and retires neither.
	var linked int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM memories WHERE project_id = ? AND source_ref = ?
	`, projectID, decisionCompanionRef(decisionID)).Scan(&linked); err != nil {
		return nil, fmt.Errorf("count linked companions: %w", err)
	}
	if linked > 1 {
		return nil, nil
	}
	if linked == 1 {
		return selectIDs(ctx, tx, `
			SELECT id FROM memories
			WHERE project_id = ? AND source_ref = ? AND resolved_at IS NULL
		`, projectID, decisionCompanionRef(decisionID))
	}

	var title, decision, rationale string
	if err := tx.QueryRowContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE id = ? AND project_id = ?`,
		decisionID, projectID).Scan(&title, &decision, &rationale); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read decision: %w", err)
	}
	content, _ := decisionCompanionContent(title, decision, rationale)

	rows, err := tx.QueryContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project decisions: %w", err)
	}
	same := 0
	for rows.Next() {
		var t, d, r string
		if err := rows.Scan(&t, &d, &r); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan project decision: %w", err)
		}
		if c, _ := decisionCompanionContent(t, d, r); c == content {
			same++
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read project decisions: %w", err)
	}
	_ = rows.Close()
	if same != 1 {
		return nil, nil
	}

	unlinked, err := selectIDs(ctx, tx, `
		SELECT id FROM memories
		WHERE project_id = ? AND source = 'decision_log' AND category = 'decision'
		  AND content = ? AND (source_ref IS NULL OR source_ref = '')
		  AND resolved_at IS NULL
	`, projectID, content)
	if err != nil {
		return nil, fmt.Errorf("find unlinked companion: %w", err)
	}
	if len(unlinked) != 1 {
		return nil, nil
	}
	return unlinked, nil
}

// retireDecisionCompanionTx retires the companion memory of a decision that has
// just been superseded, inside the transaction that marked it.
//
// The retirement is the memory-supersede machinery: a resolved_at stamp under
// the same eligibility guard SetResolved and MarkResolved use, a 'supersedes'
// edge from the replacement's companion, and a history row per stamped memory.
// The stamp is what withholds — every read binds resolved_at IS NULL — so search
// and session start stop returning the reversed decision with no model pass.
//
// Rules the shape of this function enforces:
//   - Companions are found by id link (findDecisionCompanionsTx), so a second
//     decision with identical text is never touched.
//   - A memory that is the replacement's own companion is never stamped and never
//     linked to itself.
//   - Every stamped memory gets exactly one history row, in the same transaction:
//     'supersede' naming the replacement's companion when the edge was written,
//     'resolve' with no related id when there is no replacement companion or the
//     reverse edge is already live (so history never claims an edge the graph
//     does not hold).
//   - The edge is 'manual': a caller asserted it, no classifier judged it, so the
//     classifier's re-judgement and withdrawal paths (which select on 'llm') do
//     not own it.
//   - A companion the guard refuses (pinned, persistent) is left live, writes
//     nothing, and is returned in Declined so the caller can say it still ranks.
//
// Idempotence: only live companions are looked up, so a second supersede over
// the same pair finds nothing to stamp and writes no second history row.
func retireDecisionCompanionTx(ctx context.Context, tx *sql.Tx, projectID, oldID, newID string) (DecisionRetirement, error) {
	var out DecisionRetirement
	candidates, err := findDecisionCompanionsTx(ctx, tx, projectID, oldID)
	if err != nil {
		return out, err
	}
	// The replacement's companion names the reason. One with no companion (an
	// imported decision) leaves the reason unstated rather than naming an id
	// that is not a memory.
	newCompanions, err := findDecisionCompanionsTx(ctx, tx, projectID, newID)
	if err != nil {
		return out, err
	}
	newCompanion := ""
	if len(newCompanions) > 0 {
		newCompanion = newCompanions[0]
	}
	retire := candidates[:0:0]
	for _, id := range candidates {
		if id != newCompanion {
			retire = append(retire, id)
		}
	}
	if len(retire) == 0 {
		return out, nil
	}

	in := strings.TrimSuffix(strings.Repeat("?,", len(retire)), ",")
	args := make([]any, 0, len(retire)+2)
	for _, id := range retire {
		args = append(args, id)
	}
	args = append(args, projectID, projectID)

	// The same SELECT/UPDATE pair the two resolve writers issue, so the
	// companion is withheld under exactly the guard a resolve stamp is.
	changed, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedSelectSQL, in), args...)
	if err != nil {
		return out, fmt.Errorf("select companion to retire: %w", err)
	}
	stamped := make(map[string]bool, len(changed))
	for _, id := range changed {
		stamped[id] = true
	}
	for _, id := range retire {
		if !stamped[id] {
			out.Declined = append(out.Declined, id)
		}
	}
	if len(changed) == 0 {
		return out, nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(setResolvedUpdateSQL, in), args...); err != nil {
		return out, fmt.Errorf("retire companion memory: %w", err)
	}

	events := make([]historyEvent, len(changed))
	for i, id := range changed {
		events[i] = historyEvent{phase: phaseResolve}
		if newCompanion == "" || id == newCompanion {
			continue
		}
		// The reverse edge is read inside the transaction, which holds the
		// write lock, so a pair claimed the other way round cannot be written
		// into a cycle here.
		opposed, err := linkIsActive(ctx, tx, id, newCompanion, "supersedes")
		if err != nil {
			return out, fmt.Errorf("read existing link: %w", err)
		}
		if opposed {
			continue
		}
		if err := insertLinkTx(ctx, tx, newCompanion, id, "supersedes", 1, "manual", ""); err != nil {
			return out, fmt.Errorf("link superseded companion: %w", err)
		}
		events[i] = historyEvent{phase: phaseSupersede, relatedID: newCompanion}
	}
	if err := appendHistoryEventsTx(ctx, tx, events, changed); err != nil {
		return out, err
	}
	out.Retired = changed
	return out, nil
}

// ListDecisions returns decisions for a project.
func (s *Store) ListDecisions(ctx context.Context, projectID, status string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inner := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 COALESCE(superseded_by, '') AS superseded_by, tags, created_at, updated_at
	          FROM decisions WHERE project_id = ?`
	args := []interface{}{projectID}

	if status != "" {
		inner += ` AND status = ?`
		args = append(args, status)
	}
	// The limit picks the window (newest first, as it always has), and only
	// then are superseded decisions sorted below live ones. Reordering before
	// truncating would change *which* decisions come back — it would push the
	// oldest superseded ones out of the window entirely, and a superseded
	// decision is exactly what a caller needs to see to know a prior one was
	// reversed. (This is the same truncate-first invariant the search path's
	// decayRank applies to superseded memories.) The outer sort is a no-op when
	// the caller already filtered by status.
	inner += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	query := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 superseded_by, tags, created_at, updated_at
	          FROM (` + inner + `) ORDER BY (status = 'superseded'), created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var decisions []Decision
	for rows.Next() {
		var d Decision
		var altJSON, tagJSON string
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Title, &d.Decision, &altJSON,
			&d.Rationale, &d.Status, &d.SupersededBy, &tagJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(altJSON), &d.Alternatives)
		_ = json.Unmarshal([]byte(tagJSON), &d.Tags)
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}
