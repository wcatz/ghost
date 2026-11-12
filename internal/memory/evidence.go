package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	evidenceObserved = "observed"

	evidenceImported = "imported"

	evidenceVerified = "verified"

	evidenceLegacy = "legacy"
)

// Evidence is one record of support for a memory: a moment somebody or something
// reported this fact, and whatever it reported about itself.
type Evidence struct {
	ID        string
	MemoryID  string
	Kind      string
	Agent     string
	SessionID string

	SourceRef string

	Confidence *float64

	ObservedAt *string

	VerifiedAt *string

	CarriedFrom string
}

// EvidenceCounts is what a reader can say about a memory's support without
// reading its records: how many observations back it, and how many of them
// carry a verification.
type EvidenceCounts struct {
	Observations int

	Verified int
}

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

func appendEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID, kind string, prov Provenance, isVerification bool) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), `+verifiedAtExpr(isVerification)+`)`,
		memoryID, kind,
		nullIfEmpty(prov.Agent), nullIfEmpty(prov.SessionID), nullIfEmpty(prov.SourceRef),
		prov.Confidence,
	); err != nil {
		return fmt.Errorf("append %s evidence for %s: %w", kind, RefusalArg("memory_id", memoryID), err)
	}
	return nil
}

func verifiedAtExpr(isVerification bool) string {
	if isVerification {
		return "datetime('now')"
	}
	return "NULL"
}

// AppendVerifiedEvidenceTx records that somebody CHECKED a memory, in the
// caller's transaction.
func AppendVerifiedEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID string, prov Provenance) error {
	return appendEvidenceTx(ctx, tx, memoryID, evidenceVerified, prov, true)
}

func appendVerificationIfStatedTx(ctx context.Context, tx *sql.Tx, memoryID string, prov Provenance, stated *string) error {
	if stated == nil {
		return nil
	}
	return AppendVerifiedEvidenceTx(ctx, tx, memoryID, prov)
}

func carryEvidenceTx(ctx context.Context, tx *sql.Tx, toMemoryID string, fromIDs []string) error {
	if len(fromIDs) == 0 {

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

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at, carried_from)
		SELECT ?, kind, agent, session_id, source_ref, confidence, observed_at, verified_at, memory_id
		FROM memory_provenance WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY memory_id, rowid`,
		args...,
	); err != nil {
		return fmt.Errorf("carry evidence onto %s: %w", RefusalArg("memory_id", toMemoryID), err)
	}
	return nil
}

func uncarryRestoredSourcesTx(ctx context.Context, tx *sql.Tx, snapshotID string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM memory_provenance
		WHERE carried_from IS NOT NULL
		  AND carried_from IN (
		      SELECT memory_id FROM memory_snapshots
		      WHERE snapshot_id = ? AND memory_id IS NOT NULL)
		  AND memory_id IN (
		      SELECT m.id FROM memories m
		      JOIN memory_snapshots s ON s.memory_id = m.id
		      WHERE s.snapshot_id = ? AND m.project_id = s.project_id
		        AND m.pinned = 0 AND m.resolved_at IS NULL
		        AND m.`+retentionExemptSQL+`)`,
		snapshotID, snapshotID,
	); err != nil {
		return fmt.Errorf("remove evidence carried from restored rows: %w", err)
	}
	return nil
}

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

var validEvidenceKinds = map[string]bool{
	evidenceObserved: true,
	evidenceImported: true,
	evidenceVerified: true,
	evidenceLegacy:   true,
}

// IsValidEvidenceKind reports whether kind is one of the four the table's CHECK
// accepts.
func IsValidEvidenceKind(kind string) bool { return validEvidenceKinds[kind] }

func importEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID string, e PortableEvidence) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_provenance
			(id, memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		VALUES (CASE WHEN ? != '' AND NOT EXISTS (SELECT 1 FROM memory_provenance WHERE id = ?)
			THEN ? ELSE hex(randomblob(16)) END, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ID, e.ID, memoryID, e.Kind,
		nullIfEmpty(e.Agent), nullIfEmpty(e.SessionID), nullIfEmpty(e.SourceRef),
		e.Confidence, boundStamp(e.ObservedAt), boundStamp(e.VerifiedAt),
	); err != nil {
		return fmt.Errorf("import evidence for %s: %w", RefusalArg("memory_id", memoryID), err)
	}
	return nil
}

func boundStamp(at *string) any {
	if at == nil || *at == "" {
		return nil
	}
	return *at
}

func (s *Store) MemoryProvenance(ctx context.Context, memoryID string) ([]Evidence, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

func (s *Store) MemoryEvidenceCounts(ctx context.Context, memoryID string) (EvidenceCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return evidenceCountsOne(ctx, s.queryDB(), memoryID)
}

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
