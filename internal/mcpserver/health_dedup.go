package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// maxHealthNearDuplicateProjects bounds how many projects' pair samples the
// health report lists; the total counts every project.
const maxHealthNearDuplicateProjects = 10

// maxHealthNearDuplicatePairsPerProject bounds how many pairs per project are
// sampled; the per-project total is still reported.
const maxHealthNearDuplicatePairsPerProject = 5

func (s *Server) writeNearDuplicateBlock(ctx context.Context, sb *strings.Builder, store *memory.Store, projects []memory.Project) {
	if store == nil {
		return
	}
	type projStat struct {
		name  string
		id    string
		pairs [][2]string
	}
	var stats []projStat
	totalPairs := 0
	for _, p := range projects {
		pairs, err := store.NearDuplicatePairsForProject(ctx, p.ID)
		if err != nil {
			// A failed read is reported rather than dropped; see health pattern.
			fmt.Fprintf(sb, "**Near duplicates:** could not be read for project %s: %v\n", assemble.Label(p.Name), err)
			return
		}
		if len(pairs) == 0 {
			continue
		}
		totalPairs += len(pairs)
		stats = append(stats, projStat{name: p.Name, id: p.ID, pairs: pairs})
	}
	if totalPairs == 0 {
		sb.WriteString("**Near duplicates:** no live near-duplicate pairs\n")
		sb.WriteString("  _global is not consolidated unattended; run `ghost maintenance consolidate-global --apply` to fold its pairs\n")
		return
	}
	fmt.Fprintf(sb, "**Near duplicates:** %d live near-duplicate pair(s) across %d project(s) — these pairs read as duplicates and each loser costs a candidate slot before it is cut; `ghost reflect <project> --apply` merges them (grounded, drop-guarded)\n", totalPairs, len(stats))
	sb.WriteString("  _global is not consolidated unattended; run `ghost maintenance consolidate-global --apply` to fold its pairs\n")
	shownProjects := 0
	for _, st := range stats {
		if shownProjects >= maxHealthNearDuplicateProjects {
			fmt.Fprintf(sb, "  ... and %d more project(s) with pairs\n", len(stats)-shownProjects)
			break
		}
		shownProjects++
		// Bound per-project sample.
		sample := st.pairs
		more := 0
		if len(sample) > maxHealthNearDuplicatePairsPerProject {
			more = len(sample) - maxHealthNearDuplicatePairsPerProject
			sample = sample[:maxHealthNearDuplicatePairsPerProject]
		}
		var parts []string
		for _, pr := range sample {
			parts = append(parts, fmt.Sprintf("%s and %s", assemble.Token(pr[0]), assemble.Token(pr[1])))
		}
		line := fmt.Sprintf("  - %s (%s): %d pair(s): %s", assemble.Label(st.name), assemble.Token(st.id), len(st.pairs), strings.Join(parts, ", "))
		if more > 0 {
			line += fmt.Sprintf(" ... and %d more", more)
		}
		sb.WriteString(line + "\n")
	}
}
