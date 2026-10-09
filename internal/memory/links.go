package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Link is an edge between two memories. 'related' links are symmetric and
// stored with (source_id < target_id) normalized ordering; directed relations
// (supersedes, contradicts, elaborates, causes) preserve their direction.
type Link struct {
	SourceID      string
	TargetID      string
	Relation      string
	Strength      float32
	Source        string
	CreatedAt     string
	InvalidatedAt *string
}

// symmetricRelations are stored in normalized (min, max) ID order so A→B and
// B→A collapse to one row.
var symmetricRelations = map[string]bool{"related": true}

// CreateLink inserts an edge between two memories. Idempotent: re-inserting
// an existing (source, target, relation) keeps the higher strength and
// clears any invalidation.
//
// A `supersedes` edge is also recorded in the target's history: it is the claim
// "this memory is no longer current", and it is a change to that memory's
// standing even though none of its columns move. The edge lands on the target
// alone — the source's state did not change, and a history row filed under it
// would be a record of a write that did not happen. The row is written when the
// edge BECOMES active, not on every re-write of an edge that already is (see
// below). A `related` edge from the linker is not recorded at all: nothing about
// either memory's currency is asserted by an edge the linker adds on cosine
// similarity alone.
func (s *Store) CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error {
	return s.CreateLinkJudged(ctx, sourceID, targetID, relation, strength, source, "")
}

// CreateLinkJudged is CreateLink for a caller that made a JUDGEMENT about the
// pair, and the only difference between the two a caller can see is the stamp it
// writes: judgedAt rather than the write clock. `ghost supersede` is that
// caller, and linkInsertSQL sets out why the stamp has to be the freshness of
// what was judged rather than when the row landed — in short, the two are
// minutes apart, and an edit landing between them is one no verdict was given
// for.
//
// An empty judgedAt is CreateLink, and every other caller wants exactly that:
// the linker's `related` edges, the bench seeders and the restore paths make no
// judgement, so the write clock is the honest stamp.
func (s *Store) CreateLinkJudged(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) error {
	_, err := s.createLink(ctx, sourceID, targetID, relation, strength, source, judgedAt, false)
	return err
}

// CreateLinkUnopposed is CreateLinkJudged for a DIRECTED relation whose pair may
// be claimed in at most one direction: it writes the edge only while no live edge
// already runs the OTHER way, and reports whether it wrote.
//
// It is a separate method rather than a rule inside CreateLinkJudged because the
// two spellings want different things of a cycle, and only one of them is
// PRODUCING one. `ghost supersede` is the only writer of a directed edge in
// production, and the state it must not create is a pair live in both directions
// — which demotes BOTH endpoints in ranking and leaves neither edge able to
// withdraw the other (#778). The bench seeders and the cycle fixtures, on the
// other hand, have to be able to WRITE that state: it is what a real store already
// holds, put there by a pass before #778, and both the repair pass and the tests
// that prove the repair works can only describe it by building it. A rule inside
// the shared writer would take that ability away from the only callers that need
// it.
//
// So the guard is in the caller's contract, and it is ATOMIC with the write: the
// reverse edge is read inside the same BEGIN IMMEDIATE transaction that inserts
// this one, and BEGIN IMMEDIATE takes the database write lock before the first
// statement. Two processes writing the two directions of one pair are therefore
// serialised — the second one's read happens after the first one's commit, sees
// the edge, and writes nothing (#806). A check outside the transaction would not
// close this at all: both writers could read "no reverse edge" and both would
// insert. That is also why this is not a lifecycle lock: a lock is a convention
// between the processes that take it, while this is a property of the graph that
// has to hold whatever the callers turn out to be — and `ghost supersede` is a
// command an operator runs twice, so "the other process took the lock first" is
// not a thing either of them can be asked to guarantee.
//
// A refusal is a normal outcome rather than an error, and it is the SAFE
// direction: the edge that exists is the one an earlier writer put there, and the
// pair is re-offered on the next pass, which reads the live edge and judges the
// pair in ITS direction. The caller is told, for the two reasons every other
// refusal in that pass is counted — a pass that declined a write and reported the
// totals of one that found nothing to do reads as "nothing was skipped", and a
// report that claims a link this run did not write is the one thing a report
// whose whole job is auditability may not be.
func (s *Store) CreateLinkUnopposed(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) (bool, error) {
	if symmetricRelations[relation] {
		// A symmetric relation is stored in normalized order, so its two
		// directions are ONE row and "is the reverse already live" is a question
		// about the row this call would write. Refusing by name beats answering
		// it: the caller asked for a directed claim.
		return false, fmt.Errorf("create link: relation %q is symmetric and cannot be unopposed", relation)
	}
	return s.createLink(ctx, sourceID, targetID, relation, strength, source, judgedAt, true)
}

// createLink is the one implementation both spellings above share, so the history
// row a 'supersedes' edge files and the transaction that files it cannot drift
// apart between them. requireUnopposed is the whole difference, and it is read
// and acted on INSIDE the transaction (see CreateLinkUnopposed) — which is why it
// is a parameter here and not something a wrapper could add around the call.
func (s *Store) createLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string, requireUnopposed bool) (bool, error) {
	if sourceID == targetID {
		return false, fmt.Errorf("create link: self-links not allowed (id %s)", sourceID)
	}
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// A 'supersedes' edge, and every guarded write, takes the explicit
	// transaction: the first because its history row has to share one with the
	// edge, the second because the reverse-edge check has to share one with the
	// insert. Only the ungated non-supersedes write is left on the autocommit
	// path, which is where it has always been.
	if requireUnopposed || relation == "supersedes" {
		return s.createLinkTx(ctx, sourceID, targetID, relation, strength, source, judgedAt, requireUnopposed)
	}

	if _, err := s.execGuardedWrite(ctx, "create-link-autocommit", linkInsertSQL,
		sourceID, targetID, relation, strength, source, judgedAt); err != nil {
		return false, fmt.Errorf("create link: %w", err)
	}
	return true, nil
}

// createLinkTx writes one edge inside a write transaction, refusing the write
// when the pair is already claimed the other way round. A false return with a nil
// error IS the refusal, and never a failure: nothing went wrong, and the graph is
// left in a state the next pass can act on.
func (s *Store) createLinkTx(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string, requireUnopposed bool) (bool, error) {
	// One transaction for the edge and its history row: a supersedes edge
	// with no record of it, or a record of one that was never written, are
	// both states this call must not be able to commit.
	tx, lock, err := s.beginWrite(ctx, "create-link")
	if err != nil {
		return false, fmt.Errorf("begin create link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if requireUnopposed {
		// The REVERSE edge, read through the transaction and never through the
		// pool: this transaction holds the write lock from its first statement,
		// so no other process can commit this pair's other direction between the
		// read and the insert below. A pool read would not merely be racy here,
		// it would deadlock — MaxOpenConns(1) means the open transaction is
		// holding the only connection the read would need.
		opposed, err := linkIsActive(ctx, tx, targetID, sourceID, relation)
		if err != nil {
			return false, err
		}
		if opposed {
			// No write and no history row: nothing became true, so there is
			// nothing to record, and the transaction rolls back. It reports
			// nothing to the write-lock seam either, which measures committed
			// transactions — see writeLock.reportHold.
			return false, nil
		}
	}

	if relation == "supersedes" {
		// Whether the edge is already active is read inside the transaction,
		// which holds the write lock from its first statement, so the decision
		// below cannot be overtaken by another process between the read and the
		// write. It is read rather than inferred from RowsAffected because the
		// upsert must keep raising an existing edge's strength even when the
		// edge's validity does not change — a guard in the conflict clause
		// would have had to choose one of the two.
		active, err := linkIsActive(ctx, tx, sourceID, targetID, relation)
		if err != nil {
			return false, err
		}
		if err := insertLinkTx(ctx, tx, sourceID, targetID, relation, strength, source, judgedAt); err != nil {
			return false, err
		}
		// Only when the edge BECOMES active. `ghost supersede` re-writes a pair
		// whose endpoint moved since the edge was written, and re-writing a
		// live edge changes nothing about either memory: a history row would
		// repeat the previous state byte-for-byte, spend one of the
		// per-memory version slots, and — after enough passes — prune the real
		// save/update/reflect versions this table exists to keep. Re-activating
		// an invalidated edge IS a change, and is recorded.
		if !active {
			// relatedID is the memory whose edge makes the claim — the one that
			// replaced this row. Without it a history says a memory went stale
			// without saying to what, which is the half of the sentence an audit
			// is asking for.
			if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
				phase:     phaseSupersede,
				relatedID: sourceID,
			}}, []string{targetID}); err != nil {
				return false, err
			}
		}
	} else if err := insertLinkTx(ctx, tx, sourceID, targetID, relation, strength, source, judgedAt); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit create link: %w", err)
	}
	lock.reportHold("create-link", time.Now())
	return true, nil
}

// linkInsertSQL is the upsert CreateLink performs: re-inserting an existing
// (source, target, relation) keeps the higher strength, clears any
// invalidation, and stamps created_at from judgedAt. Shared by the autocommit
// path and the transaction the supersede history row shares, so the two cannot
// record different edges.
//
// created_at on a link row is not a claim about when the edge entered the graph.
// Its one reader is `ghost supersede`'s skip-if-unchanged test, which compares
// each endpoint's updated_at against it to decide whether the pair has moved
// since it was last JUDGED — so it is a judgement stamp, and a re-judgement that
// leaves it where it was is why an edge whose endpoint was ever edited was
// re-billed on every pass after it (#784). The history row is keyed on the edge
// BECOMING active rather than on this column, so the stamp writes no history.
//
// judgedAt is passed in rather than taken from a clock, and that is the whole of
// the subtlety. `ghost supersede` reads both endpoints at the TOP of a pass and
// then spends a classify call that takes seconds to minutes, and an edit landing
// in that window — reflect's consolidation rewrite, a save through a live
// `ghost mcp` — carries an updated_at newer than the text the classifier actually
// saw but older than the moment the write lands. Stamping the WRITE would cover
// that edit instead of stopping short of it, and the pair would then sit quiet
// against text no verdict was ever given for, so the caller stamps the freshness
// of what it judged. An empty judgedAt means no judgement was made here and falls
// back to the write clock, which is what every caller but supersede wants (the
// linker's `related` edges, which nothing reads this column for, and the bench
// seeders).
const linkInsertSQL = `
	INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at)
	VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))
	ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
		strength = MAX(strength, excluded.strength),
		invalidated_at = NULL,
		created_at = excluded.created_at
`

// insertLinkTx is linkInsertSQL inside an open transaction.
func insertLinkTx(ctx context.Context, tx *sql.Tx, sourceID, targetID, relation string, strength float32, source, judgedAt string) error {
	if _, err := tx.ExecContext(ctx, linkInsertSQL,
		sourceID, targetID, relation, strength, source, judgedAt); err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	return nil
}

// linkIsActive reports whether a live (not invalidated) edge already exists for
// this exact (source, target, relation). Served by the primary key, so it is one
// index lookup.
func linkIsActive(ctx context.Context, tx *sql.Tx, sourceID, targetID, relation string) (bool, error) {
	var live int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM memory_links
		WHERE source_id = ? AND target_id = ? AND relation = ? AND invalidated_at IS NULL
	`, sourceID, targetID, relation).Scan(&live)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read existing link: %w", err)
	}
	return live == 1, nil
}

// GetLinks returns all valid (non-invalidated) links touching a memory,
// from either endpoint, strongest first.
func (s *Store) GetLinks(ctx context.Context, memoryID string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
		FROM memory_links
		WHERE (source_id = ? OR target_id = ?) AND invalidated_at IS NULL
		ORDER BY strength DESC
	`, memoryID, memoryID)
	if err != nil {
		return nil, fmt.Errorf("get links: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// MarkLinkScanned records that the linking worker has processed this memory.
// Rows cascade-delete with the memory, so reflection churn resets scans
// automatically and the worker self-heals.
func (s *Store) MarkLinkScanned(ctx context.Context, memoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.execGuardedWrite(ctx, "mark-link-scanned", `
		INSERT INTO link_scans (memory_id) VALUES (?)
		ON CONFLICT(memory_id) DO UPDATE SET scanned_at = datetime('now')
	`, memoryID)
	if err != nil {
		return fmt.Errorf("mark link scanned: %w", err)
	}
	return nil
}

// UnscannedEmbeddedMemoryIDs returns memories that have an embedding but have
// not yet been processed by the linking worker. A vector from another vector
// space is not returned: it is a vector this process cannot compare anything
// with, and including it would both pair it across spaces and spend the
// memory's one scan slot, so the link would never be built after the row is
// re-embedded. The identity applied is the store's configured one
// (SetEmbeddingIdentity) — the linker has no embedding model of its own, so it
// asks the store what it can compare; with none configured, every row is
// returned as before.
func (s *Store) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`
	args := []any{projectID, limit}
	if s.embeddingIdentity != "" {
		query = `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL AND e.model = ?
		ORDER BY m.created_at DESC
		LIMIT ?
		`
		args = []any{projectID, s.embeddingIdentity, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("unscanned embedded memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetEmbedding returns the stored embedding vector for a memory.
//
// A row recorded under a different vector identity than the configured one
// returns (nil, nil): this process has no usable vector for that memory, and
// handing the blob back would invite a cosine against vectors from another
// space — an arbitrary number that then becomes a `related` edge, a
// `supersedes` candidate, or both. A nil vector means "not available yet":
// the caller must leave the memory for a later pass, which is what it gets
// once the embedding worker has rewritten it in the configured space.
func (s *Store) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	var model string
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding, model FROM memory_embeddings WHERE memory_id = ?
	`, memoryID).Scan(&blob, &model)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if identity := s.embeddingIdentity; identity != "" && model != identity {
		return nil, nil
	}
	return bytesToFloat32s(blob), nil
}

// EmbeddingStats returns how many memories have embeddings, how many of those
// embeddings are stale, and the total memory count, across all projects. Used
// by health/status diagnostics.
//
// "Has an embedding" means "has one this process can search with": with a
// vector identity configured, rows recorded under another one are counted as
// not embedded, because a re-embed is still pending for them and reporting
// them as covered is what would let `ghost mcp status` print a passing
// coverage line while the vector leg returns nothing.
//
// Those stale rows are returned separately rather than folded into a single
// gap, because the two halves of the gap are different diagnoses: stale rows
// are work the re-embed worker has queued and will finish, while a row with no
// vector at all has never been embedded — the state of a worker that never ran
// or of an install whose embedding was only just enabled. Both are reported by
// `ghost mcp status` and `ghost_health`.
//
// The three counts come from one statement so they describe one snapshot.
// Three separate COUNTs could straddle a write and report embedded+stale >
// total, which in a coverage line reads as corruption rather than as a race.
func (s *Store) EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	identity := s.embeddingIdentity
	const countQuery = `
		SELECT
			(SELECT COUNT(*) FROM memory_embeddings WHERE ? = '' OR model = ?),
			(SELECT COUNT(*) FROM memory_embeddings WHERE ? <> '' AND model <> ?),
			(SELECT COUNT(*) FROM memories)
	`
	if err = s.db.QueryRowContext(ctx, countQuery, identity, identity, identity, identity).
		Scan(&embedded, &stale, &total); err != nil {
		return 0, 0, 0, fmt.Errorf("count embeddings: %w", err)
	}
	return embedded, stale, total, nil
}

// LinkStats returns the number of valid links and link-scanned memories
// across all projects. Used by health/status diagnostics.
func (s *Store) LinkStats(ctx context.Context) (links, scans int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_links WHERE invalidated_at IS NULL`).Scan(&links); err != nil {
		return 0, 0, fmt.Errorf("count links: %w", err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM link_scans`).Scan(&scans); err != nil {
		return 0, 0, fmt.Errorf("count link scans: %w", err)
	}
	return links, scans, nil
}

// SupersedesWithin returns the valid 'supersedes' edges whose BOTH endpoints
// are in ids: each pair is [superseder, superseded]. Used by ranking to demote
// a memory only when its actual replacement co-occurs in the same result set,
// so a superseded fact is never buried when its successor isn't even present.
//
// A pair whose endpoints' scopes conflict is withheld, on the same terms as
// SupersedePenalties: two claims about different places never stood in a
// supersession relation, and an edge that says they did is not a verdict any
// reader may act on. The edge stays in the graph.
//
// This function has no production caller today — SupersedePenalties is the
// reader ranking uses, and it applies the same exemption. The guard is here so
// that a caller added later inherits the store's rule rather than
// contradicting it, which is the trap best_practices.md warns about when an
// existing store API is exposed through a new path.
func (s *Store) SupersedesWithin(ctx context.Context, ids []string) ([][2]string, error) {
	if len(ids) < 2 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = 'supersedes' AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("supersedes within: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var pairs [][2]string
	for rows.Next() {
		var src, tgt string
		var sourceScope, targetScope sql.NullString
		if err := rows.Scan(&src, &tgt, &sourceScope, &targetScope); err != nil {
			return nil, err
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}
		pairs = append(pairs, [2]string{src, tgt})
	}
	return pairs, rows.Err()
}

// LinksByRelationSource returns all valid (non-invalidated) links of the given
// relation and source whose BOTH endpoints belong to projectID or to `_global`.
// Used by ghost supersede to find previously-created 'supersedes'/llm links so it
// can reclassify them alongside freshly-discovered candidate pairs, by the repair
// pass to load what it re-judges, and by resolve's supersedes piggyback and its
// repair pass's floor.
//
// The scope is BOTH endpoints, and it is deliberately STRICTER than
// LinksInto's, because the two reads answer different questions. That
// one is a targeted withdrawal, and the caller has already NAMED the target: the
// question is which edges bury this memory, and the memory's own project may
// answer it however the edge came to be sourced. This one feeds a PASS, and a pass
// judges a pair and then WRITES links on it -- so an edge with an endpoint outside
// the caller's project is that other project's pair, and a pass that judged it
// would be writing on a graph it was not run against.
//
// That half is what a promotion creates. `ghost_memory_promote` and `ghost reflect
// --promote-globals` move a memory into `_global` and KEEP its links, so a live
// edge is left with its source in the shared scope and its target where it was --
// and a predicate on the source alone put that edge outside all four of these
// readers at once (#786). SupersedePenalties, which carries no project predicate
// at all, went on demoting the target, while the repair pass could not load the
// edge to withdraw it and the floor let go of the resolution the demotion was
// still justifying. The TARGET half is what fixes that, and what keeps the pass
// inside its project: from `p` the promoted edge's two endpoints are the shared
// scope and `p`, so it loads; from `q` the target is a `p` memory, so it does not.
//
// Nothing downstream of this read re-filters the target either, which is why the
// exclusion half is load-bearing rather than tidiness: Run's apply block
// invalidates AND creates links on whatever pair it loaded, so a read that
// admitted an edge whose target belongs to a third project would let
// `ghost supersede p --reassess` delete a claim in `q`'s graph.
//
// `_global` is in scope from every project and NO other project is, which is the
// same rule the ref resolver follows (MemoryIDsByIDPrefix): a memory in the
// shared scope is visible in every project, and a project is a boundary.
func (s *Store) LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE (source_mem.project_id = ? OR source_mem.project_id = ?)
		  AND (target_mem.project_id = ? OR target_mem.project_id = ?)
		  AND l.relation = ? AND l.source = ? AND l.invalidated_at IS NULL
	`, projectID, GlobalProjectID, projectID, GlobalProjectID, relation, source)
	if err != nil {
		return nil, fmt.Errorf("links by relation source: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// LinksInto returns the live edges whose TARGET is memoryID and whose EITHER
// endpoint belongs to projectID or to `_global`, for the given relation — or for
// the two directed relations a targeted withdrawal can name ('supersedes' and
// 'causes', 'supersedes' first) when relation is empty.
//
// It is the read a targeted withdrawal decides on, and it answers two questions
// with one query: whether the exact edge the operator named is live, and — when
// it is not — which edges DO point at that memory, so a refusal can name them
// instead of leaving the reader to grep the graph.
//
// The ownership rule is "either endpoint is ours", and each half is about a
// different memory of the pair. The SOURCE is the one making the claim, so an
// edge sourced from the shared scope is a claim every project can see and every
// project can withdraw — that is what a `ghost_memory_promote`d source needs, and
// it is what the predicate used to be for (#786: a promoted source put the edge
// outside every project at once, so nothing could withdraw it). The TARGET is the
// one being buried, so the project that owns it is entitled to name the edge
// burying it however that edge came to be sourced: from the shared scope, or from
// a project that no longer holds the memory `ghost project merge` moved.
//
// `_global` is in scope from every project, and a project is not in scope from
// another. An edge with both endpoints in a third project is that project's edge:
// neither withdrawable from here nor visible through here, which is what keeps a
// project-scoped call from reporting (or changing) a graph that is not its own.
//
// The TARGET half is reachable from a `_global` call only, and that asymmetry is
// not an oversight. `_global`'s refs resolve without a project predicate (see
// MemoryIDsByIDPrefixAnyProject), so a project-scoped call cannot even NAME a pair
// whose source is in another project, while a `_global` call can name one whose
// target is the promoted memory — which is the case the half exists for. A caller
// reaching this read with a project it does not own therefore still gets nothing
// it could not have had.
//
// The relation is a parameter rather than a fixed column, and that is #833
// rather than a generality: a 'causes' edge became load-bearing when its
// DIRECTION started deciding which way a pair is judged, so a pair whose only
// edge is a 'causes' one is now a pair a person has to be able to name. It was
// unreachable through a read hardcoded to 'supersedes' — a pair the ordinary
// pass refuses forever (the #778 tie: both rows share updated_at and created_at,
// so there is no chronology to orient it by) had no repair at all, and neither
// `ghost supersede --withdraw` nor ghost_link_withdraw could reach it.
//
// Only the two relations a withdrawal can act on are returned by the empty form.
// A 'related' or 'contradicts' edge is not a supersession claim, nothing in the
// ranking reads it, and listing it in a refusal would send an operator after an
// edge neither surface was asked about; naming a relation the CLI and the MCP
// tool do not accept is refused by them, not silently ignored here.
func (s *Store) LinksInto(ctx context.Context, projectID, memoryID, relation string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// An empty relation means "both relations", and it is a separate statement
	// rather than an `OR relation IN (…)`: the caller that wants both wants the
	// order the withdrawal's selection reads in — 'supersedes' first, because that
	// is the relation its default picks — and an ORDER BY in a UNION would be a
	// second statement to keep agreeing with this one.
	if relation == "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
			FROM memory_links l
			JOIN memories source_mem ON source_mem.id = l.source_id
			JOIN memories target_mem ON target_mem.id = l.target_id
			WHERE l.relation IN ('supersedes', 'causes') AND l.invalidated_at IS NULL
			  AND l.target_id = ?
			  AND (source_mem.project_id IN (?, ?) OR target_mem.project_id IN (?, ?))
			ORDER BY CASE l.relation WHEN 'supersedes' THEN 0 ELSE 1 END, l.created_at
		`, memoryID, projectID, GlobalProjectID, projectID, GlobalProjectID)
		if err != nil {
			return nil, fmt.Errorf("links into: %w", err)
		}
		defer rows.Close() //nolint:errcheck
		return scanLinks(rows)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = ? AND l.invalidated_at IS NULL
		  AND l.target_id = ?
		  AND (source_mem.project_id IN (?, ?) OR target_mem.project_id IN (?, ?))
		ORDER BY l.created_at
	`, relation, memoryID, projectID, GlobalProjectID, projectID, GlobalProjectID)
	if err != nil {
		return nil, fmt.Errorf("links into: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanLinks(rows)
}

// scanLinks reads the seven link columns in the order both LinksInto statements
// select them, so the two cannot drift about which column is which — a swap would
// be silent, because every column is a string or a float and scans into a string
// just fine.
func scanLinks(rows *sql.Rows) ([]Link, error) {
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// InvalidateLink soft-invalidates a link (Zep-style: never delete, mark
// invalid with a timestamp so history is preserved). It returns how many links
// it moved out of the live set, so a caller can report an actual graph change
// instead of assuming one: the UPDATE is guarded on invalidated_at IS NULL, so a
// link that was already invalidated — including on an earlier pass — is not
// re-stamped and does not count. The guard changes nothing about the graph
// itself (every reader filters invalidated_at IS NULL, so a dead link stays
// dead either way); what it changes is this count, which is the difference
// between a real removal and a no-op the caller can now tell apart.
func (s *Store) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if relation != "supersedes" {
		res, err := s.execGuardedWrite(ctx, "invalidate-link", linkInvalidateSQL, sourceID, targetID, relation)
		if err != nil {
			return 0, fmt.Errorf("invalidate link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			// A driver that cannot report the count is not a reason to fail: the
			// invalidation itself already committed.
			return 0, nil
		}
		return n, nil
	}

	// Withdrawing a supersession is a change in the target's standing, exactly as
	// asserting one was, and the history has to say so: a corpus whose audit shows
	// a supersede and no withdrawal reads as though the stale claim is still
	// live. The edge and its row share a transaction, and the row is written only
	// when the edge was live — the same guard the UPDATE applies, so a re-run
	// that changes nothing records nothing.
	tx, _, err := s.beginWrite(ctx, "invalidate-link")
	if err != nil {
		return 0, fmt.Errorf("begin invalidate link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	active, err := linkIsActive(ctx, tx, sourceID, targetID, relation)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, linkInvalidateSQL, sourceID, targetID, relation)
	if err != nil {
		return 0, fmt.Errorf("invalidate link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The error, not a zero count. In the autocommit statement above, "the
		// invalidation itself already committed" made swallowing this honest;
		// inside this transaction it has not committed, so returning 0 would fall
		// through to the deferred rollback, discard the stamp, and tell the
		// caller the edge is still live — which is what internal/supersede then
		// branches on, four call sites deep. A driver that cannot report a count
		// is one this build has not met, and failing the call is the answer that
		// does not claim something the database may have done.
		return 0, fmt.Errorf("invalidate link rows: %w", err)
	}
	if n > 0 && active {
		if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
			phase:     phaseUnsupersede,
			relatedID: sourceID,
		}}, []string{targetID}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit invalidate link: %w", err)
	}
	return n, nil
}

// linkInvalidateSQL is the soft delete InvalidateLink performs: a timestamp, never
// a row removal, so the graph keeps the edge that was there. The guard on
// invalidated_at IS NULL is what makes the count a real graph change rather than
// a re-stamp.
const linkInvalidateSQL = `
		UPDATE memory_links SET invalidated_at = datetime('now')
		WHERE source_id = ? AND target_id = ? AND relation = ? AND invalidated_at IS NULL
	`

// PinnedContradictedRow is one open contradiction against a pinned memory: a
// live `contradicts` edge joins it to a newer, unresolved memory.
type PinnedContradictedRow struct {
	ID        string
	ProjectID string
	Content   string
	// ContradictedBy is the id of the newer row that contradicts this pinned row,
	// and ContradictedByContent is its content.
	ContradictedBy        string
	ContradictedByContent string
	// AlsoContradictedBy counts the other newer rows that contradict it too.
	AlsoContradictedBy int
}

// pinnedContradictionHead and pinnedContradictionTail are the statement ghost_health reports pinned
// contradictions from. `contradicts` is symmetric and may be stored either way
// round, so the edge is read in both directions; an edge whose endpoints' scopes
// conflict is two true claims about two places and every reader exempts it; an
// invalidated edge or a resolved row on either side is closed. Content is cut
// to pinnedContradictionPreview characters in SQL, so the read is bounded per
// row, and the newer-than test is made in Go (ParseStamp), because the two
// stamp columns are not guaranteed one textual format.
const pinnedContradictionHead = `
	SELECT m.id, m.project_id, substr(m.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(m.updated_at, ''), m.created_at),
	       o.id, substr(o.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(o.updated_at, ''), o.created_at)
	FROM memories m
	JOIN memory_links ml ON ml.relation = 'contradicts' AND ml.invalidated_at IS NULL
	 AND (ml.target_id = m.id OR ml.source_id = m.id)
	JOIN memories o ON o.id = CASE WHEN ml.target_id = m.id THEN ml.source_id ELSE ml.target_id END
	WHERE m.pinned = 1 AND m.resolved_at IS NULL AND o.resolved_at IS NULL AND o.id <> m.id
	  AND NOT `

const pinnedContradictionTail = `
	ORDER BY m.project_id, m.id, o.id`

const pinnedContradictionPreview = "200"

// PinnedRowsWithContradictions reports the pinned memories a newer memory
// contradicts through a live `contradicts` edge: one entry per PINNED row, at
// most limit of them, and the total number of such pinned rows. Each entry
// names the first contradicting row (by id) and counts the others. A pair
// stored in both directions is one contradiction. Newer is the freshness key
// the assembler's marker uses: updated_at, or created_at when unset. A pin keeps
// such a row in every session start and nothing else tells its owner later
// evidence disagrees, so this is the list to review: unpin or update the row.
// Contents are cut to 200 characters. It only reads; it never changes a row or
// an edge.
func (s *Store) PinnedRowsWithContradictions(ctx context.Context, limit int) ([]PinnedContradictedRow, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := pinnedContradictionHead + scopesConflictSQL("m.scope", "o.scope") + pinnedContradictionTail
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, 0, fmt.Errorf("pinned rows with contradictions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []PinnedContradictedRow
	pairs := map[[2]string]bool{}
	total := 0
	lastID, shown := "", false
	for rows.Next() {
		var r PinnedContradictedRow
		var pinnedStamp, otherStamp string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Content, &pinnedStamp, &r.ContradictedBy, &r.ContradictedByContent, &otherStamp); err != nil {
			return nil, 0, err
		}
		pt, _ := ParseStamp(pinnedStamp)
		ot, _ := ParseStamp(otherStamp)
		pair := [2]string{r.ID, r.ContradictedBy}
		if !ot.After(pt) || pairs[pair] {
			continue
		}
		pairs[pair] = true
		// Rows arrive ordered by pinned row, so a repeat of the previous pinned
		// row is one more contradicting row for the same entry.
		if r.ID == lastID {
			if shown {
				result[len(result)-1].AlsoContradictedBy++
			}
			continue
		}
		lastID = r.ID
		total++
		shown = len(result) < limit
		if shown {
			result = append(result, r)
		}
	}
	return result, total, rows.Err()
}
