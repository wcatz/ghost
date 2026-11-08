package memory

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SessionProjectCap is the session-start context cap. It is deliberately
// smaller than the historical 25-memory context and independent of the
// caller-supplied limit used by Store.GetTopMemories; the ranking below keeps
// the session digest bounded while preserving the most useful memories.
// Single definition: internal/mcpinit aliases this (sessionMemoriesCap).
const SessionProjectCap = 15

// SessionGlobalCap is lower than the project-memories cap (SessionProjectCap)
// since globals compete for attention across every project, not just one.
// Single definition: internal/mcpinit aliases this (globalsCap).
const SessionGlobalCap = 8

// SessionProjectDisplayBytes and SessionGlobalDisplayBytes are the per-item
// PREVIEW budgets of the rendered block, applied per bucket because a preview
// has to SAY that it was cut (ellipsis) and ClampBytes cannot. The bucket with
// more items spends fewer bytes on each, so the two sections stay comparable
// in size rather than one dominating the block. Single definition:
// internal/mcpinit aliases these (sessionDisplayBytes, globalsDisplayBytes).
const SessionProjectDisplayBytes = 200
const SessionGlobalDisplayBytes = 300

// PinnedProjectCap and PinnedGlobalCap are the session-start bucket caps for
// the project and _global slices, bounded slot guarantee. Kept as aliases so
// callers that name the guarantee can keep that name without re-spelling the cap.
const (
	PinnedProjectCap = SessionProjectCap
	PinnedGlobalCap  = SessionGlobalCap
)

// PinnedOverflow describes a project's pinned set against its session-start
// bucket. Count is the live pinned rows' total, Cap is what the bucket can
// show, and CutIDs are the lowest-ranked pinned rows by importance, most at
// risk of being cut (for awareness only, not the exact set the block drops).
// Empty when the bucket fits. Overflow is count-only: with per-row preview
// truncation and KeepPinned on the session-start fit pass, pinned rows are not
// cut for bytes as a separate rule; however assemble.FitItems may cut a pinned
// row as a last resort when no unpinned row is left, and byte budgets are not
// used to report cuts — the overflow report is count-only.
type PinnedOverflow struct {
	ProjectID string
	Count     int
	Cap       int
	CutIDs    []string
}

// PinnedOverflowExceeded reports whether the pinned set exceeds what the bucket
// can show by count only. With per-row preview truncation and KeepPinned on the
// session-start fit pass, a pinned row is not cut for bytes before unpinned rows
// in the normal case; assemble.FitItems may cut a pinned row as a last resort
// when no unpinned row is left. The preview caps keep total bytes within the
// block budget in normal cases; the guarantee is bounded by the count cap and
// the overflow report is count-only.
func (o *PinnedOverflow) Exceeded() bool {
	if o == nil {
		return false
	}
	return o.Count > o.Cap
}

// PinnedOverflow returns the overflow for one project's pinned set, measured
// at now. It counts live pinned rows (pinned=1, resolved_at IS NULL) whose
// validity window includes now and which match the configured session scope
// (ScopeMatchesSQL), ranks them by the bucket's own order, and names the
// lowest-ranked pinned rows by importance (most at risk of being cut) by the
// count cap. The session-start block truncates each row to a preview
// (sessionDisplayBytes/globalsDisplayBytes) and fits the whole block with
// KeepPinned; pinned rows are not cut for bytes as a separate rule, but
// assemble.FitItems may cut a pinned row as a last resort when no unpinned
// row is left. Byte overflow is not reported — the overflow report is
// count-only. Nil when the bucket fits. The reported ids are not guaranteed to
// match what the block drops.
func (s *Store) PinnedOverflow(ctx context.Context, projectID string, now time.Time, scope map[string]string) (*PinnedOverflow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cap := PinnedProjectCap
	if projectID == GlobalProjectID {
		cap = PinnedGlobalCap
	}

	where := `project_id = ? AND pinned = 1 AND resolved_at IS NULL`
	args := []any{projectID}
	if len(scope) > 0 {
		where += ` AND ` + ScopeMatchesSQL("scope", scope)
	}
	rows, err := s.queryDB().QueryContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("pinned overflow for %q: %w", projectID, err)
	}
	defer func() { _ = rows.Close() }()
	pinnedAll, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	// Withhold rows whose validity window excludes now, like the
	// session-start fetch does — a pin outside its window is not
	// guaranteed a slot and does not count toward the guarantee.
	var pinned []Memory
	for _, m := range pinnedAll {
		if m.ValidFrom != nil || m.ValidUntil != nil || m.VerifiedAt != nil {
			if _, out := ValidityAt(m.ValidFrom, m.ValidUntil, m.VerifiedAt, now); out {
				continue
			}
		}
		pinned = append(pinned, m)
	}

	count := len(pinned)

	if count <= cap {
		return nil, nil
	}

	// Rank the pins among themselves by importance order (and bucket tie-breaks),
	// as the cap does. The ids reported are the lowest-ranked by importance,
	// most at risk of being cut (for awareness), not necessarily the exact set
	// the session-start block will drop.
	sort.SliceStable(pinned, func(i, j int) bool {
		return pinnedLess(pinned[i], pinned[j], projectID, now)
	})

	var cutIDs []string
	for i := cap; i < len(pinned); i++ {
		cutIDs = append(cutIDs, pinned[i].ID)
	}

	return &PinnedOverflow{
		ProjectID: projectID,
		Count:     count,
		Cap:       cap,
		CutIDs:    cutIDs,
	}, nil
}

// PinnedOverflows returns every project whose pinned set exceeds its bucket.
func (s *Store) PinnedOverflows(ctx context.Context, now time.Time, scope map[string]string) ([]PinnedOverflow, error) {
	s.mu.RLock()
	rows, err := s.queryDB().QueryContext(ctx, `SELECT DISTINCT project_id FROM memories WHERE pinned = 1 AND resolved_at IS NULL`)
	s.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("list pinned projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var overflows []PinnedOverflow
	for _, pid := range projects {
		ov, err := s.PinnedOverflow(ctx, pid, now, scope)
		if err != nil {
			return nil, err
		}
		if ov != nil && ov.Exceeded() {
			overflows = append(overflows, *ov)
		}
	}
	sort.Slice(overflows, func(i, j int) bool { return overflows[i].ProjectID < overflows[j].ProjectID })
	return overflows, nil
}

func pinnedLess(a, b Memory, projectID string, now time.Time) bool {
	if projectID == GlobalProjectID {
		if a.Importance != b.Importance {
			return a.Importance > b.Importance
		}
		// UpdatedAt is lexicographic like the candidate scan: newer first.
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt > b.UpdatedAt
		}
		return a.ID < b.ID
	}
	// Project bucket: decay ranking, then importance, then creation, then id.
	// Pinned rows have DecayFactor 1, so rank == importance, but keep the
	// formula for fidelity.
	ra := float64(a.Importance) * DecayFactor(a.Category, a.Retention, true, ageDays(a.CreatedAt, now))
	rb := float64(b.Importance) * DecayFactor(b.Category, b.Retention, true, ageDays(b.CreatedAt, now))
	if ra != rb {
		return ra > rb
	}
	if a.Importance != b.Importance {
		return a.Importance > b.Importance
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID < b.ID
}
