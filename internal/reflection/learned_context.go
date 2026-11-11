package reflection

import (
	"log/slog"
	"strings"
)

func guardLearnedContext(result *ReflectionResult, input ReflectionInput, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	proposed := result.LearnedContext
	if proposed == "" {
		return
	}
	refuse := func(reason, key string, found []string) {
		logger.Warn("reflection dropped the learned context: it "+reason+"; keeping the previous one",
			key, strings.Join(found, ","), "bytes", len(proposed))
		result.LearnedContext = input.CurrentContext
	}
	if shas := unknownSHAs(proposed, knownSHAs(input)); len(shas) > 0 {
		refuse("carries a fabricated commit SHA", "tokens", shas)
		return
	}
	if len(input.OtherProjectNames) > 0 {
		if hit := foreignNamesIn(proposed, forbiddenProjectNames(input)); len(hit) > 0 {
			refuse("names a project absent from the input", "projects", hit)
			return
		}
	}
	if unknown := unknownIdentifiers(proposed, learnedContextSources(input)); len(unknown) > 0 {
		refuse("carries identifiers absent from the input", "identifiers", unknown)
	}
}

func learnedContextSources(input ReflectionInput) []string {
	sources := make([]string, 0, len(input.ExistingMemories)+len(input.LastCommits)+2)
	sources = append(sources, input.ProjectName, input.ProjectLanguage)
	sources = append(sources, input.LastCommits...)
	for _, m := range input.ExistingMemories {
		sources = append(sources, m.Content)
	}
	return sources
}
