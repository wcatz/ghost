package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

const maxHealthNearDuplicateProjects = 10

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
	scanCapped := false
	for _, p := range projects {
		pairs, capped, err := store.NearDuplicatePairsForProject(ctx, p.ID)
		if err != nil {
			fmt.Fprintf(sb, "**Near duplicates:** could not be read for project %s: %v\n", assemble.Label(p.Name), err)
			return
		}
		if capped {
			scanCapped = true
		}
		if len(pairs) == 0 {
			continue
		}
		totalPairs += len(pairs)
		stats = append(stats, projStat{name: p.Name, id: p.ID, pairs: pairs})
	}
	if totalPairs == 0 {
		sb.WriteString("**Near duplicates:** no live near-duplicate pairs\n")
		return
	}
	fmt.Fprintf(sb, "**Near duplicates:** %d live near-duplicate pair(s) across %d project(s) — these pairs read as duplicates and each loser costs a candidate slot before it is cut; `ghost reflect <project> --apply` merges them (grounded, drop-guarded)\n", totalPairs, len(stats))
	if scanCapped {
		sb.WriteString("  _near-duplicate scan capped at 2000 rows for at least one project; rows beyond the bound were not considered (scan capped/cut)\n")
	}
	shownProjects := 0
	for _, st := range stats {
		if shownProjects >= maxHealthNearDuplicateProjects {
			fmt.Fprintf(sb, "  ... and %d more project(s) with pairs\n", len(stats)-shownProjects)
			break
		}
		shownProjects++
		sample := st.pairs
		more := 0
		if len(sample) > maxHealthNearDuplicatePairsPerProject {
			more = len(sample) - maxHealthNearDuplicatePairsPerProject
			sample = sample[:maxHealthNearDuplicatePairsPerProject]
		}
		var parts []string
		for _, pr := range sample {
			parts = append(parts, fmt.Sprintf("%s and %s", memory.RefusalArg("memory_id", pr[0]), memory.RefusalArg("memory_id", pr[1])))
		}
		line := fmt.Sprintf("  - %s (%s): %d pair(s): %s", assemble.Label(st.name), assemble.Token(st.id), len(st.pairs), strings.Join(parts, ", "))
		if more > 0 {
			line += fmt.Sprintf(" ... and %d more", more)
		}
		sb.WriteString(line + "\n")
	}
}
