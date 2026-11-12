package reflection

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/memory"
)

// SQLiteConsolidator performs mechanical deduplication using Jaccard similarity.
type SQLiteConsolidator struct{}

// NewSQLiteConsolidator creates a consolidator that deduplicates via token overlap.
func NewSQLiteConsolidator() *SQLiteConsolidator {
	return &SQLiteConsolidator{}
}

func (s *SQLiteConsolidator) Name() string { return "sqlite" }

func (s *SQLiteConsolidator) Mechanical() bool { return true }

func (s *SQLiteConsolidator) Available(_ context.Context) bool { return true }

func (s *SQLiteConsolidator) Consolidate(_ context.Context, input ReflectionInput) (ReflectionResult, error) {
	mems := input.ExistingMemories
	if len(mems) == 0 {
		return ReflectionResult{LearnedContext: input.CurrentContext}, nil
	}

	var result []ReflectMemory

	for _, cluster := range duplicateClusters(mems) {
		best := mems[cluster[0]]
		for _, j := range cluster[1:] {
			if mems[j].Importance > best.Importance {
				best.Importance = mems[j].Importance
			}
			if len(mems[j].Content) > len(best.Content) {
				best.Content = mems[j].Content
			}
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

func duplicateClusters(mems []memory.Memory) [][]int {
	return duplicateClustersWhere(mems, nil)
}

func compatibleWithAll(compatible func(a, b memory.Memory) bool, mems []memory.Memory, cluster []int, j int) bool {
	if compatible == nil {
		return true
	}
	for _, m := range cluster {
		if !compatible(mems[m], mems[j]) {
			return false
		}
	}
	return true
}

func duplicateClustersWhere(mems []memory.Memory, compatible func(a, b memory.Memory) bool) [][]int {
	tokens := make([]map[string]bool, len(mems))
	for i, m := range mems {
		tokens[i] = tokenize(m.Content)
	}
	absorbed := make([]bool, len(mems))
	var clusters [][]int
	for i := range mems {
		if absorbed[i] {
			continue
		}
		cluster := []int{i}
		for j := i + 1; j < len(mems); j++ {
			if absorbed[j] {
				continue
			}
			if !memory.AreNearDuplicatesTokens(tokens[i], tokens[j]) {
				continue
			}
			if !compatibleWithAll(compatible, mems, cluster, j) {
				continue
			}
			absorbed[j] = true
			cluster = append(cluster, j)
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

func tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if isNumericToken(word) {
			tokens[word] = true
			continue
		}
		if len(word) > 1 && !stopwords[word] {
			tokens[word] = true
		}
	}
	return tokens
}

func inferGlobalScope(category, content string) string {
	lower := strings.ToLower(content)

	if looksLikeSecret(lower) {
		return "project"
	}

	projectScopedMarkers := []string{
		"this project", "this repo", "the project", "for this service",
	}
	for _, p := range projectScopedMarkers {
		if strings.Contains(lower, p) {
			return "project"
		}
	}

	if namesSingleTarget(lower) {
		return "project"
	}

	strongPatterns := []string{
		"across all", "all repos", "all projects", "every repo", "every project",
		"cross-repo", "cross-project", "from any repo",
		"personal tool", "dev machine", "workstation",
		"infrastructure topology",
	}
	for _, p := range strongPatterns {
		if strings.Contains(lower, p) {
			return "global"
		}
	}

	weakPatterns := []string{
		"deploy to", "deploy from", "push to infra",
		"ssh ", "hostname",
		"always use", "never use", "prefer ",
		"cluster ",
	}
	hits := 0
	for _, p := range weakPatterns {
		if strings.Contains(lower, p) {
			hits++
			if hits >= 2 {
				return "global"
			}
		}
	}

	return "project"
}

var singleTargetRe = regexp.MustCompile(
	`\b(?:relay|node|host|server|cluster|worker|master|bp|vm|bastion|prod|production|staging|dev)(?:-[a-z0-9]+)*-?\d[a-z0-9-]*\b` +
		`|\b\d{1,3}(?:\.\d{1,3}){3}\b` +
		`|\b[a-z0-9][a-z0-9-]*\.(?:internal|local|lan|home\.arpa)\b`)

var singleRepoRe = regexp.MustCompile(`\b(?:the|in|from|to)\s+(?:(?:the|our|your|their|my)\s+)?([a-z0-9][\w.-]*)\s+repo(?:sitory)?\b`)

func hostedRepoRef(lower string) bool {
	isName := func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	}
	ownerRepo := func(rest string) bool {
		owner := 0
		for owner < len(rest) && isName(rune(rest[owner])) {
			owner++
		}
		if owner == 0 || owner >= len(rest) || rest[owner] != '/' {
			return false
		}
		return owner+1 < len(rest) && isName(rune(rest[owner+1]))
	}
	for _, host := range []string{"github.com/", "gitlab.com/"} {
		for from := 0; ; {
			i := strings.Index(lower[from:], host)
			if i < 0 {
				break
			}
			i += from
			start := i
			if strings.HasSuffix(lower[:i], "www.") {
				start -= len("www.") // the same host
			}
			if (start == 0 || !isName(rune(lower[start-1]))) && ownerRepo(lower[i+len(host):]) {
				return true
			}
			from = i + len(host)
		}
	}
	for from := 0; ; {
		i := strings.Index(lower[from:], "git@")
		if i < 0 {
			return false
		}
		i += from
		rest := lower[i+len("git@"):]
		if colon := strings.IndexByte(rest, ':'); colon > 0 && ownerRepo(rest[colon+1:]) {
			return true
		}
		from = i + len("git@")
	}
}

var repoQuantifiers = map[string]bool{
	"a": true, "an": true, "any": true, "every": true, "all": true, "each": true,
	"other": true, "another": true, "this": true, "that": true, "same": true,
	"which": true, "the": true, "our": true, "your": true, "their": true, "my": true, "one": true, "whole": true, "entire": true, "git": true,
}

func namesSingleTarget(lower string) bool {
	if singleTargetRe.MatchString(lower) {
		return true
	}
	if hostedRepoRef(lower) {
		return true
	}
	for _, m := range singleRepoRe.FindAllStringSubmatch(lower, -1) {
		if !repoQuantifiers[m[1]] {
			return true
		}
	}
	return false
}

func containment(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	denom := len(a)
	if len(b) < denom {
		denom = len(b)
	}
	return float64(intersection) / float64(denom)
}

func isNumericToken(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}
