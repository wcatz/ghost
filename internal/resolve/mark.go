// The targeted mark: stamp resolved_at on the memories a caller NAMES, on an
package resolve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// MarkStore is the subset of *memory.Store a targeted mark needs; narrowed for
type MarkStore interface {
	memref.Store
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	MarkResolved(ctx context.Context, projectID string, ids []string, prov memory.Provenance) ([]string, error)
}

// MarkRequest is one operator's request: the refs as typed, the project they are
type MarkRequest struct {
	ProjectID  string
	Refs       []string
	Provenance memory.Provenance
	Apply      bool
}

// MarkedMemory is one ref resolved to the row it names, and what happened to it.
type MarkedMemory struct {
	ID              string
	Ref             string
	Content         string
	Category        string
	Marked          bool
	AlreadyResolved bool
	Pinned          bool
	ExemptCategory  bool
	PersistentTier  bool
	Declined        bool
	MarkFailed      bool
}

// MarkResult summarizes a request. Resolved counts the refs that named a row this
type MarkResult struct {
	Resolved        int
	Marked          int
	AlreadyResolved int
	Pinned          int
	ExemptCategory  int
	PersistentTier  int
	Declined        int
	Memories        []MarkedMemory
}

// Mark stamps resolved_at on the memories the request names and returns them
func Mark(ctx context.Context, store MarkStore, req MarkRequest, logger *slog.Logger) (MarkResult, error) {
	var res MarkResult
	if req.ProjectID == "" {
		return res, fmt.Errorf("resolve --mark: a project is required, so no memory can be named from it")
	}
	if req.ProjectID == memory.GlobalProjectID {
		return res, fmt.Errorf("nothing marked: refusing to mark in the %s project — it holds the promoted memories every project injects, and a lifecycle pass over one project cannot decide for all of them", memory.GlobalProjectID)
	}
	if len(req.Refs) == 0 {
		return res, fmt.Errorf("no memory to mark: name one as --mark <id> (a full id, or 8 or more characters of one)")
	}

	var problems []string
	rows := make([]MarkedMemory, 0, len(req.Refs))
	seen := make(map[string]bool, len(req.Refs))
	for _, ref := range req.Refs {
		row, err := resolveMarkRef(ctx, store, req.ProjectID, ref)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		rows = append(rows, row)
	}
	if len(problems) > 0 {
		return MarkResult{}, fmt.Errorf("nothing marked: %s", strings.Join(problems, "; "))
	}
	res.Resolved = len(rows)

	if err := attachMarkState(ctx, store, req.ProjectID, rows); err != nil {
		return MarkResult{}, err
	}
	res.AlreadyResolved = countMarked(rows, func(m *MarkedMemory) bool { return m.AlreadyResolved })
	res.Pinned = countMarked(rows, func(m *MarkedMemory) bool { return m.Pinned })
	res.ExemptCategory = countMarked(rows, func(m *MarkedMemory) bool { return m.ExemptCategory })
	res.PersistentTier = countMarked(rows, func(m *MarkedMemory) bool { return m.PersistentTier })

	res.Memories = rows
	if !req.Apply {
		return res, nil
	}

	stampable := make([]string, 0, len(rows))
	for _, m := range rows {
		stampable = append(stampable, m.ID)
	}
	stamped, err := store.MarkResolved(ctx, req.ProjectID, stampable, req.Provenance)
	if err != nil {
		for i := range rows {
			rows[i].MarkFailed = true
		}
		res.Memories = rows
		return res, fmt.Errorf("mark resolved in %s: %w", req.ProjectID, err)
	}
	written := make(map[string]bool, len(stamped))
	for _, id := range stamped {
		written[id] = true
	}
	for i := range rows {
		switch {
		case written[rows[i].ID]:
			rows[i].Marked = true
		case rows[i].AlreadyResolved || rows[i].Pinned || rows[i].ExemptCategory || rows[i].PersistentTier:
		default:
			rows[i].Declined = true
		}
	}
	res.Marked = len(stamped)
	res.Declined = countMarked(rows, func(m *MarkedMemory) bool { return m.Declined })
	if logger != nil {
		logger.Info("resolve marked named memories",
			"project", req.ProjectID, "resolved", res.Resolved, "marked", res.Marked,
			"already_resolved", res.AlreadyResolved, "pinned", res.Pinned, "exempt", res.ExemptCategory,
			"persistent", res.PersistentTier,
			"declined", res.Declined)
	}
	return res, nil
}

func resolveMarkRef(ctx context.Context, store MarkStore, projectID, ref string) (MarkedMemory, error) {
	var row MarkedMemory
	id, err := memref.Resolve(ctx, store, projectID, "id", ref)
	if err != nil {
		return row, err
	}
	row.ID, row.Ref = id, ref
	return row, nil
}

func attachMarkState(ctx context.Context, store MarkStore, projectID string, rows []MarkedMemory) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("load the memories to mark: %w", err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	var problems []string
	for i := range rows {
		m, ok := byID[rows[i].ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("the id ref %q named a memory that is not there to mark — it was deleted between resolving the ref and loading it; re-run to see what %s holds now",
				rows[i].Ref, projectID))
			continue
		}
		if m.ProjectID != projectID {
			problems = append(problems, fmt.Sprintf("the id ref %q is memory %s, which belongs to %s and not to %s: only a memory in the project you named can be marked there",
				rows[i].Ref, memref.Short(m.ID), m.ProjectID, projectID))
			continue
		}
		rows[i].Content, rows[i].Category = m.Content, m.Category
		rows[i].AlreadyResolved = m.ResolvedAt != nil && *m.ResolvedAt != ""
		rows[i].Pinned = m.Pinned
		rows[i].ExemptCategory = m.Category == "convention" || m.Category == "preference"
		rows[i].PersistentTier = memory.RetentionExempt(m)
	}
	if len(problems) > 0 {
		return fmt.Errorf("nothing marked: %s", strings.Join(problems, "; "))
	}
	return nil
}

func countMarked(rows []MarkedMemory, pred func(*MarkedMemory) bool) int {
	n := 0
	for i := range rows {
		if pred(&rows[i]) {
			n++
		}
	}
	return n
}
