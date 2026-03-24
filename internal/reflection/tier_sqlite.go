package reflection

import (
	"context"
	"strings"
	"unicode"
)

// SQLiteConsolidator performs mechanical deduplication using Jaccard similarity.
// Lowest tier: always available, zero external dependencies, but only merges
// near-duplicates without summarizing or restructuring.
type SQLiteConsolidator struct{}

// NewSQLiteConsolidator creates a consolidator that deduplicates via token overlap.
func NewSQLiteConsolidator() *SQLiteConsolidator {
	return &SQLiteConsolidator{}
}

func (s *SQLiteConsolidator) Name() string { return "sqlite" }

func (s *SQLiteConsolidator) Available(_ context.Context) bool { return true }

func (s *SQLiteConsolidator) Consolidate(_ context.Context, input ReflectionInput) (ReflectionResult, error) {
	mems := input.ExistingMemories
	if len(mems) == 0 {
		return ReflectionResult{LearnedContext: input.CurrentContext}, nil
	}

	// Build token sets for each memory.
	type tokenized struct {
		tokens map[string]bool
	}

	items := make([]tokenized, len(mems))
	for i, m := range mems {
		items[i] = tokenized{tokens: tokenize(m.Content)}
	}

	// Find and merge duplicates (Jaccard >= 0.5, same category only).
	absorbed := make([]bool, len(items))
	var result []ReflectMemory

	for i := range items {
		if absorbed[i] {
			continue
		}

		best := mems[i]
		for j := i + 1; j < len(items); j++ {
			if absorbed[j] {
				continue
			}
			if mems[i].Category != mems[j].Category {
				continue
			}

			sim := jaccard(items[i].tokens, items[j].tokens)
			if sim >= 0.5 {
				absorbed[j] = true
				if mems[j].Importance > best.Importance {
					best.Importance = mems[j].Importance
				}
				if len(mems[j].Content) > len(best.Content) {
					best.Content = mems[j].Content
				}
				// Union tags.
				tagSet := make(map[string]bool)
				for _, t := range best.Tags {
					tagSet[t] = true
				}
				for _, t := range mems[j].Tags {
					tagSet[t] = true
				}
				best.Tags = make([]string, 0, len(tagSet))
				for t := range tagSet {
					best.Tags = append(best.Tags, t)
				}
			}
		}

		result = append(result, ReflectMemory{
			Category:   best.Category,
			Content:    best.Content,
			Importance: best.Importance,
			Tags:       best.Tags,
			Scope:      inferGlobalScope(best.Category, best.Content),
		})
	}

	return ReflectionResult{
		LearnedContext: input.CurrentContext,
		Memories:       result,
	}, nil
}

// tokenize splits text into a set of lowercase word tokens (length > 1).
func tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) > 1 {
			tokens[word] = true
		}
	}
	return tokens
}

// inferGlobalScope uses keyword heuristics to detect memories that apply across
// all repositories rather than being project-specific. Used by the SQLite tier
// which cannot use LLM classification.
func inferGlobalScope(category, content string) string {
	// Preferences and certain facts are strong global signals.
	if category == "preference" {
		return "global"
	}

	lower := strings.ToLower(content)

	// Cross-repo workflow indicators.
	globalPatterns := []string{
		"across all", "all repos", "all projects", "every repo", "every project",
		"cross-repo", "cross-project", "from any repo",
		"deploy to", "deploy from", "push to infra",
		"ssh ", "ssh into", "hostname",
		"always use", "never use", "prefer ",
		"personal tool", "dev machine", "workstation",
		"infrastructure topology", "cluster ",
		"api key", "credential", "token ",
	}
	for _, p := range globalPatterns {
		if strings.Contains(lower, p) {
			return "global"
		}
	}

	return "project"
}

// jaccard computes the Jaccard similarity coefficient between two token sets.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}

	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
