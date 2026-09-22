package reflection

import (
	"log/slog"
	"regexp"
	"strings"
)

// foreignNameRe matches a known foreign project name as a whole token.
// Built per call with regexp.QuoteMeta so project names containing regex
// metacharacters cannot alter the pattern. Case-insensitive: project names
// are identifiers in prose, not case-sensitive keys.
func foreignNameRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
}

// dropForeignProjectMemories removes emitted memories that name a known other
// project which never appears in the input corpus. A consolidation run only
// sees one project's memories, context, and commits — it cannot legitimately
// learn a new fact about a different project. Names that already appear in
// the input are allowed (cross-references the corpus itself makes, e.g. a
// ghost memory that mentions dingo, stay). Modeled on dropFabricatedMemories:
// one contaminated memory must not nuke the run, and every drop is logged so
// a clean pass is distinguishable from a silent loss. No-op when
// input.OtherProjectNames is empty (guard not wired).
func dropForeignProjectMemories(result *ReflectionResult, input ReflectionInput, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(input.OtherProjectNames) == 0 || len(result.Memories) == 0 {
		return
	}

	// Names traceable to the input corpus are not foreign for this run.
	traceable := make(map[string]bool, len(input.OtherProjectNames))
	for _, name := range input.OtherProjectNames {
		if name == "" || name == input.ProjectName {
			continue
		}
		if foreignNameRe(name).MatchString(inputCorpus(input)) {
			traceable[name] = true
		}
	}

	forbidden := make([]string, 0, len(input.OtherProjectNames))
	for _, name := range input.OtherProjectNames {
		if name == "" || name == input.ProjectName || traceable[name] {
			continue
		}
		forbidden = append(forbidden, name)
	}
	if len(forbidden) == 0 {
		return
	}

	kept := result.Memories[:0]
	for _, m := range result.Memories {
		var hit []string
		for _, name := range forbidden {
			if foreignNameRe(name).MatchString(m.Content) {
				hit = append(hit, name)
			}
		}
		if len(hit) == 0 {
			kept = append(kept, m)
			continue
		}
		logger.Warn("reflection dropped a memory naming a project absent from the input",
			"category", m.Category, "projects", strings.Join(hit, ","),
			"preview", previewContent(m.Content))
	}
	result.Memories = kept
}

// inputCorpus concatenates every input field a foreign project name could
// legitimately appear in. Built once per drop pass so each name is searched
// against a single string rather than re-scanning per memory.
func inputCorpus(input ReflectionInput) string {
	var sb strings.Builder
	sb.WriteString(input.ProjectName)
	sb.WriteByte('\n')
	sb.WriteString(input.CurrentContext)
	sb.WriteByte('\n')
	for _, c := range input.LastCommits {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	for _, m := range input.ExistingMemories {
		sb.WriteString(m.Content)
		sb.WriteByte('\n')
	}
	return sb.String()
}
