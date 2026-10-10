package reflection

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/memory"
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

func (s *SQLiteConsolidator) Mechanical() bool { return true }

func (s *SQLiteConsolidator) Available(_ context.Context) bool { return true }

func (s *SQLiteConsolidator) Consolidate(_ context.Context, input ReflectionInput) (ReflectionResult, error) {
	mems := input.ExistingMemories
	if len(mems) == 0 {
		return ReflectionResult{LearnedContext: input.CurrentContext}, nil
	}

	// Find and merge duplicates (Jaccard >= 0.5 / full containment, ANY
	// category — the pairing used to be gated on category equality, which
	// let this consolidator re-emit one rule as paraphrases split across
	// preference/gotcha on successive passes; the gate itself is unchanged
	// and still blocks numeric conflicts). The EARLIER memory survives with
	// its category (first in input order — merging must not silently
	// recategorize it), taking max importance, the longest content, and the
	// union of tags, exactly how same-category merges have always behaved;
	// ReplaceNonManual's exact-content reuse at apply time handles the
	// surviving row's embedding/links the same way it already does for
	// same-category merges. The grouping itself is duplicateClusters, shared
	// with the _global fold so the two can never disagree about what a
	// near-duplicate is.
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

// duplicateClusters groups memories by the near-duplicate rule the SQLite tier
// has always used: Jaccard >= 0.5, or full containment of the smaller token set,
// and never across a differing purely-numeric token. Each cluster lists indexes
// into mems, the first being the earliest member in input order; a memory with
// no twin is a cluster of one. Comparison is always against the cluster's first
// member, not against a growing union, so the grouping is a function of input
// order alone.
func duplicateClusters(mems []memory.Memory) [][]int {
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
			sim := jaccard(tokens[i], tokens[j])
			// Containment only fires on full subsumption (the smaller token set
			// entirely inside the larger). A lower bar would merge partial
			// overlaps ("deploy staging" vs "deploy production") that are
			// distinct facts; Jaccard already handles same-length restatements.
			if c := containment(tokens[i], tokens[j]); c == 1.0 {
				sim = 1.0
			}
			if sim >= 0.5 && !numericConflict(tokens[i], tokens[j]) {
				absorbed[j] = true
				cluster = append(cluster, j)
			}
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

// stopwords are filler words that carry no consolidation signal; dropping them
// keeps Jaccard/containment from being diluted on longer memories.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

// tokenize splits text into a set of lowercase word tokens, excluding
// stopwords. Word tokens shorter than two characters are dropped — except
// purely-numeric tokens, which are retained at length one ("port 8" vs "port 9"
// must differ), since single-digit numbers are high-signal facts and the
// numericConflict guard depends on seeing them.
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

// inferGlobalScope uses keyword heuristics to detect memories that apply across
// all repositories rather than being project-specific. Used by the SQLite tier
// which cannot use LLM classification. Secret-looking content is never assigned
// global scope: the shared check in secrets.go applies to both reflection
// tiers, and global memories are replayed into every project's injected
// context, so promoting a credential here would widen its blast radius.
func inferGlobalScope(category, content string) string {
	lower := strings.ToLower(content)

	if looksLikeSecret(lower) {
		return "project"
	}

	// Explicit project-scoping language wins even over multiple weak-pattern
	// hits below: "deploy to the project cluster for maintenance" hits both
	// "deploy to" and "cluster " but is unambiguously project-specific.
	projectScopedMarkers := []string{
		"this project", "this repo", "the project", "for this service",
	}
	for _, p := range projectScopedMarkers {
		if strings.Contains(lower, p) {
			return "project"
		}
	}

	// A fact that names one specific host, cluster or node is an operations
	// note about that machine, whatever else the sentence says: promoting it
	// replayed "relay-3 needs a restart after a kernel update" into every
	// project's context (#966). The marker is a concrete identifier, never a
	// bare noun, so "the shared cluster" is still a weak hit like any other.
	if namesSingleTarget(lower) {
		return "project"
	}

	// Unambiguous cross-repo/personal-environment language: one hit is enough.
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

	// Ambiguous DevOps phrasing reads the same whether the memory is
	// project-specific or genuinely cross-repo (e.g. "SSH into relay 3 to
	// restart the block producer" is project-specific, not global — see #319).
	// A single hit isn't enough signal on its own; require two independent
	// hits before promoting.
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

// singleTargetRe matches an identifier for ONE machine: an infrastructure noun
// followed by a number ("relay-3", "node5", "staging-app-2", "bp-1a"), an IPv4
// address, or an internal DNS name. A bare noun ("cluster", "host") does not
// match, and neither does a version-like token ("sha-256", "utf-8"), because the
// prefix has to be an infrastructure noun.
var singleTargetRe = regexp.MustCompile(
	`\b(?:relay|node|host|server|cluster|worker|master|bp|vm|bastion|prod|production|staging|dev)(?:-[a-z0-9]+)*-?\d[a-z0-9-]*\b` +
		`|\b\d{1,3}(?:\.\d{1,3}){3}\b` +
		`|\b[a-z0-9][a-z0-9-]*\.(?:internal|local|lan|home\.arpa)\b`)

// namesSingleTarget reports whether lower (already lowercased) names one
// specific host, node or cluster.
func namesSingleTarget(lower string) bool {
	return singleTargetRe.MatchString(lower)
}

// containment is the overlap coefficient |A∩B| / min(|A|,|B|): it catches a
// memory that is a strict subset of another ("use sqlite" inside "use sqlite
// for storage"), which symmetric Jaccard scores too low to merge.
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

// numericConflict reports whether the two token sets differ on a purely-numeric
// token — a precise fact ("port 80" vs "port 81") that must not be merged even
// when the surrounding words overlap heavily.
func numericConflict(a, b map[string]bool) bool {
	for token := range a {
		if isNumericToken(token) && !b[token] {
			return true
		}
	}
	for token := range b {
		if isNumericToken(token) && !a[token] {
			return true
		}
	}
	return false
}

func isNumericToken(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
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
