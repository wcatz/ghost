package reflection

import (
	"strings"
)

// DroppedGuarded is an input memory, in any category, that has no close
// survivor in the consolidation output. Importance and Tags are carried so the
type DroppedGuarded struct {
	ID         string
	Category   string
	Content    string
	Importance float32
	Tags       []string
}

const dropContainmentThreshold = 0.45

// AuditGuardedDrops returns every input memory that has no token-overlap
// survivor among the result memories, whatever its category. Every category is
func AuditGuardedDrops(input ReflectionInput, result ReflectionResult) []DroppedGuarded {
	outTokens := make([]map[string]bool, 0, len(result.Memories))
	present := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		tokens := tokenize(m.Content)
		outTokens = append(outTokens, tokens)
		present[m.Content] = true
	}

	mergedText := make(map[string]map[string]bool, len(result.Merges))
	for _, m := range result.Merges {
		if m.Text == "" || !present[m.Text] {
			continue
		}
		tokens := tokenize(m.Text)
		for _, id := range m.IDs {
			mergedText[memIDKey(id)] = tokens
		}
	}

	var drops []DroppedGuarded
	for _, in := range input.ExistingMemories {
		key := memIDKey(in.ID)
		inTokens := tokenize(in.Content)
		if len(inTokens) == 0 {
			continue
		}
		if mergeTokens, ok := mergedText[key]; ok {
			if hasCloseSurvivor(inTokens, mergeTokens) {
				continue
			}
		} else if hasCloseSurvivorInAny(inTokens, outTokens) {
			continue
		}
		drops = append(drops, DroppedGuarded{
			ID:         in.ID,
			Category:   in.Category,
			Content:    in.Content,
			Importance: in.Importance,
			Tags:       in.Tags,
		})
	}
	return drops
}

func hasCloseSurvivor(inTokens map[string]bool, outTokens map[string]bool) bool {
	if len(outTokens) == 0 {
		return false
	}
	found := 0
	for tok := range inTokens {
		if outTokens[tok] {
			found++
		}
	}
	return float64(found)/float64(len(inTokens)) >= dropContainmentThreshold
}

func hasCloseSurvivorInAny(inTokens map[string]bool, outTokens []map[string]bool) bool {
	for _, out := range outTokens {
		if len(out) == 0 {
			continue
		}
		if hasCloseSurvivor(inTokens, out) {
			return true
		}
	}
	return false
}

func memIDKey(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}

// RetainGuardedDrops turns every flagged drop back into a result memory so it
// can be carried forward verbatim. Refusing to apply (the pre-#458
func RetainGuardedDrops(drops []DroppedGuarded) []ReflectMemory {
	retained := make([]ReflectMemory, 0, len(drops))
	for _, d := range drops {
		retained = append(retained, ReflectMemory{
			Category:   d.Category,
			Content:    d.Content,
			Importance: d.Importance,
			Tags:       d.Tags,
			RestatesID: d.ID,
		})
	}
	return retained
}
