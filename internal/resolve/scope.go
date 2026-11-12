// Scoping a repair pass to the rows it is about (#698).
package resolve

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const minSelectorPrefix = 8

const hexDigits = "0123456789abcdefABCDEF"

// Scope narrows a repair pass to the memories a repair is about. The zero value
// is the unscoped pass over the whole project, which is what every caller
// outside a repair writes.
type Scope struct {
	Only []string
}

// Scoped reports whether the scope names anything. Callers use it to choose
// between the unscoped and scoped report wording, so an unscoped run never
// prints a scope it did not ask for.
func (s Scope) Scoped() bool { return len(s.Only) > 0 }

// ScopeMiss is one selector that named no row the pass could judge. It is a
// report line, never an error: Spec is the selector the operator typed, verbatim,
// so they can see which one of the ids they pasted was wrong.
type ScopeMiss struct {
	Spec   string
	Reason string
}

// Select narrows pool to the scope, returning the selected rows in pool order,
// the selectors that matched nothing, and an error for a selector that is
// malformed or ambiguous. An empty scope returns the pool untouched.
func (s Scope) Select(pool []memory.Memory) (scoped []memory.Memory, misses []ScopeMiss, err error) {
	if !s.Scoped() {
		return pool, nil, nil
	}
	wanted := make(map[string]bool, len(s.Only))
	selected := make(map[string]bool, len(s.Only))
	for _, spec := range s.Only {
		key := strings.TrimSpace(spec)
		if key == "" {
			return nil, nil, errEmptySelector
		}
		if wanted[key] {
			continue
		}
		wanted[key] = true
		if id, ok := exactInPool(pool, key); ok {
			selected[id] = true
			continue
		}
		prefix, err := prefixKey(spec)
		if err != nil {
			return nil, nil, err
		}
		var hits []string
		for _, m := range pool {
			if strings.HasPrefix(strings.ToLower(m.ID), prefix) {
				hits = append(hits, m.ID)
			}
		}
		switch len(hits) {
		case 0:
			misses = append(misses, ScopeMiss{Spec: spec, Reason: noSuchRow})
		case 1:
			selected[hits[0]] = true
		default:
			return nil, nil, fmt.Errorf("--only prefix %q is ambiguous: it matches %d memories in this project's resolved pool (%s); pass more characters",
				spec, len(hits), abbreviate(hits))
		}
	}
	for _, m := range pool {
		if selected[m.ID] {
			scoped = append(scoped, m)
		}
	}
	return scoped, misses, nil
}

const noSuchRow = "no already-resolved memory in this project has that id or prefix (it may be unresolved, cleared by an earlier repair, or in another project)"

var errEmptySelector = fmt.Errorf("--only selector is empty: give a memory id or an 8+ character hex prefix")

func exactInPool(pool []memory.Memory, spec string) (string, bool) {
	for _, m := range pool {
		if m.ID == spec {
			return m.ID, true
		}
	}
	var folded string
	for _, m := range pool {
		if strings.EqualFold(m.ID, spec) {
			if folded != "" {
				return "", false
			}
			folded = m.ID
		}
	}
	return folded, folded != ""
}

func prefixKey(spec string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(spec))
	if strings.TrimFunc(key, func(r rune) bool { return strings.ContainsRune(hexDigits, r) }) != "" {
		return "", fmt.Errorf("--only prefix %q is not hex: a full memory id is accepted as given, and a prefix must be built from the hex digits ids are made of", spec)
	}
	if len(key) < minSelectorPrefix {
		return "", fmt.Errorf("--only prefix %q is too short to be a prefix: give the full id or at least %d hex characters", spec, minSelectorPrefix)
	}
	return key, nil
}

func abbreviate(ids []string) string {
	const show = 3
	parts := make([]string, 0, show+1)
	for i, id := range ids {
		if i == show {
			parts = append(parts, fmt.Sprintf("and %d more", len(ids)-show))
			break
		}
		if len(id) > minSelectorPrefix {
			id = id[:minSelectorPrefix]
		}
		parts = append(parts, id)
	}
	return strings.Join(parts, ", ")
}
