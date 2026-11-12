package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// FoldRow is the planner's view of one row: the id and the two stamps that say

type FoldRow struct {
	ID        string
	Content   string
	UpdatedAt string
}

// FoldCluster is one survivor and the rows to fold into it.
type FoldCluster struct {
	Survivor FoldRow
	Folded   []FoldRow
}

// FoldSkip is a row FoldRows refused, and why.
type FoldSkip struct {
	ID     string
	Reason string
}

// FoldResult reports what one FoldRows call did.
type FoldResult struct {
	Folded []string

	Clusters int

	Skipped []FoldSkip

	SnapshotID string
}

func (s *Store) FoldRows(ctx context.Context, projectID string, clusters []FoldCluster, since string) (FoldResult, error) {
	var res FoldResult
	if len(clusters) == 0 {
		return res, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "fold-rows")
	if err != nil {
		return res, fmt.Errorf("begin fold tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	type stored struct {
		importance float64
		tags       []string
	}
	check := func(r FoldRow) (stored, string, error) {
		var (
			st                                stored
			project, source, content, updated string
			created, tagsJSON                 string
			pinned                            bool
			resolved                          sql.NullString
			retention                         string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, source, content, updated_at, created_at, tags, pinned,
			       resolved_at, COALESCE(retention, 'project'), importance
			FROM memories WHERE id = ?`, r.ID).
			Scan(&project, &source, &content, &updated, &created, &tagsJSON, &pinned, &resolved, &retention, &st.importance)
		if errors.Is(err, sql.ErrNoRows) {
			return st, "no longer exists", nil
		}
		if err != nil {
			return st, "", fmt.Errorf("re-read %s: %w", RefusalArg("memory_id", r.ID), err)
		}
		switch {
		case project != projectID:
			return st, "belongs to another project", nil
		case source != "reflection":
			return st, "not reflection-written", nil
		case pinned:
			return st, "pinned", nil
		case resolved.Valid:
			return st, "resolved", nil
		case retention == RetentionPersistent:
			return st, "persistent", nil
		case since != "" && created >= since:
			return st, "saved at or after the run started", nil
		case content != r.Content:
			return st, "content changed since the plan", nil
		case updated != r.UpdatedAt:
			return st, "changed since the plan", nil
		}
		_ = json.Unmarshal([]byte(tagsJSON), &st.tags)
		return st, "", nil
	}

	type plan struct {
		survivor   FoldRow
		surv       stored
		ids        []string
		importance float64
		tags       []string
	}
	var plans []plan
	for _, c := range clusters {
		if c.Survivor.ID == "" || len(c.Folded) == 0 {
			continue
		}
		surv, why, err := check(c.Survivor)
		if err != nil {
			return res, err
		}
		if why != "" {
			res.Skipped = append(res.Skipped, FoldSkip{ID: c.Survivor.ID, Reason: "survivor " + why})
			for _, f := range c.Folded {
				res.Skipped = append(res.Skipped, FoldSkip{ID: f.ID, Reason: "survivor " + why})
			}
			continue
		}
		p := plan{survivor: c.Survivor, surv: surv, importance: surv.importance}
		tagSet := map[string]bool{}
		for _, t := range surv.tags {
			tagSet[t] = true
		}
		for _, f := range c.Folded {
			if f.ID == c.Survivor.ID {
				res.Skipped = append(res.Skipped, FoldSkip{ID: f.ID, Reason: "is the survivor"})
				continue
			}
			st, why, err := check(f)
			if err != nil {
				return res, err
			}
			if why != "" {
				res.Skipped = append(res.Skipped, FoldSkip{ID: f.ID, Reason: why})
				continue
			}
			p.ids = append(p.ids, f.ID)
			if st.importance > p.importance {
				p.importance = st.importance
			}
			for _, t := range st.tags {
				tagSet[t] = true
			}
		}
		if len(p.ids) == 0 {
			continue
		}

		links, over, err := decisionLinksOverflowTx(ctx, tx, projectID, append([]string{c.Survivor.ID}, p.ids...))
		if err != nil {
			return res, err
		}
		if over {
			why := fmt.Sprintf("decision links do not fit in one source_ref (%d link(s), %d-byte bound)", links, MaxSourceRefLen)
			res.Skipped = append(res.Skipped, FoldSkip{ID: c.Survivor.ID, Reason: why})
			for _, id := range p.ids {
				res.Skipped = append(res.Skipped, FoldSkip{ID: id, Reason: why})
			}
			continue
		}
		p.tags = make([]string, 0, len(tagSet)) // non-nil, so an untagged cluster marshals to []
		for t := range tagSet {
			p.tags = append(p.tags, t)
		}
		sort.Strings(p.tags)
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		return res, nil
	}

	snapshotID, err := s.snapshotReplaceableTx(ctx, tx, projectID)
	if err != nil {
		return FoldResult{Skipped: res.Skipped}, err
	}
	res.SnapshotID = snapshotID

	for _, p := range plans {

		consumed := make(map[string]bool, len(p.ids))
		for _, id := range p.ids {
			consumed[id] = true
		}
		merge := Memory{ReplacesIDs: p.ids}
		if _, err := absorbSourcesTx(ctx, tx, projectID, p.survivor.ID, merge, merge, nil, consumed); err != nil {
			return FoldResult{Skipped: res.Skipped}, err
		}
		var tierBefore, tierAfter string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(retention, 'project') FROM memories WHERE id = ?`, p.survivor.ID).Scan(&tierBefore); err != nil {
			return FoldResult{Skipped: res.Skipped}, fmt.Errorf("read survivor retention: %w", err)
		}
		if err := raiseReusedRetentionTx(ctx, tx, projectID, p.survivor.ID, Memory{ReplacesIDs: p.ids}); err != nil {
			return FoldResult{Skipped: res.Skipped}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(retention, 'project') FROM memories WHERE id = ?`, p.survivor.ID).Scan(&tierAfter); err != nil {
			return FoldResult{Skipped: res.Skipped}, fmt.Errorf("read survivor retention: %w", err)
		}
		if err := appendHistoryForIDsTx(ctx, tx, p.ids, phaseDelete, Provenance{}); err != nil {
			return FoldResult{Skipped: res.Skipped}, err
		}
		for _, id := range p.ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ? AND project_id = ?`, id, projectID); err != nil {
				return FoldResult{Skipped: res.Skipped}, fmt.Errorf("delete folded memory: %w", err)
			}
			if err := linkSuccessorTx(ctx, tx, id, p.survivor.ID); err != nil {
				return FoldResult{Skipped: res.Skipped}, err
			}
		}
		tagsJSON, _ := json.Marshal(p.tags)

		if p.importance != p.surv.importance || string(tagsJSON) != marshalTags(p.surv.tags) || tierAfter != tierBefore {
			if _, err := tx.ExecContext(ctx,
				`UPDATE memories SET importance = ?, tags = ?, updated_at = datetime('now') WHERE id = ?`,
				p.importance, string(tagsJSON), p.survivor.ID); err != nil {
				return FoldResult{Skipped: res.Skipped}, fmt.Errorf("update survivor: %w", err)
			}
			if err := appendHistoryForIDsTx(ctx, tx, []string{p.survivor.ID}, phaseReflect, Provenance{}); err != nil {
				return FoldResult{Skipped: res.Skipped}, err
			}
		}
		res.Folded = append(res.Folded, p.ids...)
		res.Clusters++
	}
	s.pruneSnapshotsTx(ctx, tx, projectID)

	if err := tx.Commit(); err != nil {
		return FoldResult{Skipped: res.Skipped}, fmt.Errorf("commit fold: %w", err)
	}
	lock.reportHold("fold-rows", time.Now())
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return res, nil
}

func marshalTags(tags []string) string {
	if tags == nil {
		tags = []string{}
	}
	b, _ := json.Marshal(tags)
	return string(b)
}
