package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The kinds a memory_provenance row can hold, mirroring the table's CHECK. They
// are constants rather than literals so a writer cannot spell one the CHECK will
// refuse, and so the read side can answer "what kind is this" without a second
// list to keep in step.
//
// observed, imported and legacy are written today. verified is RESERVED: the
// validity writers (issue #575) own the write that stamps a memory as checked,
// and until one lands a verified row can only come from an artifact or a
// hand-seeded store. It is in the CHECK now because the vocabulary is the
// schema's, and adding a value later would mean rebuilding the table for every
// database that already has it.
const (
	// evidenceObserved: a write path recorded what the host reported — a save, a
	// fold's second observation, a decision's companion memory.
	evidenceObserved = "observed"
	// evidenceImported: the fact reached this store through a portable artifact,
	// which is a claim no observed row makes and one a restored corpus would
	// otherwise lose entirely.
	evidenceImported = "imported"
	// evidenceVerified: somebody checked the fact. Reserved for the validity
	// writers; see above.
	evidenceVerified = "verified"
	// evidenceLegacy: migrateV18's seed of a memory that already held the values
	// in its own columns. Not an observation this build made.
	evidenceLegacy = "legacy"
)

// Evidence is one record of support for a memory: a moment somebody or something
// reported this fact, and whatever it reported about itself.
//
// It is NOT memory_history's HistoryEntry and not the Provenance a writer passes
// in. Provenance is what ONE call claimed; this is what the store kept, several
// calls' worth, and it is never overwritten — a second agent reporting the same
// fact is a second row, not a replacement of the first.
type Evidence struct {
	ID        string
	MemoryID  string
	Kind      string
	Agent     string
	SessionID string
	// SourceRef is what the reporter read it out of — a file, a path, a commit,
	// a URL. Empty when the host reported none.
	SourceRef string
	// Confidence is a belief rating in [0,1] with whatever basis the reporter
	// gave, and nil when none was given. It is a pointer because 0.0 is a real
	// rating and "no belief recorded" is a different thing from a low one.
	Confidence *float64
	// ObservedAt is when Ghost recorded this observation, and nil when no writer
	// could state it — the migration's seed of a memory's existing columns is the
	// case: the columns say who wrote the row, not when the fact was first seen.
	ObservedAt *string
	// VerifiedAt is when the fact was checked, and nil when it never was. A row
	// may carry it whatever its kind: a `legacy` seed of a hand-verified memory
	// has one, and so will a `verified` row.
	VerifiedAt *string
	// CarriedFrom is the memory this record was inherited from, and empty for a
	// record that observed this row directly. A consolidation rewrite or merge
	// mints a new id and the foreign key takes the source's evidence with it, so
	// the record is copied and this says so — verbatim, because the observation
	// really was made, but not an observation of THIS wording.
	//
	// It is also the way back into the change log: the id it names is gone, and
	// the change log is what kept that id's past.
	CarriedFrom string
}

// EvidenceCounts is what a reader can say about a memory's support without
// reading its records: how many observations back it, and how many of them
// carry a verification.
//
// It is deliberately two integers and no score. Nothing ranks on it (#673: stage
// 4's multiplier stays 1.0 until a measured change justifies one), so a "support
// score" would be a number a reader would eventually trust.
type EvidenceCounts struct {
	// Observations counts the records. Every record is one observation of the
	// fact, whatever its kind: a `legacy` seed says this build read the memory's
	// own provenance columns, and an `imported` row says the fact arrived through
	// a file. Both are support.
	Observations int
	// Verified counts the records that carry a verification stamp.
	Verified int
}

// Label renders the compact form a trace reports: "supported by 2 observations,
// 1 verified". The verified clause appears only when there is one, because
// ", 0 verified" on every row would be noise a reader learns to skip, and the
// zero is not hidden — it is the absence of the clause.
//
// A memory with no records is "no recorded evidence" rather than "supported by 0
// observations": the first says Ghost holds nothing, the second reads as a
// measurement of zero support, which is a different claim.
func (c EvidenceCounts) Label() string {
	switch {
	case c.Observations == 0:
		return "no recorded evidence"
	case c.Verified > 0:
		return fmt.Sprintf("supported by %d %s, %d verified", c.Observations, plural(c.Observations, "observation", "observations"), c.Verified)
	default:
		return fmt.Sprintf("supported by %d %s", c.Observations, plural(c.Observations, "observation", "observations"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// appendEvidenceTx records one evidence record for memoryID in the caller's
// transaction, in ONE statement.
//
// The one-statement rule is the reason this function has no prune, no existence
// check and no follow-up read. It runs inside the write transaction, on the
// critical section every writer queues for, and memory_history's growth policy
// cost the multi-process test SQLITE_BUSY when it took three extra statements per
// save (#664). The growth this leaves unbounded is bounded in practice by the
// cascade: a deleted memory's records go with it, so the table holds at most one
// observation per save for every LIVE memory, and a store that folds the same fact
// ten thousand times is a store whose change log is already at its cap.
//
// The values come from the caller's Provenance and from nowhere else. agent,
// session_id and source_ref go through nullIfEmpty and confidence through its
// pointer, so a field the host did not report is NULL rather than "" — the same
// rule the memories columns follow, and for the same reason: an empty string
// claims a value that happens to be empty. observed_at is stamped here rather
// than taken from the caller because it means "when Ghost recorded this", and a
// caller that could set it would be able to backdate its own evidence.
//
// The foreign key is the caller's guarantee that the record belongs to a live
// memory: an id with no memories row fails the statement rather than leaving
// evidence attached to nothing. Callers therefore append only after the write
// that created the row.
//
// It stores no text the memories row does not already hold: the agent, session
// and reference are the same Provenance values the calling write put on the row,
// in the same transaction and behind whatever guard that writer applies
// (internal/secret's #656 seam). So the table cannot hold a value the memory row
// does not, and a purge — which deletes these rows explicitly — reaches every
// copy of one. Widening that guard to the provenance columns themselves is
// #656's seam and not this function's, because refusing one would change what a
// save stores in memories too.
func appendEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID, kind string, prov Provenance, isVerification bool) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), `+verifiedAtExpr(isVerification)+`)`,
		memoryID, kind,
		nullIfEmpty(prov.Agent), nullIfEmpty(prov.SessionID), nullIfEmpty(prov.SourceRef),
		prov.Confidence,
	); err != nil {
		return fmt.Errorf("append %s evidence for %s: %w", kind, memoryID, err)
	}
	return nil
}

// verifiedAtExpr is the SQL expression the verified_at column of an appended
// record reads: NULL for a record that OBSERVES a fact, and the store's own clock
// for a record that IS the verification.
//
// Two literals spliced into one statement, never a value — the same shape as
// historyContentExpr, and for the same reason: a caller that could pass its own
// stamp could backdate a verification, and one that passed a formatted string
// instead would store that string. Both alternatives were the alternative designs
// to a bool; the bool is the only one with no way to be wrong about the stamp.
func verifiedAtExpr(isVerification bool) string {
	if isVerification {
		return "datetime('now')"
	}
	return "NULL"
}

// AppendVerifiedEvidenceTx records that somebody CHECKED a memory, in the
// caller's transaction.
//
// It is exported, and that is the whole reason: the validity writers that turn a
// save tool's `verified: true` into a stored claim (issue #575, PR #677) land
// separately from this table, and they need ONE name and ONE implementation for
// "somebody verified this" rather than a second copy that the two branches then
// disagree about. A verified record is what a reader counts in
// EvidenceCounts.Verified, so two spellings of it would be two different answers
// to the same question.
//
// The stamp is the store's clock, not the caller's, for the reason every other
// writer's timestamp is: a verifier that could date its own check could date it
// before the thing it checked. The caller supplies only who checked, and NULL
// there stays NULL — an anonymous verification is a real one, and the alternative
// is inventing a verifier.
func AppendVerifiedEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID string, prov Provenance) error {
	return appendEvidenceTx(ctx, tx, memoryID, evidenceVerified, prov, true)
}

// appendVerificationIfStatedTx appends a `verified` record when — and only when
// — the caller stated a verification IN THIS CALL, and does nothing otherwise.
//
// "Stated in this call" is the whole rule, and it is a narrower test than "the
// row ends up with a verified_at". A partial update COALESCEs, so a row that was
// verified last month still reads verified_at when this week's edit touches only
// the content — and appending a record for that would manufacture evidence of a
// check nobody made, in the one table whose entire claim is that a record means
// an event happened. The memories column and the evidence table are answering
// different questions: the column is the latest state, the table is the history
// of who said what, and a column read as a history is how a store ends up
// reporting "3 observations, 3 verified" for one check.
//
// stated is the caller's verified_at pointer, not a parsed instant: nil means the
// caller passed neither `verified: true` nor `verified_at`, and every non-nil
// value means the same thing here — somebody asserted the fact was checked. The
// value itself is NOT copied onto the record, because AppendVerifiedEvidenceTx
// stamps the store's own clock (see its comment): the caller's stamp lands on the
// memories column where a reader can see what was claimed, and the record says
// when Ghost learned of the check. Two facts, two places, and a verifier cannot
// date its own check.
//
// Every writer that stores a verified_at leaves a record carrying a
// verification stamp, in the same transaction, so the rule is written once rather
// than four times. Say it as the OUTCOME and not as the mechanism: the mechanisms
// differ on purpose, and a reader who filters MemoryProvenance by kind sees three
// shapes for one rule. One member is an exception to the OUTCOME and it is named
// as such below — CreateFromCorpus leaves the observation of the ingestion but no
// record carrying a stamp — so a summary sentence that claims the set holds
// uniformly would be false, and the per-writer paragraphs below are the claim.
//
// The callers of THIS function are the live ones, and they are the ones whose
// stamp is the store's clock — because Ghost is the party recording the check.
// That covers insertMemory (behind Create, and gated by insertOptions so the
// corpus route below can decline) and all three of UpsertWithOptions' branches
// and UpdateMemoryWithOptions.
//
// Two more writers store a verified_at and are deliberately NOT callers.
//
// ImportMemory is the interesting one, because it is in the SET and not in the
// callers: the artifact carries an OBSERVATION, so a check happened and is
// attested, and the import keeps that attestation on its own `imported` record
// rather than adding a second one. EvidenceCounts.Verified counts any record
// carrying a stamp whatever its kind, so the count a reader sees is right without
// the import knowing this function exists. Its stamp is the store's clock too —
// deliberately not the artifact's text, per #682: "no date is copied in from a
// file" — so the import is not an exception to the store-clock rule, it is the
// same rule applied to an attested arrival.
//
// CreateFromCorpus is the one writer in the set that leaves NO record carrying a
// verification stamp — and it still records, because insertMemory appends the
// `observed` row of the ingestion unconditionally on that route. "No verified
// record" is the whole of it; "records nothing" would be wrong, and
// TestCreateFromCorpusRecordsNoVerification is what pins the difference by
// asserting the observed row is there while no stamp is.
//
// The reason is the difference between a value and an event. A third-party dataset's
// verified_at is a value in a column with NO observation behind it: nobody checked
// anything through this store, so there is no event to record, and the only stamp
// a record could carry would be the store's clock — which would manufacture the
// event and assert that a benchmark's own claim was checked now. That is the exact
// inversion the store-clock rule exists to prevent, reached on the one path the
// rule had never been stated on. insertOptions.recordVerification is the flag that
// makes it so, and its comment carries the argument in full; the column keeps the
// dataset's value verbatim, because losing the dataset's own data would be the
// opposite defect.
//
// RestoreSnapshot is plainly outside the set — it writes in pure SQL from the
// snapshot table, a restore is a faithful copy of a corpus whose evidence rows
// travel with it (the portable artifact carries them), so appending per restored
// row would double-count every check the snapshot already holds. That is the same
// byte-exact exclusion MaxContentLen and the credential guard draw.
//
// This enumeration has now been wrong three times, every time because a comment is
// not checkable, so the set is TESTS rather than prose. Two of them, because they
// answer different questions. TestEveryVerifiedAtWriterIsEnumerated asserts the
// OUTCOME each writer leaves — whether a record carries a stamp, and of what kind —
// for the writers whose mechanism differs from the shared one.
// TestEveryVerifiedAtMentionIsClassified walks internal/memory and requires every
// function naming verified_at in its own body to be placed in one of three
// classifications, so a new writer cannot be added without a decision being made
// about it.
//
// What the second one does NOT cover is as much a part of the claim as what it
// does. It does not resolve the column name out of a package-level literal, so a
// function reaching verified_at only through one is invisible to it — and
// migrateV10, the migration that ADDS the column, is exactly that, naming it only
// through phase1aProvenanceColumns. A third test,
// TestColumnListWritersAreClassified, closes that one indirection by requiring
// every function iterating that list to be classified, and migrateV10 and
// AppendVerifiedEvidenceTx (the exported seam, whose body names no column because
// the writers reach it THROUGH it) are placed explicitly. A function reaching the
// column through some other indirection — a const, a format string, a helper — is
// still invisible, and that residual gap is the honest limit of the claim rather
// than something the next reader should have to rediscover.
func appendVerificationIfStatedTx(ctx context.Context, tx *sql.Tx, memoryID string, prov Provenance, stated *string) error {
	if stated == nil {
		return nil
	}
	return AppendVerifiedEvidenceTx(ctx, tx, memoryID, prov)
}

// carryEvidenceTx copies the evidence of the memories an emission was derived
// from onto the row that now holds their text, in ONE statement.
//
// It is a copy and not a fresh observation, and every field comes across
// unchanged: the same kind, the same agent, the same session, the same reference,
// the same confidence, the same observed_at. The observation really was made —
// by that agent, about that content, at that time. What it was not made about is
// the wording now on the row, and carried_from is what says so; a reader that
// wants only first-hand support can filter on it, and one that wants the support a
// consolidation consolidated gets the union.
//
// The sources' rows must still EXIST, which is why the caller runs this before the
// delete: the foreign key takes them the moment the source row goes, and there is
// no other copy. The snapshot is not a substitute — a restore would put them on the
// ORIGINAL id, not on the rewrite.
//
// One statement for the whole set, because a merge names every one of its sources
// and a per-source loop would make the statement count of a consolidation scale
// with the corpus it is rewriting.
func carryEvidenceTx(ctx context.Context, tx *sql.Tx, toMemoryID string, fromIDs []string) error {
	if len(fromIDs) == 0 {
		// A memory with no predecessor has no inherited support, and "none" is the
		// honest answer. Inventing an observation for a brand-new row is the one
		// thing this table must never do.
		return nil
	}
	placeholders := make([]string, 0, len(fromIDs))
	args := make([]any, 0, len(fromIDs)+1)
	for _, id := range fromIDs {
		if id == "" || id == toMemoryID {
			continue // an id the caller did not give, and the row's own
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return nil
	}
	args = append([]any{toMemoryID}, args...)
	// ORDER BY, and it is there for reproducibility rather than for meaning: two
	// agents reporting one fact have no first and second, and the set is far too
	// small for the sort to register on a path that rewrites the corpus. What it
	// buys is that two consolidations of the same corpus write the same records in
	// the same order, so `ghost export` stays byte-identical between them — an
	// engine-dependent order here would put a diff of two unchanged exports on the
	// card. The order is by source id, NOT by the order the caller named its
	// sources in, which is the one thing a per-source loop could have offered.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at, carried_from)
		SELECT ?, kind, agent, session_id, source_ref, confidence, observed_at, verified_at, memory_id
		FROM memory_provenance WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY memory_id, rowid`,
		args...,
	); err != nil {
		return fmt.Errorf("carry evidence onto %s: %w", toMemoryID, err)
	}
	return nil
}

// restoreSnapshotEvidenceTx puts back the evidence a snapshot carried, for the
// rows the restore re-created.
//
// Two statements and a hard replace, not an append, for three reasons. The rows
// are being RESTORED rather than observed again, so a record that described the
// moment of the restore would be a claim nobody made; the set is bounded by the
// snapshot the restore already read, so nothing here can be repeated by running
// the restore twice; and the destination rows are ones this transaction created,
// so a pre-existing record on them would mean the restore put back more than the
// snapshot held.
func restoreSnapshotEvidenceTx(ctx context.Context, tx *sql.Tx, snapshotID string, memoryIDs []string) error {
	const batch = 200 // well under SQLite's variable ceiling, two placeholders per id
	for start := 0; start < len(memoryIDs); start += batch {
		end := min(start+batch, len(memoryIDs))
		chunk := memoryIDs[start:end]
		ph := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			ph = append(ph, "?")
			args = append(args, id)
		}
		list := strings.Join(ph, ",")
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM memory_provenance WHERE memory_id IN (`+list+`)`, args...,
		); err != nil {
			return fmt.Errorf("clear evidence before a restore: %w", err)
		}
		copyArgs := append([]any{snapshotID}, args...)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_provenance
				(memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
			SELECT memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at
			FROM memory_snapshot_evidence
			WHERE snapshot_id = ? AND memory_id IN (`+list+`)`, copyArgs...,
		); err != nil {
			return fmt.Errorf("restore snapshot evidence: %w", err)
		}
	}
	return nil
}

// validEvidenceKinds is the set memory_provenance.kind accepts, and is what the
// portable importer validates an artifact's evidence against. The CHECK in the
// schema is the authority; this map is the same list spelled out so an import can
// name the offending value instead of failing a statement with SQLite's own
// wording — and so the rejection happens in the dry run too, where a user reads
// what the apply would refuse.
var validEvidenceKinds = map[string]bool{
	evidenceObserved: true,
	evidenceImported: true,
	evidenceVerified: true,
	evidenceLegacy:   true,
}

// IsValidEvidenceKind reports whether kind is one of the four the table's CHECK
// accepts.
func IsValidEvidenceKind(kind string) bool { return validEvidenceKinds[kind] }

// importEvidenceTx writes one evidence record the artifact carried, in the
// caller's transaction.
//
// The record's own id comes from the artifact when it has one, for the reason
// ImportMemory takes a memory's: two exports of an unchanged store are then
// byte-identical, and a hand-written record with no id gets the column's default
// rather than colliding with the row the file did name. Nothing else is
// interpreted — the record is what the file said, including a kind this build
// never writes (`legacy`, from a store seeded by the migration) and an
// observation Ghost has no writer for.
func importEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID string, e PortableEvidence) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(id, memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		VALUES (COALESCE(NULLIF(?, ''), hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, memoryID, e.Kind,
		nullIfEmpty(e.Agent), nullIfEmpty(e.SessionID), nullIfEmpty(e.SourceRef),
		e.Confidence, boundStamp(e.ObservedAt), boundStamp(e.VerifiedAt),
	); err != nil {
		return fmt.Errorf("import evidence for %s: %w", memoryID, err)
	}
	return nil
}

// boundStamp maps an artifact's optional stamp to a bind parameter, where NULL is
// the only honest answer for "this record does not say when".
//
// A non-nil pointer is not enough, and the artifact is where that bites: the form
// is documented as something a user reads and hand-edits, the column is a POINTER,
// and `omitempty` drops a nil rather than a pointer to "". An empty string bound
// straight through would be stored as a value that is not NULL — and these two
// columns are the two a reader ACTS on. `scanEvidence` reports a non-nil pointer
// for a non-NULL column, so a moment that does not exist reads as one; and
// `verified_at IS NOT NULL` is what the support counts sum, so an empty string is
// counted as a verification and the assembler's trace prints "1 verified" for a
// check nobody performed. It round-trips too: the export re-emits a non-NULL
// column as "", so every later import re-creates it.
//
// Both stamps go through here, not only the verified one: an empty `observed_at`
// is the same lie about the same kind of thing, and one rule for the pair is the
// only way a hand edit cannot slip past the field nobody remembered.
func boundStamp(at *string) any {
	if at == nil || *at == "" {
		return nil
	}
	return *at
}

// MemoryProvenance returns one memory's evidence records, oldest first.
//
// Oldest first, because the question these records answer is "what has supported
// this memory, and in what order did that support arrive" — a sequence, read
// forwards. The order is rowid rather than observed_at, for the reason
// MemoryHistory's is: observed_at is second-precision, so several observations
// recorded in one session share a timestamp and ordering by it would be a coin
// toss among them.
//
// An id with no records returns an empty slice and no error: "nothing has ever
// observed this" is an answer, and a reader should not have to tell it apart
// from a database failure.
func (s *Store) MemoryProvenance(ctx context.Context, memoryID string) ([]Evidence, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// The pool is safe here: no transaction is open on this handle.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, memory_id, kind, agent, session_id, source_ref, confidence,
		       observed_at, verified_at, carried_from
		FROM memory_provenance
		WHERE memory_id = ?
		ORDER BY rowid`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("read memory provenance: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []Evidence
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory provenance: %w", err)
	}
	return out, nil
}

// MemoryEvidenceCounts returns how many records support a memory and how many of
// them are verified — the compact form, for a reader that reports support without
// listing it. It is a GROUP BY over the same index MemoryProvenance reads, so it
// costs one query where the full read would cost a row per observation.
func (s *Store) MemoryEvidenceCounts(ctx context.Context, memoryID string) (EvidenceCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return evidenceCountsOne(ctx, s.queryDB(), memoryID)
}

// evidenceCountsOne is MemoryEvidenceCounts over any read surface, so a store
// bound to a snapshot transaction reads the snapshot rather than the pool.
func evidenceCountsOne(ctx context.Context, db Queryer, memoryID string) (EvidenceCounts, error) {
	var c EvidenceCounts
	err := db.QueryRowContext(ctx, `
		SELECT count(*), coalesce(sum(verified_at IS NOT NULL), 0)
		FROM memory_provenance WHERE memory_id = ?`, memoryID,
	).Scan(&c.Observations, &c.Verified)
	if err != nil {
		return EvidenceCounts{}, fmt.Errorf("read evidence counts: %w", err)
	}
	return c, nil
}

// evidenceCountsFor returns the evidence counts for a set of memories, as one
// query per chunk. It is the batch form the candidate read uses, and it is a
// separate function from MemoryEvidenceCounts rather than a loop over it: the
// assembler may only reach the store through ONE read (Candidates), and a
// per-row query would be one statement per candidate on the live search path.
//
// Chunked at the same size as the edge read, for the same reason: SQLite caps a
// statement's bound parameters, so a window wider than the chunk would be a
// candidate set whose evidence silently read as none.
func evidenceCountsFor(ctx context.Context, db Queryer, ids []string) (map[string]EvidenceCounts, error) {
	out := make(map[string]EvidenceCounts, len(ids))
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
			SELECT memory_id, count(*), coalesce(sum(verified_at IS NOT NULL), 0)
			FROM memory_provenance
			WHERE memory_id IN (`+strings.Join(ph, ",")+`)
			GROUP BY memory_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("read evidence counts: %w", err)
		}
		for rows.Next() {
			var id string
			var c EvidenceCounts
			if err := rows.Scan(&id, &c.Observations, &c.Verified); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("scan evidence counts: %w", err)
			}
			out[id] = c
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("iterate evidence counts: %w", err)
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
}

// scanEvidence reads one row into an Evidence. The nullable columns are scanned
// through sql.Null* because what nil means in the Go struct is "the store holds
// no such value" — a distinction "" cannot carry, and the whole point of the
// table.
func scanEvidence(sc rowScanner) (Evidence, error) {
	var e Evidence
	var agent, sessionID, sourceRef, observedAt, verifiedAt, carriedFrom sql.NullString
	var confidence sql.NullFloat64
	if err := sc.Scan(&e.ID, &e.MemoryID, &e.Kind, &agent, &sessionID, &sourceRef,
		&confidence, &observedAt, &verifiedAt, &carriedFrom); err != nil {
		return Evidence{}, fmt.Errorf("scan memory provenance: %w", err)
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
	e.CarriedFrom = carriedFrom.String
	return e, nil
}
