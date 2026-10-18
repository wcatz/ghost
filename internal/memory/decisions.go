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
	if err := CheckDecisionArguments(title, decision, rationale, alternatives, tags); err != nil {
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

// CheckDecisionArguments is every refusal RecordDecision makes from its
// arguments alone. It is the SAME predicate RecordDecision asks first, exported
// so ghost_decision_record can ask it before it opens the project the decision
// is written to: a refused decision must leave no empty project, and no checkout
// binding, behind it.
//
// alternatives is checked as a list because ghost_decisions_list renders it
// back to the agent, which makes an entry as replayable as the rationale.
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
	// And the tags, which are not a lesser field for being short: a decision's
	// tag list is marshalled into BOTH the decisions row and the companion memory
	// row built from the same three fields, and that companion is an ordinary
	// memory — assembled into every search row and quoted into the next reflect
	// prompt. So a token pasted as a tag here is exposed on exactly the two paths
	// the guard above exists for.
	return rejectSecretList("tags", tags)
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
	// Declined are live memory ids that a link names and that were left standing
	// rather than retired: the resolve guard refuses the row (pinned, retention
	// 'persistent', or a convention/preference category), or its link is one the
	// trust arms do not recognise (a forged or downgraded one, #1283). Such a
	// memory keeps being returned by search and session start.
	Declined []string
	// Ambiguous are live memory ids that claim the old decision (a link or a
	// composed text more than one memory carries) when no single one could be
	// chosen. Nothing was retired for them and they still rank.
	Ambiguous []string
	// Shared are live memory ids that carry the old decision's link TOGETHER
	// with the link of another decision that is not superseded: a consolidation
	// folded several decision companions into one successor. Such a row is not an
	// extra claimant and is not retired, because withholding it would withhold the
	// other decision it still states; it keeps ranking, so the reversed text is
	// still returned until its text is rewritten or every decision it holds is
	// superseded, at which point the last supersede retires it.
	Shared []string
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
// A companion already retired (by this pair or by an earlier replacement) is
// left exactly as the first retirement recorded it: its edge and history row
// keep naming the decision that retired it, and only the decisions row is
// repointed. History is append-only and ghost_link_withdraw repairs a wrong edge.
//
// Both IDs must belong to projectID and must differ; a decision cannot
// supersede itself. Re-running the same pair is a clean no-op, so re-running is
// safe. A decision already superseded by a DIFFERENT decision that is still
// live (active or revisit) is refused (#1144): the repoint would overwrite the
// only record that the live successor replaced it, leaving two live decisions
// and no link between them. A decision whose successor was itself superseded,
// or no longer exists, has no live successor to lose and is repointed.
func (s *Store) SupersedeDecisionReport(ctx context.Context, projectID, oldID, newID string) (DecisionRetirement, error) {
	var out DecisionRetirement
	if oldID == newID {
		return out, fmt.Errorf("supersede decision: a decision cannot supersede itself (%s)", RefusalArg("decision_id", oldID))
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
		return out, fmt.Errorf("supersede decision: lookup %s: %w", RefusalArg("decision_id", newID), err)
	}
	if exists == 0 {
		return out, fmt.Errorf("supersede decision: superseding decision %s not found in project %s", RefusalArg("decision_id", newID), RefusalArg("project_id", projectID))
	}

	// Inside the transaction, so a second writer cannot slip a repoint past a
	// read-only check the caller made before it wrote anything (#1144).
	successor, err := liveDecisionSuccessor(ctx, tx, projectID, oldID)
	if err != nil {
		return out, fmt.Errorf("supersede decision: lookup successor of %s: %w", RefusalArg("decision_id", oldID), err)
	}
	if successor != "" && successor != newID {
		return out, &DecisionSuccessorError{Successor: successor}
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
		return out, fmt.Errorf("supersede decision: decision %s not found in project %s", RefusalArg("decision_id", oldID), RefusalArg("project_id", projectID))
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

// DecisionSuccessorError is the refusal SupersedeDecisionReport returns when
// the old decision is already superseded by a different decision that is still
// live. Successor is that decision's id, a stored value; Error renders it through
// SafeToken.
type DecisionSuccessorError struct {
	Successor string
}

func (e *DecisionSuccessorError) Error() string {
	// No advice here: the store cannot know whether its caller already wrote the
	// replacement, and "pass supersedes=<successor>" is wrong advice to a caller
	// that did (ghost_decision_record words its own answer for that case).
	return fmt.Sprintf("decision is already superseded by %s, which is still live", SafeToken(e.Successor))
}

// LiveDecisionSuccessor returns the id of the decision that superseded
// decisionID when that decision is still live (active or revisit, the same
// "live" FindLiveDecisionByTitle uses), or "" when decisionID is not
// superseded, its successor was itself superseded, or its successor no longer
// exists. It is the read-only form of the check SupersedeDecisionReport makes in
// its transaction, so a caller can refuse before it writes anything.
func (s *Store) LiveDecisionSuccessor(ctx context.Context, projectID, decisionID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return liveDecisionSuccessor(ctx, s.db, projectID, decisionID)
}

// liveDecisionSuccessor takes a queryRower so it runs on the store's handle and
// inside SupersedeDecisionReport's write transaction alike.
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

// decisionCompanionRef is the stable link from a companion memory back to the
// decision it carries: written into memories.source_ref by RecordDecision, in
// the same INSERT, so it cannot be absent on a row this build wrote and cannot
// drift when the memory's text is later edited. Decision ids survive a portable
// export/import (ImportDecision keeps the artifact's id, and the memory row
// carries source_ref). The lookup does not filter on source, because a default
// import downgrades every memory's source and would otherwise lose the link;
// it requires the link to name exactly one memory instead.
func decisionCompanionRef(decisionID string) string { return decisionRefPrefix + decisionID }

// decisionRefSeparator separates the decision links a consolidation successor
// records when it stands in for more than one decision. A comma cannot occur
// inside a link: RecordDecision mints hex ids, and CheckImportedDecision refuses
// the separator in an imported id, which is the one writer that takes an id
// verbatim. A source_ref that is not a link list splits into one token and is
// matched whole.
const decisionRefSeparator = ","

// decisionRefsInSourceRef returns every decision link a stored source_ref names,
// in order. Most rows carry at most one; a successor that folded several decision
// companions carries all of them, and it is that list — not just its first entry
// — that says which decisions the row stands for.
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

// sourceRefNamesDecision reports whether ref links decisionID.
func sourceRefNamesDecision(ref, decisionID string) bool {
	return sourceRefCarriesToken(ref, decisionCompanionRef(decisionID))
}

// joinDecisionRefs renders the links of a successor that stands for more than one
// decision, in the order they were gathered, with repeats removed.
func joinDecisionRefs(refs []string) string {
	return strings.Join(dedupeDecisionRefs(refs), decisionRefSeparator)
}

// dedupeDecisionRefs is joinDecisionRefs' own list: every distinct non-empty
// link, in the order first seen. joinDecisionRefs renders it, and the overflow
// check counts it, so the number of links a merge stands for and the bytes its
// joined value takes are computed from ONE list.
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

// extendDecisionRefs appends to base every link in refs that base does not
// already carry as a token, in order. A consolidation successor's source_ref is
// the value the ordinary rules chose — the emission's own, else the first
// source's — EXTENDED by the decision links it stands for, never replaced by
// them: replacing discarded the emission's stated value and any ordinary
// reference, and keeping only a single link dropped a lone link behind a
// non-decision first source, so the decision lost its companion.
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

// sourceRefCarriesToken reports whether ref holds token as one of its
// separator-delimited parts — the reader's own matching rule.
func sourceRefCarriesToken(ref, token string) bool {
	for _, part := range strings.Split(ref, decisionRefSeparator) {
		if part == token {
			return true
		}
	}
	return false
}

// errDecisionRefsOverflow is a merge whose decision links alone do not fit in
// MaxSourceRefLen. ReplaceNonManual drops such an emission before it writes
// anything (decisionLinksOverflowTx), so its sources stay live, each still the
// companion of its decision; this error is what a writer that skipped that check
// would meet rather than a silently truncated link list.
var errDecisionRefsOverflow = errors.New("the decision links this merge stands for do not fit in a source_ref")

// boundedDecisionRefs is extendDecisionRefs held to MaxSourceRefLen. The joined
// value is composed here, not copied from a row a writer already bounded, so the
// bound is applied here: a value over it would not re-import (CheckImportedMemory
// bounds source_ref). When the base no longer fits beside the links it gives way
// to them — a lost link is the silent supersede miss, a lost path is not — and
// when the links alone do not fit, the merge is refused rather than truncated.
func boundedDecisionRefs(base string, refs []string) (string, error) {
	if len(refs) == 0 {
		return base, nil
	}
	if out := extendDecisionRefs(base, refs); len(out) <= MaxSourceRefLen {
		return out, nil
	}
	// The base's own links are links too, and a fallback that kept only refs
	// would drop them (the reuse branch's base is the reused row's link).
	if out := joinDecisionRefs(append(decisionRefsInSourceRef(base), refs...)); len(out) <= MaxSourceRefLen {
		return out, nil
	}
	return "", errDecisionRefsOverflow
}

// decisionLinksOverflowTx reports how many distinct decision links the rows ids
// name and whether their joined value would exceed MaxSourceRefLen — the one
// case boundedDecisionRefs refuses. ReplaceNonManual asks it per emission before
// anything is written, so such a merge is dropped and its sources kept instead
// of failing the whole pass (which reflect would then re-propose on every run).
// The LINK COUNT is returned beside the verdict because the caller reports the
// drop as its own reason, and "how many decision links, and the bound" is what
// tells an operator how far over the merge was.
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

// foldedDecisionLinksTx counts the distinct decision links a merge folded into
// the row target stands for: the target's own and those the sources (rows of
// projectID) carry. decisionLinksOverflowTx reads one project only, and a
// promotion's target is in _global, so it is read here by id.
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

// decisionRefPrefix is the source_ref namespace only RecordDecision (and the
// consolidation successor that inherits its link) writes. The writer tools
// (UpsertWithOptions, UpdateMemoryWithOptions) refuse it through
// checkReservedSourceRef, so a caller cannot stamp an unrelated memory with a
// decision's link; ImportMemory does not refuse it, because a restore must keep
// the link — a merged successor's mixed value such as "docs/x.md,decision:<id>"
// included — and the lookup's category and uniqueness checks cover that door.
const decisionRefPrefix = "decision:"

// checkReservedSourceRef refuses a caller-supplied source_ref that carries a
// decision link ANYWHERE the lookup would read one. The guard asks the reader
// itself (decisionRefsInSourceRef) rather than restating its rule, because the
// two drifting apart is the hole: a guard that refused only a leading prefix let
// "runbook.md,decision:<id>" through, and the token reader then took that row
// for the decision's companion.
//
// The refusal names what the caller can DO, because the value it refuses is one
// Ghost itself wrote: a merged successor's source_ref is an ordinary reference
// extended by the decision links it stands for, so a ghost_memory_update that
// reads such a row and hands its own value back is refused. Saying only "use a
// path, commit or URL" leaves that caller with no way to keep the links (#1228).
func checkReservedSourceRef(ref string) error {
	if len(decisionRefsInSourceRef(ref)) > 0 {
		return fmt.Errorf("source_ref %q is reserved: a %q entry links a decision to its companion memory and is managed by Ghost — omit source_ref to keep the links this row already has, or use a path, commit or URL", ref, decisionRefPrefix)
	}
	return nil
}

// findDecisionCompanionsTx returns the LIVE companion memory ids of decisionID.
//
// The link is memories.source_ref. Promotion moves a memory to `_global` and
// leaves that link alone, so the link is read across the decision's project AND
// `_global` — the same widening LinksInto gives a promoted endpoint — and a
// companion promoted before the supersede is still found where it now lives.
// The widening is safe for THIS arm because the link names a decision id, which
// is unique; it is not given to the composed-text fallback below, whose match
// carries no such identity and would otherwise be free to cross into another
// project's promoted memory. A companion written before that link existed
// carries none, so for a decision with no linked companion at all the lookup
// falls back to the composed text — but only when it is unambiguous: one live
// unlinked decision_log memory with that text, whatever its category now, AND
// exactly one decisions row in the project composing to it. Two decisions with
// identical text, or an edited memory, match nothing rather than the wrong row:
// a stale decision left live is recoverable, a wrong memory retired is not.
//
// A live row that carries the link but whose category and source the trust arms
// reject is NOT retired — its link may be forged, or a downgraded copy, and
// burying it would withhold a memory that is not this decision's companion —
// but it is returned in declined, because the reverse is the defect this lookup
// has been fixed twice for: a row the arms cannot see let the supersede answer a
// clean success while a memory that still states the reversed decision kept
// ranking (#1283).
func findDecisionCompanionsTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string) (found, ambiguous, shared, declined []string, err error) {
	// The writer tools refuse the reserved prefix, but an import carries
	// source_ref verbatim, so the link is trusted only for a decision-category
	// memory, one with source 'decision_log', or one with source 'reflection'.
	// The second arm is the companion a caller recategorised with
	// ghost_memory_update (which decision_record itself points them at): it keeps
	// the link and its source, and no writer tool can set that source while a
	// default import downgrades it, so the category it has now is no reason to stop
	// recognising it. The resolve guard then decides — a convention/preference
	// companion is Declined, not skipped.
	//
	// The third arm is the reflect successor of that companion, and it is the one
	// that closes the silent supersede: ReplaceNonManual hands the link to the row
	// that stands in for the companion (the INSERT branch carries it through
	// inheritedClaims, the reuse branch appends the absorbed sources' links to the
	// reused row's own in fillReusedClaimsTx) and writes the row with
	// source 'reflection' and the survivor's own category — which for a merge or
	// for the SQLite tier's cross-category clustering is a category that is not
	// 'decision'. No writer tool can set that source, so a linked row carrying it
	// is a row Ghost's own consolidation wrote over the decision, or an import
	// told to trust the artifact's provenance, which is the same row restored.
	// Without it the lookup found nothing, the decisions row flipped, the reversed
	// decision kept answering search and session start, and the caller was told
	// the supersede was clean.
	//
	// When more than one claims it, the row Ghost itself wrote is the one with
	// source 'decision_log' (RecordDecision) or 'reflection' (a consolidation
	// successor) — an import downgrades the source — and it is the companion only
	// if it is the sole such row. Otherwise nothing is chosen and the live
	// claimants are returned as ambiguous, so the caller can say the decision's
	// memory was left standing. Counting only 'decision_log' here picked the
	// RecordDecision row beside a live reflect successor carrying the same link,
	// retired it alone and answered clean while the successor kept asserting the
	// reversed decision. A reflection row counts only while it is LIVE: a resolved
	// successor states nothing, and counting it made the sole live companion
	// ambiguous, so it kept ranking beside a decision already marked superseded,
	// which no second supersede can reach. The RecordDecision row counts either
	// way, as it always has: when it is the one resolved, nothing is chosen.
	type linkRow struct {
		id       string
		category string
		source   string
		ref      string
		live     bool
		trusted  bool
		multi    bool
	}
	// The link is matched as a TOKEN, not by equality: a successor that folded
	// more than one decision companion carries every link joined by
	// decisionRefSeparator. The LIKE is deliberately broad — it selects candidates
	// cheaply — and sourceRefNamesDecision then confirms the exact token, so a
	// longer id sharing a prefix and a non-link source_ref carrying the substring
	// are both rejected.
	//
	// The query does NOT filter on the trust arms: a row the arms reject is still
	// a row a link names, and it has to reach the declined bucket rather than be
	// filtered out of the read (#1283). The arms are applied in Go, where declining
	// is possible.
	want := decisionCompanionRef(decisionID)
	rows0, err := tx.QueryContext(ctx, `
		SELECT id, category, source, COALESCE(source_ref, ''), resolved_at IS NULL FROM memories
		WHERE project_id IN (?, ?) AND source_ref LIKE ?
		ORDER BY rowid
	`, projectID, GlobalProjectID, "%"+want+"%")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read linked companions: %w", err)
	}
	var linked []linkRow
	for rows0.Next() {
		var r linkRow
		var ref string
		if err := rows0.Scan(&r.id, &r.category, &r.source, &ref, &r.live); err != nil {
			_ = rows0.Close()
			return nil, nil, nil, nil, fmt.Errorf("scan linked companion: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("read linked companions: %w", err)
	}
	_ = rows0.Close()
	// A row is shared only while another decision it links is NOT reversed: once
	// every other decision it holds is superseded it states nothing live, and it is
	// judged as this decision's own companion — otherwise it would be reported as
	// shared on every supersede and never retired. Read after the cursor is closed.
	for i := range linked {
		multi, err := linksAnotherUnsupersededDecisionTx(ctx, tx, linked[i].ref, decisionID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		linked[i].multi = multi
	}
	if len(linked) == 0 {
		found, ambiguous, shared, err := findUnlinkedDecisionCompanionTx(ctx, tx, projectID, decisionID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		return found, ambiguous, shared, declined, nil
	}
	// A live row that stands for more than one decision — another of which is not
	// yet superseded — is not this decision's clean companion: retiring it would
	// withhold whatever other decision it still states. It is reported on its own (shared), never retired and never
	// counted as a competing claimant — it is not an extra, so the ambiguity
	// answer's remedy of deleting the extras would destroy a live decision. The
	// rows that carry this decision's link ALONE are then judged by the rules
	// below exactly as if the shared rows were absent, so a decision_log
	// preference can never pick a shared row.
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
			return []string{single[0].id}, nil, shared, declined, nil
		}
		return nil, nil, shared, declined, nil
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
				return []string{logged[0].id}, nil, shared, declined, nil
			}
			return nil, nil, shared, declined, nil
		}
		for _, r := range single {
			if r.live {
				ambiguous = append(ambiguous, r.id)
			}
		}
	}
	return nil, ambiguous, shared, declined, nil
}

// linksAnotherUnsupersededDecisionTx reports whether ref links a decision other
// than decisionID that is not superseded. A link to a decision this store does
// not hold counts as not superseded: nothing says it was reversed, and retiring a
// row on that absence would withhold a statement nobody withdrew.
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

// findUnlinkedDecisionCompanionTx is the composed-text fallback of
// findDecisionCompanionsTx, for a decision with no linked companion at all. It
// never returns a shared row: a text match carries no decision identity.
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
	candidates, ambiguous, shared, declined, err := findDecisionCompanionsTx(ctx, tx, projectID, oldID)
	if err != nil {
		return out, err
	}
	out.Ambiguous = ambiguous
	out.Shared = shared
	// A live row a link names whose trust arms the read side rejects is not
	// retired, but it must not be invisible either (#1283): it is reported
	// declined so the caller cannot read the supersede as a clean success while
	// a memory that may still state the reversed decision keeps ranking.
	out.Declined = append(out.Declined, declined...)
	// The replacement's companion names the reason. One with no companion (an
	// imported decision) leaves the reason unstated rather than naming an id
	// that is not a memory. See replacementCompanionTx for a replacement whose
	// link more than one live row carries.
	newCompanion, err := replacementCompanionTx(ctx, tx, projectID, newID)
	if err != nil {
		return out, err
	}
	// The replacement's own companion cannot be among the candidates: a link
	// names one memory, so one memory is never linked to both decisions, and the
	// text fallback refuses a text two decisions share. No exclusion is needed
	// here, and none is written, rather than a guard no test can reach.
	retire := candidates
	if len(retire) == 0 {
		return out, nil
	}

	in := strings.TrimSuffix(strings.Repeat("?,", len(retire)), ",")
	args := make([]any, 0, len(retire)+2)
	for _, id := range retire {
		args = append(args, id)
	}
	// No project is bound, exactly as SetResolved passes none: the candidates
	// were already scoped by the selection, which names them by the decision
	// link, and that link reaches a promoted companion in `_global`. Binding the
	// decision's project here is the bug this closes — a promoted companion
	// carries `_global` as its own project_id and would be filtered straight back
	// out.
	args = append(args, "", "")

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
		if newCompanion == "" {
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

// replacementCompanionTx is the row a retired companion's 'supersedes' edge and
// history name: the replacement decision's companion.
//
// It is findDecisionCompanionsTx's answer when that is one row. When it is not —
// the replacement's own link is carried by its RecordDecision row AND by a live
// reflect successor, the two-carrier shape a guard re-add leaves — the lookup's
// ambiguity is a question for the replacement's own supersede, not this one, and
// the row RecordDecision wrote is still the replacement's companion. So the sole
// live 'decision_log' claimant is returned. Without it the old companion was
// retired with a 'resolve' history row and no edge, which the rules above keep
// for a replacement with no companion at all. Only when no single such row
// exists is the edge left out, as before.
func replacementCompanionTx(ctx context.Context, tx *sql.Tx, projectID, decisionID string) (string, error) {
	found, ambiguous, _, _, err := findDecisionCompanionsTx(ctx, tx, projectID, decisionID)
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0], nil
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

	return scanDecisions(rows)
}

// LiveDecisions returns a project's live decisions — active or revisit,
// superseded ones excluded — newest first, with active decisions sorted above
// revisit ones. The order is applied before the window closes, so a revisit row
// never pushes an active one out of the `limit` the caller asked for. It is the
// read the injected decision surfaces use, so a decision flagged for
// reconsideration is shown and labelled rather than hidden, and it is the same
// "not superseded" reading FindLiveDecisionByTitle and LiveDecisionSuccessor
// share.
func (s *Store) LiveDecisions(ctx context.Context, projectID string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 COALESCE(superseded_by, '') AS superseded_by, tags, created_at, updated_at
	          FROM decisions
	          WHERE project_id = ? AND status != 'superseded'
	          ORDER BY (status = 'revisit'), created_at DESC
	          LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list live decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	return scanDecisions(rows)
}

// scanDecisions reads the decision row shape both ListDecisions and
// LiveDecisions select, so the two cannot drift in how a stored row is decoded.
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
//
// It lives HERE, beside the status vocabulary that decides it, because three
// surfaces print that label and the docs promise all three print the SAME one
// (docs/mcp.md, docs/cli.md): a live decision that is not active is context a
// reader should see, so it must be distinguishable from an active one, and two
// copies of the rule would be two rules — the copy nobody edited would be the
// copy that ships the label the docs no longer describe. The dependency runs
// assemble -> memory, so a function the renderers in internal/assemble,
// internal/mcpserver and internal/mcpinit all delegate to cannot live below
// them; that is the arrangement assemble.Token already uses for SafeToken.
//
// An active (or any unrecognised) status adds nothing, so an ordinary row's
// shape is unchanged and only the row that needs marking carries the marker,
// directly after its quoted title.
func DecisionRevisitMarker(status string) string {
	if status == "revisit" {
		return " (revisit)"
	}
	return ""
}

// FindLiveDecisionByTitle returns the id of the newest decision in the project
// that is not superseded (active or revisit) and whose title equals title,
// trimmed and case-insensitively, or "" when none does. It reads only the id
// and title, so the advice it feeds does not scan decision or rationale text
// under the read lock. The case fold is SQLite's, which is ASCII-only.
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
		  ORDER BY created_at DESC LIMIT 1`, projectID, title).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
