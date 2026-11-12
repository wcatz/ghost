package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/secret"
)

type reflectionApplier interface {
	ApplyReflection(ctx context.Context, projectID string, projectMems, globalMems []memory.Memory, consolidatedSince string, promoteGlobals bool, fed []memory.CorpusRow) (preserved []string, promoted int, keptMems []memory.Memory, projectDropped, globalDropped memory.EmissionDrops, deleted int, appliedByCategory map[string]int, err error)
}

type reflectionApplied struct {
	KeptProject       []reflection.ReflectMemory
	KeptGlobal        []reflection.ReflectMemory
	Preserved         []string
	Promoted          int
	KeptMems          []memory.Memory
	ProjectDropped    memory.EmissionDrops
	GlobalDropped     memory.EmissionDrops
	Deleted           int
	AppliedByCategory map[string]int
	CredDropped       int
	Applied           bool
}

func applyReflection(ctx context.Context, store reflectionApplier, projectID string, projectMems, globalMems []reflection.ReflectMemory, consolidatedSince string, promoteGlobals bool, replaced map[replacedKey][]string, fed []memory.CorpusRow) (reflectionApplied, error) {
	keptProject, keptCrossProject, credDropped := dropCredentialProposals(projectMems, globalMems)
	projectMems = keptProject
	projectRows := reflectMemoriesToMemory(projectID, projectMems, replaced)
	globalRows := reflectMemoriesToMemory("_global", keptCrossProject, replaced)
	if len(projectRows) == 0 && len(globalRows) == 0 {
		return reflectionApplied{
			KeptProject: keptProject,
			KeptGlobal:  keptCrossProject,
		}, nil
	}
	preserved, promoted, kept, projectDropped, globalDropped, deleted, appliedByCategory, err := store.ApplyReflection(ctx, projectID, projectRows, globalRows, consolidatedSince, promoteGlobals, fed)
	if err != nil {
		return reflectionApplied{
			KeptProject: keptProject,
			KeptGlobal:  keptCrossProject,
		}, err
	}
	if credDropped > 0 && len(projectRows) > 0 {
		fmt.Fprintln(os.Stderr,
			"note: the project replace has just removed any stored memory the dropped proposal(s) were carrying forward, because it held a credential value. A manual, builtin, pinned or resolved row is not replaceable and would still be there: check the project")
	}
	applied := projectDropped.Total()+globalDropped.Total() != len(projectRows)+len(globalRows) || deleted != 0
	return reflectionApplied{
		KeptProject:       keptProject,
		KeptGlobal:        keptCrossProject,
		Preserved:         preserved,
		Promoted:          promoted,
		KeptMems:          kept,
		ProjectDropped:    projectDropped,
		GlobalDropped:     globalDropped,
		Deleted:           deleted,
		AppliedByCategory: appliedByCategory,
		CredDropped:       credDropped,
		Applied:           applied,
	}, nil
}

func displayProposal(content, category string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return humanStoredText(content, limit)
}

func displayClaim(content string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, "", content)
	}
	return humanStoredText(content, limit)
}

func withheld(finding secret.Finding, category, content string) string {
	if category == "" {
		return fmt.Sprintf("<withheld: %s, bytes=%d>", finding.Label, len(content))
	}
	return fmt.Sprintf("<withheld: %s, category=%s, bytes=%d>", finding.Label, category, len(content))
}

func humanStoredText(content string, limit int) string {
	if limit <= 0 {
		return assemble.Data(content)
	}
	return assemble.PreviewLine(content, limit)
}

func dropCredentialProposals(projectMems, globalMems []reflection.ReflectMemory) (keptProject, keptGlobal []reflection.ReflectMemory, dropped int) {
	keep := func(in []reflection.ReflectMemory) (out []reflection.ReflectMemory, n int) {
		out = make([]reflection.ReflectMemory, 0, len(in))
		for _, m := range in {
			finding, ok := secret.Detect(m.Content)
			if !ok {
				out = append(out, m)
				continue
			}
			n++
			fmt.Fprintf(os.Stderr,
				"note: consolidation proposal not applied — it holds a credential value (format=%s category=%s scope=%s bytes=%d)\n",
				finding.Label, m.Category, m.Scope, len(m.Content))
		}
		return out, n
	}
	keptProject, n := keep(projectMems)
	keptGlobal, m := keep(globalMems)
	return keptProject, keptGlobal, n + m
}

type replacedKey struct {
	text string
	id   string
}

func reflectMemoriesToMemory(projectID string, mems []reflection.ReflectMemory, replaced map[replacedKey][]string) []memory.Memory {
	if len(mems) == 0 {
		return nil
	}
	rows := make([]memory.Memory, len(mems))
	for i, m := range mems {
		var replaces []string
		if m.RestatesID == "" {
			replaces = replaced[replacedKey{text: m.Content}]
		} else {
			replaces = replaced[replacedKey{id: m.RestatesID}]
		}
		rows[i] = memory.Memory{
			ProjectID:   projectID,
			Category:    m.Category,
			Content:     m.Content,
			Importance:  m.Importance,
			Source:      "reflection",
			Tags:        m.Tags,
			ReplacesIDs: replaces,
		}
	}
	return rows
}

func replacedIDsFor(result *reflection.ReflectionResult) map[replacedKey][]string {
	out := map[replacedKey][]string{}
	if result == nil {
		return out
	}
	for _, m := range result.Merges {
		key, _ := memory.ClampContent(m.Text)
		out[replacedKey{text: key}] = append(out[replacedKey{text: key}], m.IDs...)
	}
	for _, r := range result.Replacements {
		key, _ := memory.ClampContent(r.Text)
		out[replacedKey{text: key}] = append(out[replacedKey{text: key}], r.ID)
	}
	for _, d := range result.Drops {
		if d.Successor == "" {
			continue
		}
		out[replacedKey{id: d.Successor}] = append(out[replacedKey{id: d.Successor}], d.ID)
	}
	return out
}

func reflectCategoryParts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
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

func reflectResultLine(result reflection.ReflectionResult) string {
	counts := make(map[string]int, len(result.Memories))
	for _, m := range result.Memories {
		counts[m.Category]++
	}
	return fmt.Sprintf("Result:       %d memories (%s)%s\n", len(result.Memories), reflectCategoryParts(counts), reflectRepairNote(result.RepairTurns))
}

func reflectRepairNote(turns int) string {
	if turns <= 0 {
		return ""
	}
	return fmt.Sprintf("  repair: %d (a rejected response was re-read)", turns)
}

func crossProjectKept(globalMems []reflection.ReflectMemory, globalDropped int) int {
	if kept := len(globalMems) - globalDropped; kept > 0 {
		return kept
	}
	return 0
}

func appliedSummary(appliedByCategory map[string]int, globalMems []reflection.ReflectMemory, promoted, globalDropped int, promoteGlobals bool) string {
	total := 0
	for _, n := range appliedByCategory {
		total += n
	}
	summary := fmt.Sprintf("%d memories consolidated", total)
	if parts := reflectCategoryParts(appliedByCategory); parts != "" {
		summary += " (" + parts + ")"
	}
	if len(globalMems) == 0 {
		return summary
	}
	if promoteGlobals {
		return summary + fmt.Sprintf(", %d promoted to global", promoted)
	}
	return summary + fmt.Sprintf(", %d cross-project candidates kept project-scoped", crossProjectKept(globalMems, globalDropped))
}

func writeAppliedReport(w io.Writer, applied, promoteGlobals bool, appliedByCategory map[string]int,
	keptProject, keptGlobal []reflection.ReflectMemory, keptMems []memory.Memory,
	projectForSummary []reflection.ReflectMemory, promoted, droppedGlobal, deleted int) (summary string, wrote bool) {
	if !applied || (promoted == 0 && len(keptMems) == 0 && len(keptProject) == 0 && len(keptGlobal) == 0) {
		return "", false
	}
	summaryGlobalDropped := 0
	if !promoteGlobals {
		summaryGlobalDropped = droppedGlobal
	}
	summary = appliedSummary(appliedByCategory, keptGlobal, promoted, summaryGlobalDropped, promoteGlobals)
	line := summary
	if deleted > 0 {
		line += fmt.Sprintf(", %d removed", deleted)
	}
	_, _ = fmt.Fprintf(w, "Applied: %s\n", line)
	if !promoteGlobals && crossProjectKept(keptGlobal, summaryGlobalDropped) > 0 {
		_, _ = fmt.Fprintln(w, "(re-run with --promote-globals to inject them into every project)")
	}
	if len(projectForSummary) > 0 {
		_, _ = fmt.Fprintln(w, restoreHint(promoted))
	} else if promoted > 0 {
		_, _ = fmt.Fprintln(w, "(promoted globals must be removed from _global by hand; no project snapshot was created)")
	}
	return summary, true
}

func restoreHint(promoted int) string {
	if promoted > 0 {
		return "(use --restore to undo the project replace; promoted globals stay in _global with the evidence and decision links they carried from the replaced rows, and must be removed from _global by hand)"
	}
	return "(use --restore to undo)"
}

func recoveryWarning(kept, total int) string {
	if kept <= 0 {
		return fmt.Sprintf("error: none of the %d unpromoted global memories could be returned to the project either — use --restore or re-run", total)
	}
	return fmt.Sprintf("warning: %d of %d global memories could not be promoted and were kept in the project", kept, total)
}
