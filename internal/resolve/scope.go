// Scoping a repair pass to the rows it is about (#698).
//
// The repair pass over the WHOLE project is the wrong tool for the repair it is
// usually used for. `ghost supersede --reassess --apply` withdraws the wrong
// 'supersedes' edges, and the resolution each of those edges caused can only be
// cleared by `ghost resolve --reassess` — which re-judges every already-resolved
// memory in the project. On a real store that unscoped pass proposed un-hiding
// 143 memories, and an independent judge sampling 40 found about 35% of them
// stale: completed changelogs, PR and host status snapshots, notes a newer
// memory in the same project had already superseded, a description of a retired
// code path. The repair that was supposed to undo a few wrong resolutions
// proposed undoing dozens of right ones.
//
// So the repair pass can be pointed at the rows a repair is about. Scope is that
// pointer, and it is deliberately the narrowest possible thing: it narrows WHICH
// rows are judged, and nothing else. Every floor still applies to a scoped row —
// a live 'supersedes' edge, a correction pairing, the KEEP cache — because those
// answers do not change when the question gets smaller, and a scoped pass that
// quietly skipped them would clear a row the very next ordinary pass re-stamps.
//
// Selectors are a full id or an 8+ character hex prefix, which is what an
// operator can read off a report (the CLI prints the first eight characters) and
// paste back. The two ways a selector can fail are not the same kind of failure
// and are handled differently:
//
//   - it matches nothing. That is a mistyped id, a row in another project, or a
//     row an earlier repair already cleared. It is reported and skipped, and the
//     rest of the scope is still judged: refusing the run would cost the
//     operator the repairs that DO name rows that exist.
//   - it matches more than one row. Nothing can be said about which rows were
//     meant, and picking one — or judging both — is the exact un-hiding this
//     type exists to prevent, so it is an error and the run stops.
package resolve

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// minSelectorPrefix is the shortest selector that may be treated as a prefix.
// Eight hex characters is 32 bits of a 128-bit id, which no realistic project
// collides on, and it is the width the CLI's own reports abbreviate to — so a
// prefix the tool printed is always a prefix it will accept. Below the floor a
// selector is a usage error rather than a miss: nobody means one hex character,
// and reporting it in a list of skips would bury the mistake.
const minSelectorPrefix = 8

// hexDigits is the alphabet memory.Store's id column produces
// (hex(randomblob(16))). A selector outside it names no row under any reading,
// so it is rejected up front rather than reported as a miss.
const hexDigits = "0123456789abcdefABCDEF"

// Scope narrows a repair pass to the memories a repair is about. The zero value
// is the unscoped pass over the whole project, which is what every caller
// outside a repair writes.
type Scope struct {
	// Only holds the selectors in the order the operator gave them. Select
	// returns rows in the pool's own order, not this one, so a report reads the
	// same way whether it was scoped or not.
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
//
// A selector is matched case-insensitively: ids are lowercased hex from SQLite,
// and an operator pasting from a terminal has no reason to know that.
func (s Scope) Select(pool []memory.Memory) (scoped []memory.Memory, misses []ScopeMiss, err error) {
	if !s.Scoped() {
		return pool, nil, nil
	}
	wanted := make(map[string]bool, len(s.Only))
	// selected records the rows the scope named, so the pool is walked once at
	// the end and the answer comes back in the store's order however the
	// selectors were written.
	selected := make(map[string]bool, len(s.Only))
	for _, spec := range s.Only {
		key := strings.TrimSpace(spec)
		if key == "" {
			return nil, nil, emptySelectorError
		}
		if wanted[key] {
			// A repeated selector names one row, so it is judged once. Silently,
			// because the operator's intent is unambiguous either way.
			continue
		}
		wanted[key] = true
		// A FULL id, whatever its shape, before any rule about prefixes. The
		// alphabet check below is about what a PREFIX may be built from, and
		// applying it first refused exactly the rows this repair exists to name:
		// `ghost import` writes an artifact's ids verbatim and ImportMemory
		// refuses only an empty one, so a stored id can hold a space, a `;` or a
		// leading dash — and a supersede withdrawal that orphans such a target
		// prints `ghost resolve --only <that id>`, which this pass then refused.
		// internal/supersede.resolveRef already resolves a ref on this order, and
		// two id-resolution paths that disagree about the same id are the defect,
		// not the feature.
		if id, ok := exactInPool(pool, key); ok {
			selected[id] = true
			continue
		}
		// Past the full-id reading it IS a prefix, and a prefix is a prefix of a
		// hex id: the two shape rules are about the prefix, and both are checked
		// before a single candidate is gathered, so a malformed selector can never
		// reach the ambiguity listing.
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

// noSuchRow is the one miss reason, and it names only what is knowable from the
// pool resolve was given: the row is not in it. Whether it was never resolved,
// resolved and since cleared, or belongs to another project is not something
// this pass can see, and guessing between them would be a lie in a report line.
const noSuchRow = "no already-resolved memory in this project has that id or prefix (it may be unresolved, cleared by an earlier repair, or in another project)"

// emptySelectorError is one message for both empty spellings ("" and whitespace),
// because there is nothing to distinguish them for the reader: either way no
// memory was named.
var emptySelectorError = fmt.Errorf("--only selector is empty: give a memory id or an 8+ character hex prefix")

// exactInPool finds the row a selector names in full, comparing the stored id
// rather than the selector's spelling: a byte-exact id first, then a single
// case-folded one, so an uppercase paste of a lowercase hex id still resolves
// while two ids differing only in case are left to the prefix rules (where the
// operator is told about the ambiguity rather than handed one of the two).
//
// The first rule, "at most one row can be byte-equal to this", is what makes a
// full id safe to accept without the shape check: an id is a unique key, so an
// exact match is not a guess.
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

// prefixKey normalises one selector to the form a PREFIX is matched against,
// after checking it could name a row at all. It runs only after exactInPool has
// declined to read the selector as a full id, so both rules below are rules about
// a prefix and neither is a statement about what an id may contain.
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

// abbreviate renders at most three ids for an ambiguity error, each shortened
// the way every Ghost report shortens an id, so the operator can see what
// collided without reading a list of thirty-two-character strings.
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
