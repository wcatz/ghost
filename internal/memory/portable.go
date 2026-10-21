package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PortableProject is a project as the export/import artifact carries it.
//
// It is a distinct type from Project rather than a reuse of it because the
// artifact is a wire format, not a view of a row: it adds the repository remote
// (which is what makes two checkouts of one repository one project, and what
// another machine needs to resolve the project from its own directory) and it
// keeps the timestamps, which no read of the projects table returns today.
type PortableProject struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	RepoRemote string `json:"repo_remote,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// PortableMemory is a memory as the artifact carries it: every column of the
// memories table that describes the memory itself.
//
// Deliberately absent, and each for a reason a reader can check:
//
//   - memory_embeddings. The vector is derived from the content by a local
//     model, not stored knowledge; the async embedding worker rebuilds it for
//     every imported memory, and shipping a blob would be shipping a model's
//     output as if it were the memory.
//   - memory_links. A link is only meaningful between two memories that are
//     both present, so importing edges ahead of their endpoints would either
//     fail the foreign key or fabricate relationships. The linking worker
//     recomputes related edges after import.
//   - resolve_kept_hash. A cache of the resolve classifier's KEEP verdicts,
//     keyed by the content hash, plus the negative audit evidence the verdict
//     was judged with when there was any (#880). Ghost recomputes it, and a
//     stale one would mark a memory it has never classified as reviewed.
//   - memory_history. The change log is not a derived value, so nothing
//     rebuilds it: an imported memory starts a fresh one, holding only the
//     single `import` row ImportMemory writes in the same transaction as the
//     insert, and `ghost history` on a restored memory shows just that row.
//     Carrying the versions would be a format change of its own; `ghost
//     backup` is the copy that keeps them.
//   - memory_flags. An agent's append-only `wrong`/`stale` claim about a
//     memory is not here either, and nothing recomputes an objection somebody
//     made: a restored memory carries no flags, so the resolve and reflect
//     passes lose the count they were handed. That is a format change of its
//     own again, and `ghost backup` is the copy that keeps them.
//
// Every other column is here, including the ones Memory does not expose:
// access_count, last_accessed, the validity triple (valid_from, valid_until,
// verified_at), the pinned flag and the retention tier. Those are what a restore
// has to put back — restoring a memory with a fresh created_at would age it out
// of injection immediately, and restoring it unpinned would silently drop a
// user's pin.
type PortableMemory struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Category  string `json:"category"`
	Content   string `json:"content"`
	// Importance is a pointer because 0 is a real rating and an absent field is
	// not: a record exported with `"importance":0` states it, and re-importing
	// that must store 0 rather than raise it to the column's default. The value
	// is rare because the save tools default an omitted importance —
	// `ghost_memory_save` stores 0.7 and `ghost_save_global` 0.8 — and the
	// writers that bind no default (`Store.Create` and the corpus seeders that
	// share its insert) are not reached by a production save path. A plain
	// float64 cannot tell a stated 0 from a record that states nothing, and
	// folding the two together would promote a 0 to the column's default on
	// every re-import — a silent rewrite of exactly the kind the artifact exists
	// to avoid. nil therefore takes the column default, and only nil.
	Importance   *float64          `json:"importance,omitempty"`
	AccessCount  int               `json:"access_count"`
	LastAccessed *string           `json:"last_accessed,omitempty"`
	Source       string            `json:"source"`
	Tags         []string          `json:"tags,omitempty"`
	Pinned       bool              `json:"pinned"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	ResolvedAt   *string           `json:"resolved_at,omitempty"`
	ValidFrom    *string           `json:"valid_from,omitempty"`
	ValidUntil   *string           `json:"valid_until,omitempty"`
	VerifiedAt   *string           `json:"verified_at,omitempty"`
	Agent        string            `json:"agent,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
	SourceRef    string            `json:"source_ref,omitempty"`
	Confidence   *float64          `json:"confidence,omitempty"`
	Scope        map[string]string `json:"scope,omitempty"`

	// Retention is the tier: how long the memory is wanted and what may be done
	// to it. It is a plain string rather than a pointer, and no `omitempty`,
	// because the two states that would need telling apart fold safely: an
	// absent key and an empty string both mean "the record says nothing", and
	// NormalizeRetention resolves both to the column's own `project` default —
	// the one tier that changes nothing. So a v1 artifact, which states none,
	// imports exactly as it did before the field existed, while every record
	// this build writes carries the tier it holds so a reader of the file does
	// not have to infer the common case.
	//
	// It is CHECKed at its column and ImportMemory normalizes it through
	// NormalizeRetention, so a fourth value is refused by name rather than
	// failing the INSERT; by default an import downgrades it to `project` under
	// the same flag that downgrades source and pin.
	Retention string `json:"retention"`

	// Evidence are the memory_provenance records this memory has: every agent that
	// reported it, every session, every reference, in the order they were
	// reported. It is the reason the artifact is version 2 — a record gained a
	// nested list, which is a shape change rather than another optional field.
	//
	// Nested under the memory rather than written as records of their own type,
	// because a bare evidence line cannot be attributed: memory_provenance names
	// its memory, and the memory is what the artifact orders first. An artifact
	// from a v1 build has none, and the import still works — which is the whole
	// reason v1 is still accepted.
	Evidence []PortableEvidence `json:"evidence,omitempty"`
}

// PortableEvidence is one evidence record as the artifact carries it: the same
// columns the table holds, minus memory_id, which the enclosing memory record
// already names, and minus carried_from, for the reason below.
//
// A distinct type rather than Evidence because the artifact is a wire format and
// not a view of a row: it is flat, it omits the memory id, and it is what a user
// reads and hand-edits. Everything nullable stays nullable — an omitted field is
// "the record does not say", and an import must not fill it in from the memory's
// own columns, which would be a second claim nobody made.
//
// carried_from is dropped rather than carried, and it is the one field a transfer
// cannot honestly move. It names the memory a consolidation carried the record
// FROM — a row that was deleted, which is the whole reason the record was
// carried — so the id resolves against nothing here, and the change log that could
// have explained it is not in the artifact either. Carrying it would hand the
// destination a pointer to a memory it has never heard of, and a reader would have
// no way to tell that from a pointer it could follow. So the destination holds
// these records as its own direct support, which is what they are: this store
// learned the fact, with this support, from a file.
type PortableEvidence struct {
	ID         string   `json:"id,omitempty"`
	Kind       string   `json:"kind"`
	Agent      string   `json:"agent,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	SourceRef  string   `json:"source_ref,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	ObservedAt *string  `json:"observed_at,omitempty"`
	VerifiedAt *string  `json:"verified_at,omitempty"`
}

// PortableLink is one edge of the memory graph as the artifact carries it.
//
// It is a distinct type from Link rather than a reuse of it because the artifact
// is a wire format and not a view of a row: it carries the CLAIM, and leaves out
// the bookkeeping a live row holds — the rowid, and the withdrawal stamp of an
// edge that is no longer live. A withdrawn claim is not carried at all: an
// edge the store invalidated is a claim somebody took back, and an import is
// not a claim about it.
//
// The fields are the ones a reader of the graph acts on. SourceID makes the
// claim and TargetID is what it is about, so the pair is directed and the
// relation says which way it reads. Strength is how strongly the classifier
// judged the pair. Source names the writer that made the claim ('llm' for the
// supersede pass, 'auto' for the linker, 'manual' for a person), and the
// supersede pass reads it to find the edges it owns. CreatedAt is the stamp of
// the JUDGEMENT rather than of the write — the supersede pass compares it with
// an endpoint's updated_at to decide whether the pair has moved since it was
// judged, so an edge restored with the import time instead would read as
// judged now, and the pass would leave it alone.
type PortableLink struct {
	SourceID  string  `json:"source_id"`
	TargetID  string  `json:"target_id"`
	Relation  string  `json:"relation"`
	Strength  float64 `json:"strength"`
	Source    string  `json:"source"`
	CreatedAt string  `json:"created_at"`
}

// CarriedLinkRelations are the relations of the memory graph the portable
// artifact carries, and the rule for which ones they are is the rule the rest
// of the artifact follows: a value travels when nothing recomputes it.
//
// The linking worker writes the `related` edges from the cosine similarity of
// two vectors, and the near-duplicate fold writes the `duplicate` ones from the
// text of two memories. Both are DERIVED, and a worker rebuilds them after an
// import. The supersede pass writes the `supersedes` and `causes` edges, and
// each one is a classifier's judgement about a pair of memories: nothing
// recomputes it, because recomputing it means asking the classifier again.
// Those two travel, because a restore that dropped them would leave a memory a
// `supersedes` edge had demoted undemoted, and would leave the withdraw and
// reassess repairs with no edge to act on.
//
// The other two relations the table's CHECK allows, `contradicts` and
// `elaborates`, have no writer in this build: nothing creates one, so there is
// nothing to carry and nothing to lose.
func CarriedLinkRelations() []string {
	return []string{"supersedes", "causes"}
}

// carriedLinkRelation reports whether a relation is one the artifact carries.
func carriedLinkRelation(relation string) bool {
	for _, r := range CarriedLinkRelations() {
		if r == relation {
			return true
		}
	}
	return false
}

// carriedLinkSource reports whether a link's source column is one the table's
// own CHECK accepts.
func carriedLinkSource(source string) bool {
	switch source {
	case "auto", "llm", "manual":
		return true
	}
	return false
}

// recordedVerification reports whether an imported memory's validity column is a
// real claim that somebody checked it, which is what earns the arrival record its
// verified_at.
//
// anyCarriedVerification reports whether the artifact already carries a record with
// a real verification stamp, whatever its kind.
//
// It is the other half of recordedVerification, and the two together decide whether
// an import records a check or acknowledges one. See the arrival append's comment
// for why the kind is not consulted.
func anyCarriedVerification(records []PortableEvidence) bool {
	for _, e := range records {
		if recordedVerification(e.VerifiedAt) {
			return true
		}
	}
	return false
}

// A non-nil pointer is not enough. The artifact form is documented as something a
// user reads and hand-edits, the column is a POINTER, and `omitempty` drops a nil
// rather than a pointer to "" — so a record can carry `"verified_at": ""`, which
// is what an edit or a templating accident leaves behind. The memory row stores it
// as-is and every reader of that column treats "" as no claim, so the evidence
// record has to agree: a record stamped as verified by a check nobody performed is
// a fabrication, and it would put the support summary at odds with the row the
// same call wrote.
func recordedVerification(at *string) bool {
	return at != nil && *at != ""
}

// ImportOptions is what one import run is doing with the records it is given.
type ImportOptions struct {
	// Apply writes. False is a dry run: every check that does not depend on what
	// the same run has already written runs, the action that would be taken is
	// returned, and nothing is written.
	Apply bool

	// TrustProvenance keeps each memory's own source, pin state and retention
	// tier instead of downgrading them. It is off by default, and the default is
	// the safe one to ship: an artifact is a file that arrived from somewhere,
	// and on its own authority it gets to say that a row is `manual` (the user's
	// own words, and exempt from consolidation) or `builtin` (a rule Ghost ships,
	// which CanonicalOriginSource presents as such) or `pinned` (exempt from
	// consolidation whatever its source). A file a user downloaded would then be
	// able to plant rows that read as the user's own material and as Ghost's own
	// rules, permanently exempt from consolidation and indistinguishable from
	// rows Ghost wrote itself. Off, every imported memory is stamped
	// `onboarding` and unpinned — the same source internal/claudeimport uses for
	// memories imported from outside Ghost at first contact — which keeps it
	// consolidatable and honest about where it came from.
	//
	// The retention tier rides the same flag for the same reason, one field over:
	// `persistent` is the one tier every lifecycle pass spares and `session` is a
	// row scheduled for deletion by its own derived expiry, so a file that
	// arrived from somewhere must not be able to plant either. Off, a stated tier
	// is replaced with `project` — the durable default, which is the direction
	// that keeps a memory rather than the one that ends it.
	//
	// A user importing their OWN export wants the fidelity, and gets it with
	// this flag. That asymmetry is deliberate: the cost of the default being
	// wrong is planted provenance, and the cost of the flag being wrong is a
	// user who has to pass it once.
	TrustProvenance bool
}

// DowngradedSource is the source every imported memory is stamped with unless
// the run was told to trust the artifact's own. `onboarding` is the value
// internal/claudeimport already uses for memories brought in from outside Ghost
// at first contact, and it is the only one of the eight that means "this did not
// originate in a session" without also claiming authorship. It is in the
// memories table's CHECK already, so downgrading needs no migration.
const DowngradedSource = "onboarding"

// validMemorySources is the set the memories table's source CHECK accepts, and
// is what ImportMemory validates against. The CHECK is the schema's own truth;
// this map is the same list spelled out so an import can name the offending
// value instead of failing a statement with SQLite's own wording.
var validMemorySources = map[string]bool{
	"reflection":   true,
	"chat":         true,
	"manual":       true,
	"tool":         true,
	"mcp":          true,
	"onboarding":   true,
	"decision_log": true,
	"builtin":      true,
}

// IsValidSource reports whether src is one of the source values the memories
// table accepts.
func IsValidSource(src string) bool { return validMemorySources[src] }

// validTaskStatuses and validDecisionStatuses mirror the corresponding CHECK
// constraints, for the same reason as validMemorySources.
var (
	validTaskStatuses = map[string]bool{
		"pending": true, "active": true, "done": true, "blocked": true,
	}
	validDecisionStatuses = map[string]bool{
		"active": true, "superseded": true, "revisit": true,
	}
)

// PortableProjects returns every project in id order.
//
// The order is not cosmetic: it makes an export of an unchanged database
// byte-identical, which is what lets a user diff two artifacts or keep a
// checksum over one. ListProjects orders by name instead, which is the right
// order for a directory listing and the wrong one for a diff.
func (s *Store) PortableProjects(ctx context.Context) ([]PortableProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, name, COALESCE(repo_remote, ''), created_at, updated_at
		FROM projects ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("portable projects: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableProject
	for rows.Next() {
		var p PortableProject
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &p.RepoRemote, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PortableMemories returns the portable form of the memories in projectIDs, in
// id order. A nil or empty projectIDs means every project, which is what an
// unfiltered export asks for.
func (s *Store) PortableMemories(ctx context.Context, projectIDs []string) ([]PortableMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at,
		       resolved_at, valid_from, valid_until, verified_at,
		       agent, session_id, source_ref, confidence, scope, retention
		FROM memories`
	// Ghost's own seeds are excluded, and the reason is that no import could ever
	// reconcile them: SeedGlobalMemories writes each seed under the schema's
	// default id, hex(randomblob(16)), which is per-install, so the artifact's
	// copy never equals the destination's own. Every importer dedups by id alone,
	// so a cross-machine import inserts a second row with byte-identical content
	// — with --trust-provenance a second pinned builtin copy of Ghost's shipped
	// rule, surfaced to every session as a rule it already has; by default an
	// onboarding copy that has silently lost the "this is Ghost's own" label,
	// which is the exact downgrade the provenance policy exists to prevent.
	// Nothing repairs it: SeedGlobalMemories skips by content, and no path
	// deletes memories, so the duplicate is permanent.
	//
	// Excluding at the read is the cheap side of the fix. It keeps the import rule
	// simple (dedup by id) and matches what the destination already does on every
	// open — it writes exactly these rows by content — so a restored store ends up
	// with one copy, written by Ghost, not two.
	//
	// The exclusion is on the seed's CONTENT, the way SeedGlobalMemories
	// recognises its own rows — a row in the reserved global project whose content
	// is one of the shipped sentences, shippedSeedContents — and never on its
	// source. `builtin` is a legal memories.source for a user's own row (`ghost
	// import --trust-provenance` stores an artifact's source verbatim), and such a
	// row is the only copy of itself: an exclusion written as project AND source
	// dropped it from every artifact while the export still reported success and
	// exited 0.
	var args []any
	var where string
	if len(projectIDs) > 0 {
		// One "?" per id and the ids themselves only ever in args: the query
		// text is assembled from placeholders alone, never from a value.
		placeholders := make([]string, len(projectIDs))
		for i, id := range projectIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = " WHERE project_id IN (" + strings.Join(placeholders, ",") + ")"
	} else {
		where = " WHERE 1 = 1"
	}
	// The seed contents ride as parameters for the same reason the project ids do:
	// the query text carries only placeholders. A build with no shipped seeds
	// generates no clause rather than an `IN ()`, which SQLite rejects.
	if seedContents := shippedSeedContents(); len(seedContents) > 0 {
		placeholders := make([]string, len(seedContents))
		for i, content := range seedContents {
			placeholders[i] = "?"
			args = append(args, content)
		}
		where += ` AND NOT (project_id = '_global' AND content IN (` + strings.Join(placeholders, ",") + `))`
	}
	query += where
	// Ordered by id for the same byte-reproducibility reason as the projects.
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("portable memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableMemory
	ids := make([]string, 0, 64)
	for rows.Next() {
		m, err := scanPortableMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("portable memories: %w", err)
	}
	// The evidence is a SECOND read, after the rows are closed: the handle is
	// pinned to one connection, so a query issued while these rows are still open
	// would wait for the connection this loop is holding. The memories' own ids
	// bound it, so nothing outside the exported set is read.
	evidence, err := portableEvidence(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Evidence = evidence[out[i].ID]
	}
	return out, nil
}

// portableEvidence reads the evidence records of a set of memories, keyed by
// memory id and ordered oldest-first per memory so the artifact is
// byte-reproducible: an unordered read would emit the same records in a
// different order on each run and every diff of two exports would show churn.
func portableEvidence(ctx context.Context, db *sql.DB, ids []string) (map[string][]PortableEvidence, error) {
	out := make(map[string][]PortableEvidence, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `
			SELECT memory_id, id, kind, agent, session_id, source_ref, confidence,
			       observed_at, verified_at
			FROM memory_provenance
			WHERE memory_id IN (`+strings.Join(ph, ",")+`)
			ORDER BY memory_id, rowid`, args...)
		if err != nil {
			return nil, fmt.Errorf("portable evidence: %w", err)
		}
		for rows.Next() {
			var memoryID string
			var e PortableEvidence
			var agent, sessionID, sourceRef, observedAt, verifiedAt sql.NullString
			var confidence sql.NullFloat64
			if err := rows.Scan(&memoryID, &e.ID, &e.Kind, &agent, &sessionID, &sourceRef,
				&confidence, &observedAt, &verifiedAt); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("portable evidence: %w", err)
			}
			e.Agent = agent.String
			e.SessionID = sessionID.String
			e.SourceRef = sourceRef.String
			if confidence.Valid {
				c := confidence.Float64
				e.Confidence = &c
			}
			if observedAt.Valid {
				v := observedAt.String
				e.ObservedAt = &v
			}
			if verifiedAt.Valid {
				v := verifiedAt.String
				e.VerifiedAt = &v
			}
			out[memoryID] = append(out[memoryID], e)
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("portable evidence: %w", err)
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
}

// PortableLinks returns the edges of the carried relations that TOUCH the given
// projects, in a deterministic order.
//
// "Touch" is either endpoint, and that is the whole of the difference between
// this read and a narrower one. An edge between a project memory and a memory
// of another project is that project's edge as much as the other's — the
// supersede demotion it causes has no project predicate at all — so an export
// that filtered on both endpoints would not know the edge was there, and could
// not say that it left it out. The exporter decides which edges it can carry
// (both endpoints in the artifact), so this read has to hand it every edge in
// the neighbourhood, not only the ones that travel.
//
// Only LIVE edges are read. An edge the store invalidated is a claim somebody
// took back, and the artifact carries claims, not withdrawals.
//
// The order is (source, target, relation), so two exports of an unchanged store
// are byte-identical.
func (s *Store) PortableLinks(ctx context.Context, projectIDs []string) ([]PortableLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(projectIDs) == 0 {
		return nil, nil
	}
	// The relations and the project ids ride as parameters, and the query text
	// carries only placeholders — the same rule the other portable reads follow.
	relations := CarriedLinkRelations()
	relPh := make([]string, len(relations))
	args := make([]any, 0, len(relations)+2*len(projectIDs))
	for i, r := range relations {
		relPh[i] = "?"
		args = append(args, r)
	}
	projPh := make([]string, len(projectIDs))
	for i := range projectIDs {
		projPh[i] = "?"
	}
	for _, id := range projectIDs {
		args = append(args, id)
	}
	for _, id := range projectIDs {
		args = append(args, id)
	}
	projects := strings.Join(projPh, ",")

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at
		FROM memory_links l
		WHERE l.invalidated_at IS NULL
		  AND l.relation IN (`+strings.Join(relPh, ",")+`)
		  AND (l.source_id IN (SELECT id FROM memories WHERE project_id IN (`+projects+`))
		    OR l.target_id IN (SELECT id FROM memories WHERE project_id IN (`+projects+`)))
		ORDER BY l.source_id, l.target_id, l.relation
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("portable links: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableLink
	for rows.Next() {
		var l PortableLink
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("portable links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ImportLink inserts one edge of the graph that the artifact carries, and
// reports created=false when the edge is already in the store.
//
// It never overwrites and never revives. The row is written only when there is
// no row for its (source, target, relation) at all: an edge the store already
// holds is the live copy — the artifact is the older of the two — and an edge
// the store holds WITHDRAWN stays withdrawn, because a withdrawal is an
// explicit act and an import is not a claim about it. `CreateLink`'s upsert
// does the opposite of both (it keeps the higher strength and clears the
// invalidation), which is right for a live judgement and wrong for a restore.
//
// A `supersedes` edge files the same history row `CreateLink` files: the claim
// changes the target's standing, and a corpus whose audit shows a supersede and
// no withdrawal reads as though the stale claim were still live. The row is
// written in the same transaction as the edge, and only when the edge BECOMES
// active — a re-import of an artifact whose edge is already there is a skip and
// writes nothing.
//
// The endpoints are NOT checked here. Whether they exist is a fact about the
// run rather than about the record: a dry run has created nothing yet, so a
// check against the store would refuse the edge in the preview and accept it
// in the run it previews. The caller knows which memories the run lands, and
// the store's own foreign key is the backstop behind that answer.
func (s *Store) ImportLink(ctx context.Context, l PortableLink, apply bool) (created bool, err error) {
	// The lock is taken here, above the presence check, for the reason the other
	// importers give: the presence check is a read whose answer decides whether
	// a write happens, so the two have to be atomic together.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The presence check comes FIRST among the store's own checks, and that order
	// is load-bearing in one direction: an edge the store already holds is a
	// SKIP, never a rejection. Ahead of this check, an edge whose endpoint id
	// this build refuses on shape (a pre-#791 write, a restored snapshot, a
	// hand edit) would make a re-run FAIL over an edge that is not being written
	// and could not be.
	var present int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memory_links WHERE source_id = ? AND target_id = ? AND relation = ?`,
		l.SourceID, l.TargetID, l.Relation,
	).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import link: check whether the edge is present: %w", err)
		}
	} else {
		return false, nil
	}
	// THEN CheckImportedLink, the importer's OWN refusal predicate and the same
	// one `ghost export` calls (#813). It takes the record and never the store.
	if err := CheckImportedLink(l); err != nil {
		return false, err
	}
	if !apply {
		return true, nil
	}

	// The insert and the history row that records it share a transaction, so an
	// edge cannot land without the claim it makes about the target. The
	// conflict clause is DO NOTHING rather than an update: an edge that appeared
	// between the check above and this write is left exactly as it is, live or
	// withdrawn, and the write reports itself as a skip.
	tx, _, err := s.beginWrite(ctx, "import-link")
	if err != nil {
		return false, fmt.Errorf("import link: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))
		ON CONFLICT(source_id, target_id, relation) DO NOTHING
	`, l.SourceID, l.TargetID, l.Relation, l.Strength, l.Source, l.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("import link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("import link rows: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if l.Relation == "supersedes" {
		// The edge makes the claim, so the history row names the memory that
		// made it — the one that replaced the target — exactly as CreateLink's
		// does. Without it, a corpus whose audit shows a supersede and no
		// withdrawal reads as though the stale claim were still live.
		if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
			phase:     phaseSupersede,
			relatedID: l.SourceID,
		}}, []string{l.TargetID}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit import link: %w", err)
	}
	return true, nil
}

// projectCollision reports whether a *different* project already records p's
// path or its repository remote, and says which.
//
// `projects.path` is UNIQUE and `projects.repo_remote` carries a partial UNIQUE
// index created on every open, so a project cannot be inserted beside another
// with the same checkout or the same repository. The remote is compared in
// normalized form because that is the form stored, and an artifact carrying a
// raw `git@host:owner/repo.git` has to collide with the canonical spelling of
// the same repository rather than slip past it.
func (s *Store) projectCollision(ctx context.Context, q Queryer, p PortableProject) error {
	remote := NormalizeRepoRemote(p.RepoRemote)
	// `repo_remote <> ''` is what keeps an absent — or unrecognizable, since
	// NormalizeRepoRemote answers "" for a bare host or a filesystem path — from
	// matching every project that has no repository. `EnsureProject` binds "" for
	// one, and the rest of the store already reads NULL and '' as the same "no
	// remote" (the partial UNIQUE index excludes them for exactly this reason).
	// Without the guard an ordinary import of a remote-less project would be
	// refused, naming an unrelated project and an empty repository.
	var byPath, foundRemote string
	err := q.QueryRowContext(ctx, `
		SELECT
			coalesce((SELECT id FROM projects WHERE path = ? AND id != ? LIMIT 1), ''),
			coalesce((SELECT id FROM projects
			          WHERE repo_remote = ? AND repo_remote <> '' AND id != ? LIMIT 1), '')
	`, p.Path, p.ID, remote, p.ID).Scan(&byPath, &foundRemote)
	if err != nil {
		return fmt.Errorf("import project %s: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	switch {
	case byPath != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records %s as project %s — import into that project, or merge it with `ghost project merge`",
			RefusalArg(ProjectIdentifierLabel, p.ID), p.Path, RefusalArg(ProjectIdentifierLabel, byPath))
	case foundRemote != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records repository %s as project %s — import into that project, or merge it with `ghost project merge`",
			RefusalArg(ProjectIdentifierLabel, p.ID), remote, RefusalArg(ProjectIdentifierLabel, foundRemote))
	}
	return nil
}

// ProjectForCheckout reports the id of a project that already records path or
// repoRemote, or "" when the store holds neither.
//
// It is what lets an artifact from another machine attach its records to the
// project this store already has for the same repository, instead of failing on
// the uniqueness constraint that the two would otherwise collide on. The remote
// is matched in normalized form, so two spellings of one repository are one
// answer, and a project with no remote at all is never matched on one.
func (s *Store) ProjectForCheckout(ctx context.Context, path, repoRemote string) (id, name string, err error) {
	remote := NormalizeRepoRemote(repoRemote)
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Path first, then remote: a recorded path is the stronger claim of the two —
	// it is the directory a session in it resolves to, while a remote only says
	// the two checkouts share a repository.
	if path != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, name FROM projects WHERE path = ? AND id != '_global' LIMIT 1`, path).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("lookup project by path %s: %w", path, err)
		}
		id, name = "", ""
	}
	if remote != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, name FROM projects WHERE repo_remote = ? AND id != '_global' LIMIT 1`, remote).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("lookup project by remote %s: %w", remote, err)
		}
	}
	return "", "", nil
}

// requireProject reports whether projectID exists, naming the record and the
// project when it does not. It is the check the three record importers make
// immediately before their write, so a record naming a project this store does
// not hold is refused with a message that says which field of which record is
// wrong rather than with SQLite's foreign-key wording.
func requireProject(ctx context.Context, db *sql.DB, kind, id, projectID string) error {
	var present int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&present)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%s %s: project %s not found", kind, RefusalArg(kind+"_id", id), RefusalArg("project_id", projectID))
	}
	if err != nil {
		return fmt.Errorf("%s %s: lookup project %s: %w", kind, RefusalArg(kind+"_id", id), RefusalArg("project_id", projectID), err)
	}
	return nil
}

// rowScanner is the one method scanPortableMemory needs, satisfied by both
// *sql.Rows and *sql.Row.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPortableMemory reads one memories row into the portable form. tags and
// scope arrive as JSON text and are decoded the same defensive way the search
// path decodes them: a malformed value costs that one field, never the record,
// because losing a whole memory over a side-channel field would be the wrong
// trade for a restore. retention arrives as the plain tier the column's CHECK
// allows — a store is migrated on every open, so the column is always there —
// and is carried through without a default, because the default is the
// import's to apply and an absent record means something different from a row
// that states `project`.
func scanPortableMemory(sc rowScanner) (PortableMemory, error) {
	var m PortableMemory
	var tagsJSON, scopeJSON sql.NullString
	var agent, sessionID, sourceRef sql.NullString
	var confidence sql.NullFloat64
	// The column is NOT NULL, so the scan target is a plain float64 and the
	// pointer is filled in afterwards: what nil means here is "this build could
	// not record one", which the column never allows.
	var importance float64
	if err := sc.Scan(
		&m.ID, &m.ProjectID, &m.Category, &m.Content, &importance, &m.AccessCount,
		&m.LastAccessed, &m.Source, &tagsJSON, &m.Pinned, &m.CreatedAt, &m.UpdatedAt,
		&m.ResolvedAt, &m.ValidFrom, &m.ValidUntil, &m.VerifiedAt,
		&agent, &sessionID, &sourceRef, &confidence, &scopeJSON, &m.Retention,
	); err != nil {
		return PortableMemory{}, err
	}
	m.Importance = &importance
	if tagsJSON.Valid {
		_ = json.Unmarshal([]byte(tagsJSON.String), &m.Tags)
	}
	m.Agent = agent.String
	m.SessionID = sessionID.String
	m.SourceRef = sourceRef.String
	if confidence.Valid {
		c := confidence.Float64
		m.Confidence = &c
	}
	m.Scope = parseScope(scopeJSON)
	return m, nil
}

// ImportProject inserts a project that the store does not have, and reports
// created=false when the id is already present.
//
// It never updates an existing row. A recorded path is a project's identity for
// every other command, and an artifact exported from another machine is the
// older of the two copies: rewriting a live project's path, name or remote from
// it would re-point a project at a checkout that is no longer its own, and would
// do so from the file most likely to be a stale copy.
//
// The project row is inserted directly rather than through EnsureProject, for
// two reasons the resolver-oriented writers cannot satisfy: the timestamps are
// the artifact's, and the id is the artifact's. Both are identity, not
// preferences, and re-deriving either would make a re-import of the same
// artifact create a second project.
func (s *Store) ImportProject(ctx context.Context, p PortableProject, apply bool) (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// FIRST the id-presence check, and above everything that can refuse. A record
	// already in the store is a SKIP, never a rejection, and that is the portable
	// format's own promise — "never overwrites an id that already exists, so
	// re-running is always safe". Ahead of this check, a store holding an id this
	// build refuses on shape would make a re-run FAIL over a row that is not being
	// written and could not be.
	//
	// Placement above the record checks matters MORE for a project than for the
	// other three records, because a project refusal CASCADES: a failed project
	// step never records its id, so every memory, task and decision naming that
	// project is then rejected for a project "not found". A check that rejected
	// a project the store legitimately holds would therefore take its whole
	// contents with it — which is what the length bound on CheckImportedProjectID
	// did, and why that bound is gone.
	//
	// It names the id in neither arm, deliberately. A message carrying the id here
	// would be a way for a hostile id to reach a report line, and a database
	// failure is not a fact about the record, so the operator does not need the id
	// to act on it.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project: check whether the id is present: %w", err)
		}
	} else {
		return false, nil
	}
	// THEN CheckImportedProject, which is the importer's OWN refusal predicate and
	// the same one `ghost export` calls to decide what it may write (#813). One
	// rule with two callers, so a refusal the importer can make is a refusal the
	// exporter cannot miss, and a new one has exactly two places to be added rather
	// than two that have to be kept in step.
	//
	// The predicate takes the RECORD, never the store, which is why the presence
	// check above is the one thing it cannot do — and why it is here, under the
	// lock, rather than ahead of it. It is pure validation with no I/O, so the
	// critical section stays short either way.
	if err := CheckImportedProject(p); err != nil {
		return false, err
	}
	// The two UNIQUE constraints projects carries are checked here, in both
	// modes, and reported by name. A bare "UNIQUE constraint failed" from the
	// INSERT is the worst possible diagnosis for the common case: project ids are
	// per-install, so an artifact exported on one machine lands on another that
	// already knows the same checkout or the same repository, and the collision
	// is the expected outcome rather than a mistake in the file.
	//
	// Below the predicate rather than inside it, and the reason is the whole of
	// this change: a collision is a fact about THIS store, not about the record.
	// An exporter has no store to compare against, so a predicate that refused on
	// it could not be called by `ghost export` at all — and the exporter must
	// refuse everything the importer can refuse about a record, or the round trip
	// is not a round trip.
	//
	// The remedy is not in this method's gift: adopting the colliding project is
	// a policy decision about where the records should go, so the caller makes it
	// (the portable importer resolves it before ever asking here). What this
	// guarantees is that a collision which survives that decision arrives as a
	// sentence naming the project it collides with, in the dry run as well as the
	// apply — so the preview cannot promise a create the write would refuse.
	if err := s.projectCollision(ctx, s.db, p); err != nil {
		return false, err
	}
	if !apply {
		return true, nil
	}

	// The remote is normalized on the way in, as every other project writer
	// does. The column is UNIQUE (schema v15), so the spelling matters: an
	// artifact that carried a raw `git@host:owner/repo.git` and stored it
	// verbatim would collide with the canonical spelling of the same repository
	// and fail the INSERT with a uniqueness error naming neither.
	//
	// created_at/updated_at default to now, so a record with no timestamp in the
	// artifact (a hand-written one) gets the current time rather than the
	// epoch — and an artifact written by this format always has one.
	// beginWrite rather than execGuardedWrite, for the reason the other three
	// importers give: the presence re-check has to be in the SAME transaction as
	// the INSERT, and s.mu does not close the cross-process race. This is the
	// worst of the four to leave unfixed, because a failed project step never
	// records its id and every memory, task and decision naming that project is
	// then rejected as "project not found" — one concurrent import turns a clean
	// apply into a wholesale rejection of the artifact's contents.
	tx, _, err := s.beginWrite(ctx, "import-project")
	if err != nil {
		return false, fmt.Errorf("import project %s: begin tx: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project %s: re-check presence: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
		}
	} else {
		return false, nil
	}
	// And the collision check again, inside the transaction. projects.path is
	// UNIQUE and repo_remote carries a partial UNIQUE index, so two processes
	// importing artifacts that name the same checkout or repository under
	// DIFFERENT project ids both pass the pre-check and the second one's INSERT
	// dies on the unique index with a bare "UNIQUE constraint failed" — the
	// outcome the pre-check exists to replace with a named sentence. The
	// re-check runs through the transaction, not the pool: the pool is pinned
	// to one connection and the transaction holds it, so a pool read here would
	// deadlock. The pre-check stays for the dry run, which writes nothing.
	if err := s.projectCollision(ctx, tx, p); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote, created_at, updated_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')),
		                   COALESCE(NULLIF(?, ''), datetime('now')))
	`, p.ID, p.Path, p.Name, nullIfEmpty(NormalizeRepoRemote(p.RepoRemote)), p.CreatedAt, p.UpdatedAt); err != nil {
		return false, fmt.Errorf("import project %s: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	// The project's ghost_state row, in the SAME transaction as the projects
	// insert and for the same reason the other two creation routes write it
	// (EnsureProject, createRepoProjectTx): a project row without it is a project
	// whose learned context cannot be recorded. `ghost reflect` writes learned
	// context with a plain UPDATE, so without this row that UPDATE affects 0 rows
	// and used to report success while storing nothing. INSERT OR IGNORE keeps a
	// re-import of an id the store already holds a skip either way, though the
	// presence re-check above already returned for that case.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, p.ID); err != nil {
		return false, fmt.Errorf("import project %s: state row: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import project %s: commit: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	return true, nil
}

// ImportMemory inserts a memory under the id the artifact carries, and reports
// created=false when that id is already present. It never updates an existing
// row, for the reason ImportProject gives: the artifact is a copy to be restored
// from, and the row already in the store is whatever the user has since done
// with it.
//
// The record is validated exactly as an MCP save validates one, because an
// import is another way to reach the same table and must not be the way around
// its rules. Every REFUSAL about the record's own bytes is made by
// CheckImportedMemory, which `ghost export` calls too (#813) — see that function
// for the whole rule and the order. What is left here is the three transformations
// a save also performs and this function therefore still owns:
//
//   - content goes through ClampContent, so a long line from another machine is
//     cut at the same cap with the same marker a normal save would add — and cut
//     is reported so the caller can say so;
//   - importance is clamped to [0,1], the same bound a normal save applies;
//   - the validity triple is normalized to the stored layout by ImportedValidity,
//     the save tool's own reading of RFC 3339 and of a bare date;
//   - each evidence record's own stamps are normalized the same way by
//     ImportedEvidence, and its rating is judged by the same CheckConfidence the
//     memory's own rating is judged by.
//
// None is a refusal, which is why none is in the predicate: a cut content and a
// clamped importance still import, so a predicate that refused them would turn a
// reported adjustment into a lost row. A stamp the rule cannot read IS refused,
// and the predicate makes that refusal by calling the same ImportedValidity and
// ImportedEvidence.
//
// It does not run Upsert's near-duplicate probe. Upsert exists to stop a live
// save from adding a redundant row; a restore is not adding knowledge, it is
// putting back what was there, and folding two rows of the artifact into one
// would silently drop a memory the user chose to keep.
//
// apply=false performs every check and reports the action that would be taken
// without writing, which is what makes a dry run a faithful preview rather than
// a separate code path that can disagree with the real one.
//
// The one check a dry run cannot make here is whether the memory's project
// exists: only the caller knows what the same run has created or will create,
// and a caller importing an artifact in dependency order is exactly that. The
// portable package is such a caller, and it makes the check itself, so a dry run
// and the apply run it previews classify every record the same way.
func (s *Store) ImportMemory(ctx context.Context, m PortableMemory, opts ImportOptions) (created, clamped, downgraded bool, err error) {
	apply := opts.Apply
	// The lock is taken HERE, above the presence check, and that is the whole
	// point of this round. The presence check is a read whose answer decides
	// whether a write happens, so the two have to be atomic together: outside
	// the lock, two imports of the same new id both see it absent, both pass
	// validation, and the second then fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip. A check-then-write is only a
	// check-then-write if nothing can change between the check and the write,
	// and the mutex is what makes that true. ImportProject has always had it
	// this way; the other three importers were moved and this is the cost.
	//
	// The shape check and the field checks used to sit inside the lock, which is
	// where they were before this branch. They are pure validation with no I/O, so
	// the critical section stays short either way; moving them into the predicate
	// above took them out of it for the reason that matters more, which is that
	// `ghost export` has to be able to make the same calls.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The presence check comes first among the store's own checks, and that order is
	// load-bearing in ONE direction, which is why it survived: a record already in
	// the store is a SKIP, never a rejection, and that is the portable format's own
	// promise — "never overwrites an id that already exists, so re-running is always
	// safe" — which docs/invariants.md states for importer guards in the same
	// words. Ahead of this check, a store holding a pre-#791 id (a space, a
	// guillemet, a backtick, an over-long one: a pre-guard write, a restored
	// snapshot, a hand edit) would make a re-run FAIL over a row that is not being
	// written and could not be. The record-level checks were in front of this for a
	// round, and the fix was this position rather than a weakened rule.
	//
	// It is a read, and it is under the lock anyway, because a read that decides
	// whether a write happens is part of the write. See the comment on the lock
	// above for the race this closes.
	//
	// It names the id in neither arm, deliberately. Nothing in this function's
	// record-level refusals names an id any more — they are CheckImportedMemory's,
	// and every message there names a FIELD — so this is the one message left that
	// could, and a database failure is not a fact about the record, so the operator
	// does not need the id to act on it. The artifact line number in the report is
	// what they act on.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, false, false, fmt.Errorf("import memory: check whether the id is present: %w", err)
		}
	} else {
		return false, false, false, nil
	}
	// THEN CheckImportedMemory, the importer's OWN refusal predicate and the same
	// one `ghost export` calls (#813). It takes the record and never the store,
	// which is why the presence check above is the one thing it cannot do.
	//
	// The rule that used to constrain where the record checks sat is now a property
	// of the PREDICATE rather than of this function: the id's SHAPE is checked
	// above every message, and none of those messages names the id. Before, they
	// interpolated it (`memory %s: content is required`) and the ORDER was the only
	// thing stopping a hostile id from reaching a refusal and from there the report
	// line that prints it. Now the order still holds and the message could not carry
	// the payload even if it were moved — two independent reasons, and the second
	// is the one that survives the next person adding a check.
	if err := CheckImportedMemory(m); err != nil {
		return false, false, false, err
	}
	// What is left here, and belongs here, is everything that is a fact about this
	// store or a TRANSFORMATION of the record: the presence check above, the
	// content clamp, the importance clamp, the provenance rewrite, the history
	// check and the pre-write project check. A predicate cannot do any of those,
	// and `ghost export` has no business pretending otherwise.
	content, cut := ClampContent(m.Content)
	// The validity triple in the stored layout, read by the rule the predicate
	// above has just accepted, so the row holds what a save would have stored.
	validity, err := ImportedValidity(m)
	if err != nil {
		return false, false, false, err
	}
	// The evidence records' own stamps and rating, read by the rule the predicate
	// above has just accepted, for the same reason: an evidence row's stamp is a
	// moment a reader interprets, so the stored one is the layout every reader
	// parses rather than the artifact's own spelling of it. A stamp in the stored
	// layout is kept as its raw text, so an export of a store this build wrote
	// re-imports byte-stable.
	evidence := make([]PortableEvidence, len(m.Evidence))
	for i, e := range m.Evidence {
		read, err := ImportedEvidence(i, e)
		if err != nil {
			return false, false, false, &EvidenceRefusalError{Err: err}
		}
		evidence[i] = read
	}
	// A stated importance is clamped to [0,1], the same bound a normal save
	// applies. An unstated one is passed as NULL for the COALESCE to default, and
	// a stated 0 stays 0 — see the field's comment for why the two must not be
	// folded together.
	var importance any
	if m.Importance != nil {
		clamped := *m.Importance
		if clamped < 0 {
			clamped = 0
		}
		if clamped > 1 {
			clamped = 1
		}
		importance = clamped
	}
	// Provenance is rewritten before anything else, so a dry run reports the
	// rewrite it would make and not the one the artifact asked for. The flag is
	// what stands between "I am importing someone's file" and "these are my own
	// rows" — see ImportOptions.TrustProvenance for what each of the two values
	// is protecting.
	//
	// The tier rides the same flag and is the same argument: a `persistent` row
	// is exempt from consolidation, resolve, supersede and pruning, and a
	// `session` row is scheduled for deletion by its own derived expiry — so a
	// file that arrived from somewhere must not be able to plant either. An
	// absent or empty tier is the column's own default, which the untrusted path
	// lands on as well, so a record that states nothing and one that states
	// `project` are the same row under it.
	retention, err := NormalizeRetention(m.Retention)
	if err != nil {
		return false, false, false, err
	}
	source, pinned := m.Source, m.Pinned
	if !opts.TrustProvenance {
		source, pinned = DowngradedSource, false
		retention = RetentionProject
		downgraded = true
	}

	// A free id is not necessarily a NEW id. A memory deleted locally leaves its
	// recorded history behind (that is what the history is for), and the artifact
	// carries ids verbatim, so importing into an id that still has history would
	// splice two unrelated memories' records under one id: the old text under its
	// save/delete pair, then an import row for text that memory never held, with
	// the two sharing one per-memory version budget and one as_of answer (#647).
	// Refused, with the way out named — purging is the redaction path and is
	// exactly the operation that makes the id safe to reuse.
	var historic int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memory_history WHERE memory_id = ? LIMIT 1`, m.ID,
	).Scan(&historic); err == nil {
		return false, false, false, fmt.Errorf(
			"import memory %s: that id has recorded history from a memory that was deleted here, "+
				"and importing would splice two memories' histories under one id — "+
				"purge it first with `ghost history purge %s`, or give the record a new id in the artifact",
			RefusalArg("memory_id", m.ID), RefusalArg("memory_id", m.ID))
	} else if err != sql.ErrNoRows {
		return false, false, false, fmt.Errorf("import memory %s: %w", RefusalArg("memory_id", m.ID), err)
	}

	if !apply {
		return true, cut, downgraded, nil
	}

	// The project is checked immediately before the write, so a memory naming a
	// project the store does not hold is reported as a missing project rather
	// than as a foreign-key failure at the INSERT.
	if err := requireProject(ctx, s.db, "memory", m.ID, m.ProjectID); err != nil {
		return false, false, downgraded, err
	}

	tags, _ := json.Marshal(m.Tags)
	// The timestamp and importance fallbacks live in the SQL, not in Go, for the
	// reason a bound parameter defeats them: created_at is
	// `NOT NULL DEFAULT (datetime('now'))` and importance defaults to 0.5, but
	// a value bound for a column never lets its default apply. Passing "" would
	// store the empty string, and julianday('') is NULL, so the whole decay
	// expression is NULL and the memory sorts last out of every ranked read —
	// invisible to recall while looking perfectly present in the store. So the
	// COALESCE is here, as it already is in the three sibling importers, and a
	// record that omits either field takes the column's own default.
	//
	// The expiry is DERIVED for a session row and nothing else, through the one
	// function every save uses, because the artifact carries no expires_at and a
	// session row restored without one would be a row no prune can ever reach:
	// a row that outlives the conversation tier it was saved under is the growth
	// the tier exists to bound. A restored `project` or `persistent` row takes
	// NULL, which is the unprunable direction and the right one for a durable
	// row. The clock is read once, here, for the same reason a save reads it
	// once: the row and its expiry are one decision.
	expires := sessionExpiry(retention, time.Now())
	// The insert and the history row that records it share a transaction, so an
	// imported memory cannot land without its origin. phaseImport rather than
	// phaseSave because the artifact is a file that arrived from somewhere, and
	// the audit has to be able to tell those rows apart from Ghost's own saves.
	tx, _, err := s.beginWrite(ctx, "import")
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: begin tx: %w", RefusalArg("memory_id", m.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	// Re-check presence INSIDE the write transaction. The pre-check above is
	// under s.mu, which is a per-Store lock and closes nothing across processes:
	// a second `ghost import` on the same database can pass the pre-check, see
	// the id as absent, and fail the INSERT with a UNIQUE constraint error where
	// it used to return a skip. This re-check runs after BEGIN IMMEDIATE has
	// taken SQLite's write lock, so a second process's BEGIN IMMEDIATE blocks
	// until this one commits and then sees the row. The check-then-write is
	// atomic because both halves are in the same transaction, which is the only
	// thing that makes it so — the process mutex was never enough.
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, false, downgraded, fmt.Errorf("import memory: re-check presence: %w", err)
		}
	} else {
		return false, false, downgraded, nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, importance, access_count,
			last_accessed, source, tags, pinned, created_at, updated_at, resolved_at,
			valid_from, valid_until, verified_at, agent, session_id, source_ref,
			confidence, scope, retention, expires_at)
		VALUES (?, ?, ?, ?, COALESCE(?, 0.5), ?, ?, ?, ?, ?,
		        COALESCE(NULLIF(?, ''), datetime('now')),
		        COALESCE(NULLIF(?, ''), datetime('now')), ?,
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ProjectID, m.Category, content, importance, m.AccessCount,
		m.LastAccessed, source, string(tags), pinned,
		m.CreatedAt, m.UpdatedAt, m.ResolvedAt,
		validity.ValidFrom, validity.ValidUntil, validity.VerifiedAt,
		nullIfEmpty(m.Agent), nullIfEmpty(m.SessionID), nullIfEmpty(m.SourceRef),
		m.Confidence, scopeJSON(m.Scope), retention, expires)
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: %w", RefusalArg("memory_id", m.ID), err)
	}
	if err := appendHistoryTx(ctx, tx, m.ID, phaseImport, Provenance{
		Agent:     m.Agent,
		SessionID: m.SessionID,
	}); err != nil {
		return false, false, downgraded, err
	}
	// The evidence the artifact itself carried, then the record of the arrival.
	// The order is the order they happened in, and MemoryProvenance reads forwards:
	// the observations came first, on the machine the artifact was exported from.
	for _, e := range evidence {
		if err := importEvidenceTx(ctx, tx, m.ID, e); err != nil {
			return false, false, downgraded, err
		}
	}
	// The arrival is a claim no carried record makes: this store learned the fact
	// from a file, and a restored corpus that kept only the origin machine's
	// observations would say nothing about its own. Its verified_at is the memory's
	// own when the artifact recorded one — the import preserves that column, and
	// the record is stamped with the store's own clock rather than the artifact's
	// text, so the support summary and the row cannot disagree about whether
	// anybody checked this fact, and no date is copied in from a file.
	//
	// TrustProvenance does not gate it, and neither does it gate the history row
	// above: the flag governs what a row IS (its source, its pin), while who the
	// artifact says observed the fact is the artifact's claim about its own
	// contents, kept as such.
	//
	// And the stamp is conditional, which is the whole of a round trip. A record
	// that already carries a verification says the check HAPPENED — on the origin
	// machine, by whoever did it — so stamping the arrival adds a second stamp for
	// one event and a corpus that crosses a machine boundary N times reports N+1
	// verifications of one check. The number a reader trusts has to survive being
	// moved, so the arrival is stamped only when nothing carried already is.
	//
	// Kind-agnostic, and deliberately: the second hop's artifact carries the FIRST
	// hop's arrival, whose kind is `imported`, so a guard that looked only for
	// `verified` records would re-inflate on every hop after the first. A check
	// already recorded is a check already recorded whatever kind recorded it.
	//
	// A non-empty value, because the artifact form is hand-edited and a record can
	// carry `"verified_at": ""`, which every reader of the column treats as no
	// claim. Empty is not a verification, and treating it as one would suppress a
	// real arrival stamp.
	arrivalStamped := recordedVerification(validity.VerifiedAt) && !anyCarriedVerification(evidence)
	if err := appendEvidenceTx(ctx, tx, m.ID, evidenceImported, Provenance{
		Agent:      m.Agent,
		SessionID:  m.SessionID,
		SourceRef:  m.SourceRef,
		Confidence: m.Confidence,
	}, arrivalStamped); err != nil {
		return false, false, downgraded, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: commit: %w", RefusalArg("memory_id", m.ID), err)
	}
	if s.onSave != nil {
		// The embedding worker is told there is something new to vectorize. A
		// restored memory has no embedding row and the worker's periodic sweep
		// would find it anyway; this only makes it prompt, and it is the same
		// notification a normal save gives.
		s.onSave(m.ProjectID)
	}
	return true, cut, downgraded, nil
}

// ImportTask inserts a task under the artifact's id, reporting created=false
// when the id is present. Like ImportProject it never updates an existing row.
//
// Every refusal about the record's own bytes is made by CheckImportedTask, which
// `ghost export` calls too (#813): the id's shape, project_id, title, the status
// and priority value sets, and the credential guard over title, description and
// notes. A hand-edited record is refused by name rather than by a failed
// statement.
//
// blocked_by is inserted as given: the caller is responsible for having ordered
// the records so a blocker exists before the task that points at it (see the
// portable package's ordering), and a pointer to a task that is in neither the
// artifact nor this store, that the same run rejected, that lands or is held in
// another project than this task, or that is part of a cycle, is dropped there
// and reported rather than written as a foreign key that can never resolve. A
// pointer to a task this store already holds in the task's project is kept.
//
// The project and the blocker are checked immediately before the write, for the
// reason ImportMemory gives. Both checks come after the dry-run return because a
// dry run writes nothing, so its blocker is never in the store to be found; the
// portable plan makes the dry run's equivalent check against the records the run
// has landed so far, which is what keeps the preview and the apply agreeing.
//
// The second return names the fields the caps below cut, in the record's own
// field order, so the portable import can put the loss on the record's report
// line (#1232). It is empty whenever the record was not written — a skip and a
// rejection both say nothing about the caps, because nothing was cut. The names
// are the artifact's field names rather than the caps': a report that says
// `title` was truncated names the field the reader can go and shorten, while
// MaxTitleLen is this build's number.
func (s *Store) ImportTask(ctx context.Context, t Task, apply bool) (created bool, clamped []string, err error) {
	// The lock is taken HERE, above the presence check, for the reason
	// ImportMemory gives in full: a read that decides whether a write happens is
	// part of the write, and outside the lock two imports of the same new id both
	// see it absent and the second fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The id-presence check comes FIRST, for ImportMemory's reason: a record
	// already in the store is a skip, not a rejection, so re-running an import is
	// always safe. It is the one check the record predicate cannot do — it takes a
	// record, not a store — and it is the only message left in this function that
	// could name an id at all, so it names it in neither arm.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import task: check whether the id is present: %w", err)
		}
	} else {
		return false, nil, nil
	}
	// THEN CheckImportedTask, the importer's OWN refusal predicate and the same
	// one `ghost export` calls (#813). One rule with two callers, so a refusal the
	// importer can make is one the exporter cannot miss.
	if err := CheckImportedTask(t); err != nil {
		return false, nil, err
	}
	// What is left here, and belongs here, is everything that is a fact about this
	// store or a TRANSFORMATION of the record — the presence check above and these
	// three clamps, for the reason ImportMemory gives: a predicate cannot transform
	// a record, and `ghost export` calls the same predicate precisely so it can be
	// told what this build ACCEPTS rather than have it decide what it stores.
	//
	// The caps are the ones the task's own writers apply (#1226). Without them an
	// artifact could store 100 KB of title, description and notes, and the row-
	// rendering read tools would hand a single `ghost_task_list` row of about
	// 200 KB back against a 16000-byte budget whose own rule — keep the first row
	// whole rather than drop it — is only sound because a row is bounded by the
	// caps its writer applies. The oversize text reached FTS, embeddings and every
	// export from here too.
	//
	// They are clamps rather than refusals for the reason MaxTitleLen states: a
	// refusal in CheckImportedTask would put the task outside every export as well
	// as outside the import, and the stored text says what was lost anyway — the
	// marker ClampContent appends is IN the row.
	//
	// Above the apply=false return below, so a dry run classifies and previews the
	// apply run exactly, which is the same window the credential guard sits in.
	title := TruncateUTF8(t.Title, MaxTitleLen)
	description, descriptionCut := ClampContent(t.Description)
	notes, notesCut := ClampContent(t.Notes)
	// Which of the three the cut actually touched, answered from the two calls
	// above rather than from the caps re-applied here: a title is cut when
	// TruncateUTF8 returned something other than what it was given (it returns
	// its input unchanged whenever the input fits), and a body field is cut when
	// ClampContent says so. Asking the calls that made the cut is why the report
	// cannot name a field that was not cut.
	if title != t.Title {
		clamped = append(clamped, "title")
	}
	if descriptionCut {
		clamped = append(clamped, "description")
	}
	if notesCut {
		clamped = append(clamped, "notes")
	}
	if !apply {
		return true, clamped, nil
	}

	if err := requireProject(ctx, s.db, "task", t.ID, t.ProjectID); err != nil {
		return false, nil, err
	}
	// blocked_by is a self-reference, so a pointer to a task that is not present
	// is a reference that can never resolve. Naming it here is what turns a raw
	// SQLite foreign-key error into something that says which field of which
	// record is wrong.
	if t.BlockedBy != "" {
		var blocker int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.BlockedBy).Scan(&blocker); err != nil {
			if err == sql.ErrNoRows {
				return false, nil, fmt.Errorf("task %s: blocked_by names task %s, which is not in this store", RefusalArg("task_id", t.ID), RefusalArg("blocked_by", t.BlockedBy))
			}
			return false, nil, fmt.Errorf("import task %s: %w", RefusalArg("task_id", t.ID), err)
		}
	}

	// The write goes through beginWrite rather than execGuardedWrite so the
	// presence re-check can sit in the SAME transaction as the INSERT. See
	// ImportMemory for why: s.mu is a per-Store lock and closes nothing across
	// processes, so a second `ghost import` can pass the pre-check and fail the
	// INSERT with a UNIQUE constraint error. BEGIN IMMEDIATE takes SQLite's write
	// lock, so the re-check inside it is the atomic half of the check-then-write.
	tx, _, err := s.beginWrite(ctx, "import-task")
	if err != nil {
		return false, nil, fmt.Errorf("import task %s: begin tx: %w", RefusalArg("task_id", t.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import task %s: re-check presence: %w", RefusalArg("task_id", t.ID), err)
		}
	} else {
		return false, nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_id, title, description, status, priority,
			blocked_by, branch, pr_number, notes, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''),
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')),
			NULLIF(?, ''))
	`, t.ID, t.ProjectID, title, description, t.Status, t.Priority,
		nullIfEmpty(t.BlockedBy), nullIfEmpty(t.Branch), t.PRNumber, notes,
		t.CreatedAt, t.UpdatedAt, t.CompletedAt); err != nil {
		return false, nil, fmt.Errorf("import task %s: %w", RefusalArg("task_id", t.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("import task %s: commit: %w", RefusalArg("task_id", t.ID), err)
	}
	return true, clamped, nil
}

// HasTask reports whether a task with exactly this id is in the store, in the
// project projectID. The portable import asks it before dropping a blocked_by
// pointer whose target the artifact does not supply: a target the store
// already holds in the project the record is imported under resolves, so the
// pointer is kept. A target in any other project does not: the store's own
// readers follow a pointer only within one project, and a row kept pointing
// across projects is rewritten by deleting the other one. It is a plain read,
// so a dry run's read-only store answers it exactly as the apply's does.
func (s *Store) HasTask(ctx context.Context, projectID, id string) (bool, error) {
	return s.hasRow(ctx, `SELECT 1 FROM tasks WHERE id = ? AND project_id = ?`, id, projectID)
}

// HasDecision is HasTask for a decision's superseded_by.
func (s *Store) HasDecision(ctx context.Context, projectID, id string) (bool, error) {
	return s.hasRow(ctx, `SELECT 1 FROM decisions WHERE id = ? AND project_id = ?`, id, projectID)
}

func (s *Store) hasRow(ctx context.Context, query string, args ...any) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var present int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&present)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ImportDecision inserts a decision under the artifact's id, reporting
// created=false when the id is present. It never updates an existing row.
//
// Unlike RecordDecision it writes no companion memory. That companion exists so
// a decision is retrievable by the same search path as everything else, and a
// restored decision already has that companion among the artifact's memories —
// writing a second one would double every decision on every import.
//
// Every refusal about the record's own bytes is made by CheckImportedDecision,
// which `ghost export` calls too (#813): the id's shape, project_id, title,
// decision, rationale, the status value set, and the credential guard over all
// four plus alternatives and tags.
//
// superseded_by is inserted as given: the caller orders the records so a
// superseding decision exists first, and drops (and reports) a pointer to a
// decision that is in neither the artifact nor this store, that the same run
// rejected, that lands or is held in another project than this decision, or
// that is part of a cycle. As with a task, the project
// and the reference are checked immediately before the write, after the dry-run
// return, and the portable plan makes the dry run's equivalent check.
//
// The second return names the fields the caps below cut, in the record's own
// field order, for the reason ImportTask gives: a decision's stored row shows a
// cut `decision` or `rationale` in the marker ClampContent appends, but nothing
// at all about a cut title or about alternatives dropped past the twentieth,
// so the report is the only place the loss can be said (#1232).
func (s *Store) ImportDecision(ctx context.Context, d Decision, apply bool) (created bool, clamped []string, err error) {
	// The lock is taken HERE, above the presence check, for the reason
	// ImportMemory gives in full: a read that decides whether a write happens is
	// part of the write, and outside the lock two imports of the same new id both
	// see it absent and the second fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The presence check is first for ImportMemory's reason: an already-present id
	// is a skip, not a rejection, so re-running an import is always safe. It names
	// the id in neither arm — see the comment above ImportTask's.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import decision: check whether the id is present: %w", err)
		}
	} else {
		return false, nil, nil
	}
	// THEN CheckImportedDecision, the importer's OWN refusal predicate and the same
	// one `ghost export` calls (#813). Every refusal about the record's own bytes
	// lives there — including alternatives and tags, which are lists because they
	// are: ghost_decisions_list renders alternatives back to the agent, and the
	// tags column arrives from an artifact file and is re-emitted into the next
	// one, so an entry in either is as replayable as the rationale beside it.
	if err := CheckImportedDecision(d); err != nil {
		return false, nil, err
	}
	// The caps `ghost_decision_record` applies, for the reason ImportTask gives in
	// full (#1226): every one of them bounds a row the row-rendering read tools
	// show whole, and `ghost_decisions_list` renders a decision's title, decision,
	// rationale and its WHOLE alternatives list as one row — so an artifact
	// carrying 100 KB of each field, or 25 oversized alternatives, produced a
	// single row of megabytes that no listing argument could narrow, against a
	// 16000-byte budget.
	//
	// Clamps rather than refusals, for the reason MaxTitleLen states, and above the
	// apply=false return below so a dry run previews the apply run exactly.
	title := TruncateUTF8(d.Title, MaxTitleLen)
	decision, decisionCut := ClampContent(d.Decision)
	rationale, rationaleCut := ClampContent(d.Rationale)
	alternatives := BoundAlternatives(d.Alternatives)
	// Which four the cut actually touched, from the same calls that made it, for
	// ImportTask's reason. `alternatives` is asked separately rather than compared
	// with what went in, because BoundAlternatives answers a list either way and
	// only AlternativesClamped — the predicate it decides on — knows whether the
	// list it returned is a cut one.
	if title != d.Title {
		clamped = append(clamped, "title")
	}
	if decisionCut {
		clamped = append(clamped, "decision")
	}
	if rationaleCut {
		clamped = append(clamped, "rationale")
	}
	if AlternativesClamped(d.Alternatives) {
		clamped = append(clamped, "alternatives")
	}
	if !apply {
		return true, clamped, nil
	}

	if err := requireProject(ctx, s.db, "decision", d.ID, d.ProjectID); err != nil {
		return false, nil, err
	}
	// Superseded_by is a self-reference checked for the same reason blocked_by is
	// on a task: a pointer to a decision this store does not hold can never
	// resolve, and the error has to name the field.
	if d.SupersededBy != "" {
		var superseder int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.SupersededBy).Scan(&superseder); err != nil {
			if err == sql.ErrNoRows {
				return false, nil, fmt.Errorf("decision %s: superseded_by names decision %s, which is not in this store", RefusalArg("decision_id", d.ID), RefusalArg("superseded_by", d.SupersededBy))
			}
			return false, nil, fmt.Errorf("import decision %s: %w", RefusalArg("decision_id", d.ID), err)
		}
	}

	alts, _ := json.Marshal(alternatives)
	tags, _ := json.Marshal(d.Tags)
	// beginWrite rather than execGuardedWrite, for the reason ImportTask gives:
	// the presence re-check has to be in the same transaction as the INSERT,
	// and s.mu does not close the cross-process race.
	tx, _, err := s.beginWrite(ctx, "import-decision")
	if err != nil {
		return false, nil, fmt.Errorf("import decision %s: begin tx: %w", RefusalArg("decision_id", d.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import decision %s: re-check presence: %w", RefusalArg("decision_id", d.ID), err)
		}
	} else {
		return false, nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale,
			status, superseded_by, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?,
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')))
	`, d.ID, d.ProjectID, title, decision, string(alts), rationale,
		d.Status, nullIfEmpty(d.SupersededBy), string(tags), d.CreatedAt, d.UpdatedAt); err != nil {
		return false, nil, fmt.Errorf("import decision %s: %w", RefusalArg("decision_id", d.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("import decision %s: commit: %w", RefusalArg("decision_id", d.ID), err)
	}
	return true, clamped, nil
}
