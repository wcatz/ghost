package reflection

import (
	"log/slog"
	"regexp"
	"strings"
)

func foreignNameRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
}

func dropForeignProjectMemories(result *ReflectionResult, input ReflectionInput, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(input.OtherProjectNames) == 0 || len(result.Memories) == 0 {
		return
	}
	forbidden := forbiddenProjectNames(input)
	if len(forbidden) == 0 {
		return
	}

	kept := result.Memories[:0]
	for _, m := range result.Memories {
		hit := foreignNamesIn(m.Content, forbidden)
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

func forbiddenProjectNames(input ReflectionInput) []string {
	corpus := inputCorpus(input)
	forbidden := make([]string, 0, len(input.OtherProjectNames))
	for _, name := range input.OtherProjectNames {
		if name == "" || name == input.ProjectName {
			continue
		}
		if foreignNameRe(name).MatchString(corpus) {
			continue
		}
		forbidden = append(forbidden, name)
	}
	return forbidden
}

func foreignNamesIn(text string, forbidden []string) []string {
	var hit []string
	for _, name := range forbidden {
		if foreignNameRe(name).MatchString(text) {
			hit = append(hit, name)
		}
	}
	return hit
}

func inputCorpus(input ReflectionInput) string {
	var sb strings.Builder
	sb.WriteString(input.ProjectName)
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
