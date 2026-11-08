package memory

import (
	"context"
	"fmt"
)

// IsConsolidatable reports whether m may be rewritten by reflect.
// It is the Go mirror of replaceableRowSQL: a row the replace may delete,
// reuse or merge into. Every caller that decides "will reflect see this?"
// uses this predicate, so the filter lives in one place.
func IsConsolidatable(m Memory) bool {
	if m.Pinned {
		return false
	}
	if RetentionExempt(m) {
		return false
	}
	if m.Source == "manual" || m.Source == "builtin" {
		return false
	}
	if m.ResolvedAt != nil {
		return false
	}
	return true
}

// IsFrozen reports whether reflect will never consolidate m.
func IsFrozen(m Memory) bool { return !IsConsolidatable(m) }

// ReflectionExclusionStats counts, per project, how many live rows
// reflect cannot touch and why. Total is COUNT(*) for the project;
// Frozen is the distinct rows matching any freeze reason; Consolidatable
// = Total - Frozen. The per-reason counts are independent and may overlap
// (a pinned manual row counts in both), so they can sum past Frozen.
type ReflectionExclusionStats struct {
	Total          int
	Frozen         int
	Consolidatable int
	Pinned         int
	Persistent     int
	Manual         int
	Builtin        int
	Resolved       int
}

// ReflectionExclusionStats returns the per-project exclusion breakdown.
// It reads through queryDB so a diagnostic transaction sees one snapshot.
func (s *Store) ReflectionExclusionStats(ctx context.Context, projectID string) (ReflectionExclusionStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var st ReflectionExclusionStats
	// One statement so the counts describe one snapshot. Frozen is derived
	// as total - consolidatable so the two always sum to total even when
	// retention is NULL (older rows).
	const q = `
		SELECT
			COUNT(*),
			COUNT(CASE WHEN pinned = 1 THEN 1 END),
			COUNT(CASE WHEN retention = '` + RetentionPersistent + `' THEN 1 END),
			COUNT(CASE WHEN source = 'manual' THEN 1 END),
			COUNT(CASE WHEN source = 'builtin' THEN 1 END),
			COUNT(CASE WHEN resolved_at IS NOT NULL THEN 1 END),
			COUNT(CASE WHEN source NOT IN ('manual','builtin') AND pinned = 0 AND resolved_at IS NULL AND COALESCE(retention,'project') <> '` + RetentionPersistent + `' THEN 1 END)
		FROM memories WHERE project_id = ?
	`
	if err := s.queryDB().QueryRowContext(ctx, q, projectID).Scan(
		&st.Total,
		&st.Pinned,
		&st.Persistent,
		&st.Manual,
		&st.Builtin,
		&st.Resolved,
		&st.Consolidatable,
	); err != nil {
		return ReflectionExclusionStats{}, fmt.Errorf("count reflection exclusions: %w", err)
	}
	st.Frozen = st.Total - st.Consolidatable
	return st, nil
}
