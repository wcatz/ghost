package memory

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SessionProjectCap is the session-start project context cap.
const SessionProjectCap = 15

// SessionGlobalCap is the session-start global context cap.
const SessionGlobalCap = 8

// SessionProjectDisplayBytes is the per-item preview budget for project rows.
const SessionProjectDisplayBytes = 200

// SessionGlobalDisplayBytes is the per-item preview budget for global rows.
const SessionGlobalDisplayBytes = 300

const (
	PinnedProjectCap = SessionProjectCap
	PinnedGlobalCap  = SessionGlobalCap
)

// PinnedOverflow describes a project's pinned set against its session-start bucket.
type PinnedOverflow struct {
	ProjectID string
	Count     int
	Cap       int
	CutIDs    []string
}

func (o *PinnedOverflow) Exceeded() bool {
	if o == nil {
		return false
	}
	return o.Count > o.Cap
}

func (s *Store) PinnedOverflow(ctx context.Context, projectID string, now time.Time, scope map[string]string) (*PinnedOverflow, error) {
	return s.pinnedOverflow(ctx, projectID, now, scope, nil)
}

func (s *Store) pinnedOverflow(ctx context.Context, projectID string, now time.Time, scope map[string]string, skip func(Memory) bool) (*PinnedOverflow, error) {
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
	var pinned []Memory
	for _, m := range pinnedAll {
		if m.ValidFrom != nil || m.ValidUntil != nil || m.VerifiedAt != nil {
			if _, out := ValidityAt(m.ValidFrom, m.ValidUntil, m.VerifiedAt, now); out {
				continue
			}
		}
		if skip != nil && skip(m) {
			continue
		}
		pinned = append(pinned, m)
	}

	count := len(pinned)

	if count <= cap {
		return nil, nil
	}

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

func (s *Store) PinnedOverflows(ctx context.Context, now time.Time, scope map[string]string) ([]PinnedOverflow, error) {
	return s.PinnedOverflowsExcept(ctx, now, scope, nil)
}

func (s *Store) PinnedOverflowsExcept(ctx context.Context, now time.Time, scope map[string]string, skip func(Memory) bool) ([]PinnedOverflow, error) {
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
		ov, err := s.pinnedOverflow(ctx, pid, now, scope, skip)
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
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt > b.UpdatedAt
		}
		return a.ID < b.ID
	}
	ra := float64(a.Importance) * DecayFactor(a.Category, a.Content, a.Retention, true, ageDays(a.CreatedAt, now))
	rb := float64(b.Importance) * DecayFactor(b.Category, b.Content, b.Retention, true, ageDays(b.CreatedAt, now))
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
