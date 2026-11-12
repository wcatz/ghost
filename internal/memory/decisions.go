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

func (s *Store) RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, companionClamped bool, err error) {

	if err := CheckDecisionArguments(title, decision, rationale, alternatives, tags); err != nil {
		return "", "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "decision-record")
	if err != nil {
		return "", "", false, fmt.Errorf("record decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	decisionID, memoryID, cut, err := recordDecisionTx(ctx, tx, projectID, title, decision, rationale, alternatives, tags)
	if err != nil {
		return "", "", false, err
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

func recordDecisionTx(ctx context.Context, tx *sql.Tx, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, cut bool, err error) {
	altJSON, _ := json.Marshal(alternatives)
	tagJSON, _ := json.Marshal(tags)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO decisions (project_id, title, decision, alternatives, rationale, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, title, decision, string(altJSON), rationale, string(tagJSON)).Scan(&decisionID)
	if err != nil {
		return "", "", false, fmt.Errorf("record decision: %w", err)
	}

	content, cut := decisionCompanionContent(title, decision, rationale)
	err = tx.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags, source_ref)
		VALUES (?, 'decision', ?, 'decision_log', 0.9, ?, ?)
		RETURNING id
	`, projectID, content, string(tagJSON), decisionCompanionRef(decisionID)).Scan(&memoryID)
	if err != nil {
		return "", "", false, fmt.Errorf("record decision memory: %w", err)
	}

	if err := appendHistoryTx(ctx, tx, memoryID, phaseSave, Provenance{}); err != nil {
		return "", "", false, fmt.Errorf("record decision memory history: %w", err)
	}

	if err := appendEvidenceTx(ctx, tx, memoryID, evidenceObserved, Provenance{}, false); err != nil {
		return "", "", false, fmt.Errorf("record decision memory evidence: %w", err)
	}
	return decisionID, memoryID, cut, nil
}

// CheckDecisionArguments is every refusal RecordDecision makes from its
// arguments alone. It is the SAME predicate RecordDecision asks first, exported
// so ghost_decision_record can ask it before it opens the project the decision
// is written to: a refused decision must leave no empty project, and no checkout
// binding, behind it.
func CheckDecisionArguments(title, decision, rationale string, alternatives, tags []string) error {
	if err := rejectSecretFields(
		secretField{"title", title},
		secretField{"decision", decision},
		secretField{"rationale", rationale},
	); err != nil {
		return err
	}
	if err := rejectSecretList("alternatives", alternatives); err != nil {
		return err
	}

	return rejectSecretList("tags", tags)
}

func decisionCompanionContent(title, decision, rationale string) (content string, cut bool) {
	content, cut = ClampContent(fmt.Sprintf("%s: %s. Rationale: %s", title, decision, rationale))
	return content, cut
}

// DecisionRetirement is what a supersede did to the old decision's companion
// memory, so a caller can say so instead of reporting a decisions-row flip as
// if it were the whole of the change.
type DecisionRetirement struct {
	Retired []string

	Declined []string

	Ambiguous []string

	Shared []string
}

func (s *Store) SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error {
	_, err := s.SupersedeDecisionReport(ctx, projectID, oldID, newID)
	return err
}

func (s *Store) SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (DecisionRetirement, error) {
	var out DecisionRetirement
	if oldID == newID {
		return out, fmt.Errorf("supersede decision: a decision cannot supersede itself (%s)", RefusalArg("decision_id", oldID))
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "supersede-decision")
	if err != nil {
		return out, fmt.Errorf("supersede decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	out, err = supersedeDecisionTx(ctx, tx, projectID, oldID, newID)
	if err != nil {
		return DecisionRetirement{}, err
	}

	if err := tx.Commit(); err != nil {
		return DecisionRetirement{}, fmt.Errorf("supersede decision: commit: %w", err)
	}
	lock.reportHold("supersede-decision", time.Now())
	return out, nil
}

type supersedeRefusal struct{ err error }

func (r supersedeRefusal) Error() string { return r.err.Error() }
func (r supersedeRefusal) Unwrap() error { return r.err }

func supersedeDecisionTx(ctx context.Context, tx *sql.Tx, projectID, oldID, newID string) (DecisionRetirement, error) {
	var out DecisionRetirement

	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE id = ? AND project_id = ?`,
		newID, projectID).Scan(&exists); err != nil {
		return out, fmt.Errorf("supersede decision: lookup %s: %w", RefusalArg("decision_id", newID), err)
	}
	if exists == 0 {
		return out, supersedeRefusal{fmt.Errorf("supersede decision: superseding decision %s not found in project %s", RefusalArg("decision_id", newID), RefusalArg("project_id", projectID))}
	}

	successor, err := liveDecisionSuccessor(ctx, tx, projectID, oldID)
	if err != nil {
		return out, fmt.Errorf("supersede decision: lookup successor of %s: %w", RefusalArg("decision_id", oldID), err)
	}
	if successor != "" && successor != newID {
		return out, supersedeRefusal{&DecisionSuccessorError{Successor: successor}}
	}

	var currentSupersededBy string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(superseded_by, '') FROM decisions WHERE id = ? AND project_id = ?`,
		oldID, projectID).Scan(&currentSupersededBy); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("supersede decision: lookup %s: %w", RefusalArg("decision_id", oldID), err)
	}
	priorSuccessor := currentSupersededBy
	if priorSuccessor == newID {
		priorSuccessor = ""
	}

	repoint := currentSupersededBy != "" && currentSupersededBy != newID

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

		return out, supersedeRefusal{fmt.Errorf("supersede decision: decision %s not found in project %s", RefusalArg("decision_id", oldID), RefusalArg("project_id", projectID))}
	}

	out, err = retireDecisionCompanionTx(ctx, tx, projectID, oldID, newID, priorSuccessor, repoint)
	if err != nil {
		return DecisionRetirement{}, fmt.Errorf("supersede decision: retire companion: %w", err)
	}
	return out, nil
}

// RecordedDecision is what RecordDecisionSuperseding wrote.
type RecordedDecision struct {
	DecisionID       string
	MemoryID         string
	CompanionClamped bool

	Retirement DecisionRetirement

	SupersedeErr error
}

func (s *Store) RecordDecisionSuperseding(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string, oldID string) (RecordedDecision, error) {
	var out RecordedDecision
	if err := CheckDecisionArguments(title, decision, rationale, alternatives, tags); err != nil {
		return out, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "decision-record")
	if err != nil {
		return out, fmt.Errorf("record decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	out.DecisionID, out.MemoryID, out.CompanionClamped, err = recordDecisionTx(ctx, tx, projectID, title, decision, rationale, alternatives, tags)
	if err != nil {
		return RecordedDecision{}, err
	}
	if s.afterDecisionInsert != nil {
		s.afterDecisionInsert()
	}

	out.Retirement, err = supersedeDecisionTx(ctx, tx, projectID, oldID, out.DecisionID)
	if err != nil {
		var refusal supersedeRefusal
		if !errors.As(err, &refusal) {
			return RecordedDecision{}, err
		}
		out.Retirement = DecisionRetirement{}
		out.SupersedeErr = err
	}

	if err := tx.Commit(); err != nil {
		return RecordedDecision{}, fmt.Errorf("record decision: commit: %w", err)
	}
	lock.reportHold("decision-record", time.Now())

	if s.onSave != nil {
		s.onSave(projectID)
	}
	return out, nil
}

// DecisionSuccessorError is the refusal SupersedeDecisionReport returns when
// the old decision is already superseded by a different decision that is still
// live. Successor is that decision's id, a stored value; Error renders it through
// SafeToken.
type DecisionSuccessorError struct {
	Successor string
}

func (e *DecisionSuccessorError) Error() string {

	return fmt.Sprintf("decision is already superseded by %s, which is still live", SafeToken(e.Successor))
}

func (s *Store) LiveDecisionSuccessor(ctx context.Context, projectID, decisionID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return liveDecisionSuccessor(ctx, s.db, projectID, decisionID)
}

func liveDecisionSuccessor(ctx context.Context, q queryRower, projectID, decisionID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT s.id FROM decisions d
		JOIN decisions s ON s.id = d.superseded_by AND s.project_id = d.project_id
		WHERE d.id = ? AND d.project_id = ? AND d.status = 'superseded' AND s.status != 'superseded'
	`, decisionID, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func decisionCompanionRef(decisionID string) string { return decisionRefPrefix + decisionID }

const decisionRefSeparator = ","

func decisionRefsInSourceRef(ref string) []string {
	if ref == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(ref, decisionRefSeparator) {
		if strings.HasPrefix(part, decisionRefPrefix) {
			out = append(out, part)
		}
	}
	return out
}

func sourceRefNamesDecision(ref, decisionID string) bool {
	return sourceRefCarriesToken(ref, decisionCompanionRef(decisionID))
}

func joinDecisionRefs(refs []string) string {
	return strings.Join(dedupeDecisionRefs(refs), decisionRefSeparator)
}

func dedupeDecisionRefs(refs []string) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

func extendDecisionRefs(base string, refs []string) string {
	out := base
	for _, ref := range refs {
		if ref == "" || sourceRefCarriesToken(out, ref) {
			continue
		}
		if out == "" {
			out = ref
			continue
		}
		out += decisionRefSeparator + ref
	}
	return out
}

func sourceRefCarriesToken(ref, token string) bool {
	for _, part := range strings.Split(ref, decisionRefSeparator) {
		if part == token {
			return true
		}
	}
	return false
}

var errDecisionRefsOverflow = errors.New("the decision links this merge stands for do not fit in a source_ref")

func boundedDecisionRefs(base string, refs []string) (string, error) {
	if len(refs) == 0 {
		return base, nil
	}
	if out := extendDecisionRefs(base, refs); len(out) <= MaxSourceRefLen {
		return out, nil
	}

	if out := joinDecisionRefs(append(decisionRefsInSourceRef(base), refs...)); len(out) <= MaxSourceRefLen {
		return out, nil
	}
	return "", errDecisionRefsOverflow
}

func preserveDecisionLinks(ref, stored string) (string, error) {
	links := decisionRefsInSourceRef(stored)
	if len(links) == 0 {
		return ref, nil
	}
	out := extendDecisionRefs(ref, links)
	if len(out) > MaxSourceRefLen {
		return "", fmt.Errorf("source_ref %s and the decision links this row carries do not fit in one source_ref (%d-byte bound) — shorten the reference, or omit source_ref to keep the link",
			RefusalArg("source_ref", ref), MaxSourceRefLen)
	}
	return out, nil
}

func decisionLinksOverflowTx(ctx context.Context, tx *sql.Tx, projectID string, ids []string) (links int, over bool, err error) {
	var refs []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		var ref sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT source_ref FROM memories WHERE id = ? AND project_id = ?`, id, projectID).Scan(&ref)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, false, fmt.Errorf("read source_ref of %s: %w", RefusalArg("memory_id", id), err)
		}
		refs = append(refs, ref.String)
	}
	links, over = DecisionLinksOverflow(refs)
	return links, over, nil
}

// DecisionLinksOverflow is decisionLinksOverflowTx's rule over source_refs the
// caller already holds: how many distinct decision links they name between them,
// and whether those links joined exceed MaxSourceRefLen — the merge the replace
// drops before it writes. The reflect accounting calls it with the input rows'
// source_refs, so a dry run lists such a merge as refused instead of as a merge
// the apply will not write, and the two cannot disagree about the rule.
func DecisionLinksOverflow(sourceRefs []string) (links int, over bool) {
	var refs []string
	for _, ref := range sourceRefs {
		refs = append(refs, decisionRefsInSourceRef(ref)...)
	}
	unique := dedupeDecisionRefs(refs)
	return len(unique), len(joinDecisionRefs(unique)) > MaxSourceRefLen
}

func foldedDecisionLinksTx(ctx context.Context, tx *sql.Tx, projectID, target string, sources []string) (int, error) {
	var own sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT source_ref FROM memories WHERE id = ?`, target).Scan(&own); err != nil {
		return 0, fmt.Errorf("read source_ref of %s: %w", RefusalArg("memory_id", target), err)
	}
	refs := decisionRefsInSourceRef(own.String)
	for _, id := range sources {
		if id == "" || id == target {
			continue
		}
		var ref sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT source_ref FROM memories WHERE id = ? AND project_id = ?`, id, projectID).Scan(&ref)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read source_ref of %s: %w", RefusalArg("memory_id", id), err)
		}
		refs = append(refs, decisionRefsInSourceRef(ref.String)...)
	}
	return len(dedupeDecisionRefs(refs)), nil
}

const decisionRefPrefix = "decision:"

func checkReservedSourceRef(ref string) error {
	if len(decisionRefsInSourceRef(ref)) > 0 {
		return fmt.Errorf("source_ref %s is reserved: a %q entry links a decision to its companion memory and is managed by Ghost — omit source_ref to keep the links this row already has, or use a path, commit or URL", RefusalArg("source_ref", ref), decisionRefPrefix)
	}
	return nil
}

func findDecisionCompanionsTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string) (found, retired, ambiguous, shared, declined []string, err error) {

	type linkRow struct {
		id       string
		category string
		source   string
		ref      string
		live     bool
		trusted  bool
		multi    bool
	}

	want := decisionCompanionRef(decisionID)
	rows0, err := tx.QueryContext(ctx, `
		SELECT id, category, source, COALESCE(source_ref, ''), resolved_at IS NULL FROM memories
		WHERE project_id IN (?, ?) AND source_ref LIKE ?
		ORDER BY rowid
	`, projectID, GlobalProjectID, "%"+want+"%")
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("read linked companions: %w", err)
	}
	var linked []linkRow
	for rows0.Next() {
		var r linkRow
		var ref string
		if err := rows0.Scan(&r.id, &r.category, &r.source, &ref, &r.live); err != nil {
			_ = rows0.Close()
			return nil, nil, nil, nil, nil, fmt.Errorf("scan linked companion: %w", err)
		}
		if !sourceRefNamesDecision(ref, decisionID) {
			continue
		}
		r.ref = ref
		r.trusted = r.category == "decision" || r.source == "decision_log" || r.source == "reflection"
		if r.trusted {
			linked = append(linked, r)
			continue
		}
		if r.live {
			declined = append(declined, r.id)
		}
	}
	if err := rows0.Err(); err != nil {
		_ = rows0.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("read linked companions: %w", err)
	}
	_ = rows0.Close()

	for i := range linked {
		multi, err := linksAnotherUnsupersededDecisionTx(ctx, tx, linked[i].ref, decisionID)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		linked[i].multi = multi
	}
	if len(linked) == 0 {
		found, ambiguous, shared, err := findUnlinkedDecisionCompanionTx(ctx, tx, projectID, decisionID)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		return found, nil, ambiguous, shared, declined, nil
	}

	var single []linkRow
	for _, r := range linked {
		if r.multi {
			if r.live {
				shared = append(shared, r.id)
			}
			continue
		}
		single = append(single, r)
	}
	if len(single) == 1 {
		if single[0].live {
			return []string{single[0].id}, nil, nil, shared, declined, nil
		}
		return nil, []string{single[0].id}, nil, shared, declined, nil
	}
	if len(single) > 1 {
		var logged []linkRow
		for _, r := range single {
			if r.source == "decision_log" || (r.source == "reflection" && r.live) {
				logged = append(logged, r)
			}
		}
		if len(logged) == 1 {
			if logged[0].live {
				return []string{logged[0].id}, nil, nil, shared, declined, nil
			}
			return nil, []string{logged[0].id}, nil, shared, declined, nil
		}
		for _, r := range single {
			if r.live {
				ambiguous = append(ambiguous, r.id)
			}
		}
	}
	return nil, nil, ambiguous, shared, declined, nil
}

func retiredBySupersedeTx(ctx context.Context, tx *sql.Tx, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ?`,
			id, phaseSupersede).Scan(&n); err != nil {
			return nil, fmt.Errorf("read companion history: %w", err)
		}
		if n > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

func linksAnotherUnsupersededDecisionTx(ctx context.Context, tx *sql.Tx, ref, decisionID string) (bool, error) {
	want := decisionCompanionRef(decisionID)
	for _, link := range decisionRefsInSourceRef(ref) {
		if link == want {
			continue
		}
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM decisions WHERE id = ?`,
			strings.TrimPrefix(link, decisionRefPrefix)).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read linked decision status: %w", err)
		}
		if status != "superseded" {
			return true, nil
		}
	}
	return false, nil
}

func findUnlinkedDecisionCompanionTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string) (found, ambiguous, shared []string, err error) {
	var title, decision, rationale string
	if err := tx.QueryRowContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE id = ? AND project_id = ?`,
		decisionID, projectID).Scan(&title, &decision, &rationale); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("read decision: %w", err)
	}
	content, _ := decisionCompanionContent(title, decision, rationale)

	rows, err := tx.QueryContext(ctx,
		`SELECT title, decision, rationale FROM decisions WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read project decisions: %w", err)
	}
	same := 0
	for rows.Next() {
		var t, d, r string
		if err := rows.Scan(&t, &d, &r); err != nil {
			_ = rows.Close()
			return nil, nil, nil, fmt.Errorf("scan project decision: %w", err)
		}
		if c, _ := decisionCompanionContent(t, d, r); c == content {
			same++
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, nil, fmt.Errorf("read project decisions: %w", err)
	}
	_ = rows.Close()

	unlinked, err := selectIDs(ctx, tx, `
		SELECT id FROM memories
		WHERE project_id = ? AND source = 'decision_log'
		  AND content = ? AND (source_ref IS NULL OR source_ref = '')
		  AND resolved_at IS NULL
	`, projectID, content)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find unlinked companion: %w", err)
	}
	if same == 1 && len(unlinked) == 1 {
		return unlinked, nil, nil, nil
	}
	if len(unlinked) == 0 {
		return nil, nil, nil, nil
	}
	return nil, unlinked, nil, nil
}

func retireDecisionCompanionTx(ctx context.Context, tx *sql.Tx, projectID, oldID, newID, priorSuccessor string, repoint bool) (DecisionRetirement, error) {
	var out DecisionRetirement
	candidates, retired, ambiguous, shared, declined, err := findDecisionCompanionsTx(ctx, tx, projectID, oldID)
	if err != nil {
		return out, err
	}
	out.Ambiguous = ambiguous
	out.Shared = shared

	out.Declined = append(out.Declined, declined...)

	newCompanion, err := replacementCompanionTx(ctx, tx, projectID, newID, repoint)
	if err != nil {
		return out, err
	}

	retire := candidates
	if len(retire) == 0 {
		if repoint {
			var priorCompanion string
			if priorSuccessor != "" {
				priorCompanion, err = replacementCompanionTx(ctx, tx, projectID, priorSuccessor, true)
				if err != nil {
					return out, err
				}
			}
			retired, err = retiredBySupersedeTx(ctx, tx, retired)
			if err != nil {
				return out, err
			}
			if err := repointRetiredCompanionsTx(ctx, tx, retired, newCompanion, priorCompanion); err != nil {
				return out, err
			}
		}
		return out, nil
	}

	in := strings.TrimSuffix(strings.Repeat("?,", len(retire)), ",")
	args := make([]any, 0, len(retire)+2)
	for _, id := range retire {
		args = append(args, id)
	}

	args = append(args, "", "")

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
		if newCompanion == "" {
			continue
		}

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

func repointRetiredCompanionsTx(ctx context.Context, tx *sql.Tx, retired []string, newCompanion, priorCompanion string) error {
	if len(retired) == 0 {
		return nil
	}
	for _, id := range retired {

		if priorCompanion != "" && priorCompanion != newCompanion {
			active, err := linkIsActive(ctx, tx, priorCompanion, id, "supersedes")
			if err != nil {
				return fmt.Errorf("read stale supersedes link: %w", err)
			}
			if active {
				res, err := tx.ExecContext(ctx, linkInvalidateSQL, priorCompanion, id, "supersedes")
				if err != nil {
					return fmt.Errorf("invalidate stale supersedes link: %w", err)
				}
				n, err := res.RowsAffected()
				if err != nil {

					return fmt.Errorf("invalidate stale supersedes link rows: %w", err)
				}
				if n > 0 {
					if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
						phase:     phaseUnsupersede,
						relatedID: priorCompanion,
					}}, []string{id}); err != nil {
						return err
					}
				}
			}
		}
		if newCompanion == "" {
			continue
		}
		active, err := linkIsActive(ctx, tx, newCompanion, id, "supersedes")
		if err != nil {
			return fmt.Errorf("read existing link: %w", err)
		}
		if active {
			continue
		}
		opposed, err := linkIsActive(ctx, tx, id, newCompanion, "supersedes")
		if err != nil {
			return fmt.Errorf("read existing link: %w", err)
		}
		if opposed {
			continue
		}
		if err := insertLinkTx(ctx, tx, newCompanion, id, "supersedes", 1, "manual", ""); err != nil {
			return fmt.Errorf("link superseded companion: %w", err)
		}
		if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
			phase:     phaseSupersede,
			relatedID: newCompanion,
		}}, []string{id}); err != nil {
			return err
		}
	}
	return nil
}

func replacementCompanionTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string, allowRetired bool) (string, error) {
	found, retired, ambiguous, _, _, err := findDecisionCompanionsTx(ctx, tx, projectID, decisionID)
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0], nil
	}
	if allowRetired && len(retired) > 0 {
		return retired[0], nil
	}
	var logged []string
	for _, id := range ambiguous {
		var source string
		if err := tx.QueryRowContext(ctx, `SELECT source FROM memories WHERE id = ? AND resolved_at IS NULL`, id).Scan(&source); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return "", fmt.Errorf("read replacement claimant %s: %w", RefusalArg("memory_id", id), err)
		}
		if source == "decision_log" {
			logged = append(logged, id)
		}
	}
	if len(logged) == 1 {
		return logged[0], nil
	}
	return "", nil
}

func (s *Store) ListDecisions(ctx context.Context, projectID, status string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inner := `SELECT rowid AS rid, id, project_id, title, decision, alternatives, rationale, status,
	                 COALESCE(superseded_by, '') AS superseded_by, tags, created_at, updated_at
	          FROM decisions WHERE project_id = ?`
	args := []interface{}{projectID}

	if status != "" {
		inner += ` AND status = ?`
		args = append(args, status)
	}

	inner += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	query := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 superseded_by, tags, created_at, updated_at
	          FROM (` + inner + `) ORDER BY (status = 'superseded'), created_at DESC, rid DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return scanDecisions(rows)
}

func (s *Store) LiveDecisions(ctx context.Context, projectID string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 COALESCE(superseded_by, '') AS superseded_by, tags, created_at, updated_at
	          FROM decisions
	          WHERE project_id = ? AND status != 'superseded'
	          ORDER BY (status = 'revisit'), created_at DESC, rowid DESC
	          LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list live decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return scanDecisions(rows)
}

func scanDecisions(rows *sql.Rows) ([]Decision, error) {
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

// DecisionRevisitMarker renders the label the injected decision surfaces put
// beside a decision flagged for reconsideration.
func DecisionRevisitMarker(status string) string {
	if status == "revisit" {
		return " (revisit)"
	}
	return ""
}

func (s *Store) FindLiveDecisionByTitle(ctx context.Context, projectID, title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM decisions
		  WHERE project_id = ? AND status != 'superseded' AND lower(trim(title)) = lower(?)
		  ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectID, title).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
