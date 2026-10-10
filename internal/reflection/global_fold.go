package reflection

import (
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// GlobalFoldCluster is one group of near-duplicate _global rows: the row that
// stays and the rows folded into it.
type GlobalFoldCluster struct {
	Survivor memory.Memory
	Folded   []memory.Memory
}

// PlanGlobalFold finds the near-duplicate clusters among the _global rows a
// consolidation may touch and builds the replacement set that folds them. It is
// pure: no store, no model, the same rows always give the same plan.
//
// mems must already be limited to what ReplaceNonManual may replace (not
// pinned, resolved, manual, builtin or persistent), which is what makes "a
// pinned row is never touched" true here: such a row is never in the input, so
// it is never a survivor, a fold target or a row whose content is rewritten.
//
// "Near-duplicate" is duplicateClusters, the rule the SQLite tier applies to a
// project, not a second one. The survivor is the newest row (created_at, then
// id), its text kept verbatim so its embedding and links stay with it; it takes
// the highest importance in the cluster and the union of its tags.
//
// rows is the complete replacement set for ReplaceNonManual: every input row not
// in a cluster is restated as it is (a verbatim re-emission writes nothing), and
// each cluster contributes its survivor with ReplacesIDs naming every member, so
// the folded rows' delete history names the survivor and their evidence is
// carried onto it. Nil when there is nothing to fold, so a caller cannot replace
// a corpus it has no reason to touch.
func PlanGlobalFold(mems []memory.Memory) (clusters []GlobalFoldCluster, rows []memory.Memory) {
	var out []memory.Memory
	for _, idx := range duplicateClusters(mems) {
		if len(idx) == 1 {
			m := mems[idx[0]]
			out = append(out, memory.Memory{
				ProjectID:  "_global",
				Category:   m.Category,
				Content:    m.Content,
				Importance: m.Importance,
				Tags:       m.Tags,
				Source:     m.Source,
			})
			continue
		}
		members := make([]memory.Memory, len(idx))
		for i, j := range idx {
			members[i] = mems[j]
		}
		sort.SliceStable(members, func(a, b int) bool {
			if members[a].CreatedAt != members[b].CreatedAt {
				return members[a].CreatedAt > members[b].CreatedAt
			}
			return members[a].ID > members[b].ID
		})
		survivor := members[0]
		merged := survivor
		tagSet := map[string]bool{}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
			if m.Importance > merged.Importance {
				merged.Importance = m.Importance
			}
			for _, t := range m.Tags {
				tagSet[t] = true
			}
		}
		merged.Tags = make([]string, 0, len(tagSet))
		for t := range tagSet {
			merged.Tags = append(merged.Tags, t)
		}
		sort.Strings(merged.Tags)
		clusters = append(clusters, GlobalFoldCluster{Survivor: survivor, Folded: members[1:]})
		out = append(out, memory.Memory{
			ProjectID:   "_global",
			Category:    survivor.Category,
			Content:     survivor.Content,
			Importance:  merged.Importance,
			Tags:        merged.Tags,
			Source:      survivor.Source,
			ReplacesIDs: ids,
		})
	}
	if len(clusters) == 0 {
		return nil, nil
	}
	return clusters, out
}
