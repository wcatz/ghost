package reflection

import (
	"log/slog"
	"strings"
)

// guardLearnedContext runs the content checks a proposed memory passes over
// the learned_context a harness returned: no commit SHA the input does not
// hold, no project the input never mentions, and no path, host, version or
// number the input does not carry. The paragraph is injected at every session
// start and read back into the next round, so an unchecked one is where a
// claim the memory guards refused would otherwise land.
//
// A paragraph that fails any check is dropped whole and the previous
// learned_context is kept, the same fallback the mechanical tier uses. It is
// not repaired: stripping the offending claim would leave a paragraph the
// harness never wrote. It never fails the response either, because a bad
// summary must cost the summary, not the consolidation beside it. On a first
// reflection the previous value is empty, and the writer already skips an
// empty one.
//
// The grounding is the input corpus without the stored learned_context: that
// text is the harness's own earlier output, so a specific only it carries was
// never traced to anything.
//
// The refusal is reported like a memory drop, by the guard that tripped and
// the tokens it found, but the paragraph itself is not echoed: only its size.
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

// learnedContextSources are the texts a learned_context's identifiers may be
// grounded in: every input memory, the recent commits, and the project's own
// name and language. Not the stored learned_context, for the reason
// guardLearnedContext gives.
func learnedContextSources(input ReflectionInput) []string {
	sources := make([]string, 0, len(input.ExistingMemories)+len(input.LastCommits)+2)
	sources = append(sources, input.ProjectName, input.ProjectLanguage)
	sources = append(sources, input.LastCommits...)
	for _, m := range input.ExistingMemories {
		sources = append(sources, m.Content)
	}
	return sources
}
