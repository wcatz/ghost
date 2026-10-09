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
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'decision', ?, 'decision_log', 0.9, ?)
		RETURNING id
	`, projectID, content, string(tagJSON)).Scan(&memoryID)
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
// It is a pure function of the three fields the decisions row holds, and that
// is the ONLY thing identifying which memory belongs to which decision: the
// two rows carry no id pointing at each other, on either side. So this
// composition is the mapping in both directions — RecordDecision uses it to
// write the memory, and SupersedeDecision recomposes it from the stored row
// to find that memory again. Clamping here rather than at the field level is
// load-bearing for the same reason it is at the write: two individually
// sub-cap fields can concatenate over the cap, and a composition clamped to a
// different string than the one stored is a lookup that finds nothing.
func decisionCompanionContent(title, decision, rationale string) (content string, cut bool) {
	content, cut = ClampContent(fmt.Sprintf("%s: %s. Rationale: %s", title, decision, rationale))
	return content, cut
}

// SupersedeDecision marks oldID as superseded by newID within one project.
// The decisions table has carried `status` and `superseded_by` columns since
// the schema was written, but nothing ever wrote them — so a reversed decision
// stayed `status: active` forever and ranked alongside the decision that
// replaced it. This is the writer.
//
// It also retires the old decision's companion memory in the same transaction.
// The decisions row was the only half being written, and the half that mattered
// most was the one left behind: the companion is an ordinary memory, so search
// and session start kept returning the reversed decision as current long after
// `ghost_decisions_list` had dropped it. Retiring it runs the same machinery a
// memory supersede runs — see retireDecisionCompanionTx for what that means and
// for how the two rows are matched.
//
// Both IDs must belong to projectID and must differ; a decision cannot
// supersede itself. Superseding an already-superseded decision just repoints
// it, so re-running is safe.
func (s *Store) SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("supersede decision: a decision cannot supersede itself (%s)", oldID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// The status flip and the retirement are ONE transaction, because they are
	// one fact — "this decision is no longer current" — and a reader that saw
	// the first without the second would keep being handed the reversed
	// decision by search and session start. The whole of the companion lookup,
	// the resolved_at stamp and the history row therefore run inside this
	// transaction, and a failure anywhere rolls back both halves.
	tx, lock, err := s.beginWrite(ctx, "supersede-decision")
	if err != nil {
		return fmt.Errorf("supersede decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	// Verify the superseding decision exists in this project before pointing
	// at it — superseded_by is ON DELETE SET NULL, not enforced on insert.
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE id = ? AND project_id = ?`,
		newID, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("supersede decision: lookup %s: %w", newID, err)
	}
	if exists == 0 {
		return fmt.Errorf("supersede decision: superseding decision %s not found in project %s", newID, projectID)
	}

	// Both decisions are READ, not merely counted: the old one's three fields
	// compose the text of the memory this retires, and the new one's compose
	// the text of the memory that replaces it — the id that names the reason.
	var oldTitle, oldDecision, oldRationale string
	if err := tx.QueryRowContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE id = ? AND project_id = ?`,
		oldID, projectID).Scan(&oldTitle, &oldDecision, &oldRationale); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("supersede decision: decision %s not found in project %s", oldID, projectID)
		}
		return fmt.Errorf("supersede decision: lookup %s: %w", oldID, err)
	}
	var newTitle, newDecision, newRationale string
	if err := tx.QueryRowContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE id = ? AND project_id = ?`,
		newID, projectID).Scan(&newTitle, &newDecision, &newRationale); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("supersede decision: superseding decision %s not found in project %s", newID, projectID)
		}
		return fmt.Errorf("supersede decision: lookup %s: %w", newID, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE decisions
		SET status = 'superseded', superseded_by = ?, updated_at = datetime('now')
		WHERE id = ? AND project_id = ?
	`, newID, oldID, projectID)
	if err != nil {
		return fmt.Errorf("supersede decision: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("supersede decision: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("supersede decision: decision %s not found in project %s", oldID, projectID)
	}

	oldContent, _ := decisionCompanionContent(oldTitle, oldDecision, oldRationale)
	newContent, _ := decisionCompanionContent(newTitle, newDecision, newRationale)
	if err := s.retireDecisionCompanionTx(ctx, tx, projectID, oldContent, newContent); err != nil {
		return fmt.Errorf("supersede decision: retire companion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("supersede decision: commit: %w", err)
	}
	lock.reportHold("supersede-decision", time.Now())
	return nil
}

// retireDecisionCompanionTx retires the companion memory of a decision that has
// just been superseded, inside the transaction that marked it.
//
// The companion is found by CONTENT, because the composed text is the only
// link between a decisions row and its memory (see decisionCompanionContent).
// That is exact for every decision recorded by this build and every one
// restored from a portable artifact, since both the row and the memory were
// clamped by the same function. It is exact only up to a tie when the same
// decision was recorded twice in one project: two companions then assert the
// same claim, and the newest of them is the REPLACEMENT's own whenever the
// replacement was recorded after the reversed decision — the shape
// supersession has, and the one shape in which superseding would otherwise
// withhold the claim that just won. So with more than one match the newest is
// left standing, and the claim the new decision asserts survives.
//
// The retirement is the memory-supersede machinery, all three parts of it:
// a 'supersedes' edge from the replacement's companion to this one, a
// resolved_at stamp under the same eligibility guard SetResolved and
// MarkResolved use, and a 'supersede' history row naming the replacement's
// companion as the reason. The stamp is what withholds — every read binds
// resolved_at IS NULL — so search and session start stop returning the
// reversed decision with no LLM pass, and the edge plus the row are what
// answer "what replaced it" afterwards.
//
// The stamp re-checks resolved_at IS NULL, which is what makes a second
// SupersedeDecision over the same pair a no-op rather than a second history
// row for the same retirement. A companion that cannot be stamped — one that is
// pinned or persistent — is excluded from the stamp (the same guard that
// SetResolved and MarkResolved use), so it takes no edge, records no history
// row and registers no demotion. This is intentional: a decision companion
// that the user has explicitly pinned or set to retention-exempt status is a
// non-negotiable rule and must not be silently retired by a supersede pass.
func (s *Store) retireDecisionCompanionTx(ctx context.Context, tx *sql.Tx, projectID, oldContent, newContent string) error {
	var companionIDs []string
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM memories
		WHERE project_id = ? AND source = 'decision_log' AND category = 'decision'
		  AND content = ? AND resolved_at IS NULL
		ORDER BY created_at DESC, rowid DESC
	`, projectID, oldContent)
	if err != nil {
		return fmt.Errorf("find companion memory: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan companion memory: %w", err)
		}
		companionIDs = append(companionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read companion memories: %w", err)
	}
	if len(companionIDs) == 0 {
		// Nothing to retire, and not an error: a decision restored from a
		// portable artifact carries no companion of its own, and one whose
		// companion was deleted has nothing left to withhold. The decisions
		// row is the half that always lands.
		return nil
	}
	if len(companionIDs) > 1 {
		// Newest first, so the replacement's own companion — recorded after
		// the decision it reverses — is the one left standing.
		companionIDs = companionIDs[1:]
	}

	// The replacement's companion, whose id names the reason. A replacement
	// with no companion (an imported decision) leaves the reason unstated
	// rather than naming an id that is not a memory.
	var newCompanion string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM memories
		WHERE project_id = ? AND source = 'decision_log' AND category = 'decision'
		  AND content = ? AND resolved_at IS NULL
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, projectID, newContent).Scan(&newCompanion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("find replacement companion memory: %w", err)
	}

	in := strings.TrimSuffix(strings.Repeat("?,", len(companionIDs)), ",")
	args := make([]interface{}, 0, len(companionIDs)+2)
	for _, id := range companionIDs {
		args = append(args, id)
	}
	args = append(args, projectID, projectID)

	// The same SELECT/UPDATE pair the two resolve writers issue, so the
	// companion is withheld under exactly the guard a resolve stamp is —
	// unpinned, non-exempt, unresolved. Idempotent by that guard: a second
	// supersede selects nothing, stamps nothing and records nothing.
	changed, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedSelectSQL, in), args...)
	if err != nil {
		return fmt.Errorf("select companion to retire: %w", err)
	}
	if len(changed) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(setResolvedUpdateSQL, in), args...); err != nil {
		return fmt.Errorf("retire companion memory: %w", err)
	}

	if newCompanion != "" {
		for _, id := range changed {
			// The reverse edge is read inside the transaction, which holds the
			// write lock, so a pair claimed the other way round cannot be
			// written into a cycle here.
			opposed, err := linkIsActive(ctx, tx, id, newCompanion, "supersedes")
			if err != nil {
				return fmt.Errorf("read existing link: %w", err)
			}
			if opposed {
				continue
			}
			if err := insertLinkTx(ctx, tx, newCompanion, id, "supersedes", 1, "llm", ""); err != nil {
				return fmt.Errorf("link superseded companion: %w", err)
			}
		}
		// One history row per retired companion, in the phase vocabulary a
		// supersede already writes and naming the memory that replaced it.
		events := make([]historyEvent, len(changed))
		for i := range events {
			events[i] = historyEvent{phase: phaseSupersede, relatedID: newCompanion}
		}
		if err := appendHistoryEventsTx(ctx, tx, events, changed); err != nil {
			return err
		}
	}
	return nil
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
