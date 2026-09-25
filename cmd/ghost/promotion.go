package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// reflectionApplier is the single write boundary for `ghost reflect`. Keeping
// project replacement, explicit global promotion, and failed-candidate
// recovery behind one call makes their ordering testable and lets the store
// commit them atomically.
type reflectionApplier interface {
	ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []memory.Memory, consolidatedSince string, promoteGlobals bool) (preserved []string, promoted int, keptMems []memory.Memory, err error)
}

// applyReflection converts reflection proposals to store rows and always
// invokes the store apply boundary. Promotion is not nested under a
// project-memory guard: a result containing only cross-project candidates is
// precisely the case where an explicit promotion request must still run.
func applyReflection(ctx context.Context, store reflectionApplier, projectID string, projectMems, globalMems []reflection.ReflectMemory, consolidatedSince string, promoteGlobals bool) (preserved []string, promoted int, keptMems []memory.Memory, err error) {
	if !promoteGlobals && len(globalMems) > 0 {
		projectMems = append(append([]reflection.ReflectMemory(nil), projectMems...), globalMems...)
		globalMems = nil
	}
	projectRows := reflectMemoriesToMemory(projectID, projectMems)
	globalRows := reflectMemoriesToMemory("_global", globalMems)
	if len(projectRows) == 0 && len(globalRows) == 0 {
		return nil, 0, nil, nil
	}
	return store.ApplyReflection(ctx, projectID, projectRows, globalRows, consolidatedSince, promoteGlobals)
}

func reflectMemoriesToMemory(projectID string, mems []reflection.ReflectMemory) []memory.Memory {
	if len(mems) == 0 {
		return nil
	}
	rows := make([]memory.Memory, len(mems))
	for i, m := range mems {
		rows[i] = memory.Memory{
			ProjectID:  projectID,
			Category:   m.Category,
			Content:    m.Content,
			Importance: m.Importance,
			Source:     "reflection",
			Tags:       m.Tags,
		}
	}
	return rows
}

func reflectCategoryParts(mems []reflection.ReflectMemory) string {
	counts := make(map[string]int)
	for _, m := range mems {
		counts[m.Category]++
	}
	categories := make([]string, 0, len(counts))
	for category := range counts {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	parts := make([]string, 0, len(categories))
	for _, category := range categories {
		parts = append(parts, fmt.Sprintf("%d %s", counts[category], category))
	}
	return strings.Join(parts, ", ")
}

// appliedSummary describes the rows actually written to the project. The
// caller passes the post-fold project slice, so a global candidate kept
// project-scoped is counted in both the total and category breakdown.
func appliedSummary(projectMems, globalMems []reflection.ReflectMemory, promoted int, promoteGlobals bool) string {
	summary := fmt.Sprintf("%d memories consolidated", len(projectMems))
	if parts := reflectCategoryParts(projectMems); parts != "" {
		summary += " (" + parts + ")"
	}
	if len(globalMems) == 0 {
		return summary
	}
	if promoteGlobals {
		return summary + fmt.Sprintf(", %d promoted to global", promoted)
	}
	return summary + fmt.Sprintf(", %d cross-project candidates kept project-scoped", len(globalMems))
}

func restoreHint(promoted int) string {
	if promoted > 0 {
		return "(use --restore to undo the project replace; promoted globals must be removed from _global by hand)"
	}
	return "(use --restore to undo)"
}

// recoveryWarning is kept separate from the apply transaction so its count is
// never printed as zero successful recoveries. ApplyReflection rolls back on
// a failed recovery, but this formatter still fails safe if a future caller
// ever supplies a partial result.
func recoveryWarning(kept, total int) string {
	if kept <= 0 {
		return fmt.Sprintf("error: none of the %d unpromoted global memories could be returned to the project either — use --restore or re-run", total)
	}
	return fmt.Sprintf("warning: %d of %d global memories could not be promoted and were kept in the project", kept, total)
}
