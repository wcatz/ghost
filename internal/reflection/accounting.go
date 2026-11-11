package reflection

import (
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// AccountMerge is one merge line: the ids it folded in, the text it produced,
// whether that text is one of the result's memories, and how many of its sources
type AccountMerge struct {
	IDs         []string
	Text        string
	In          bool
	SourceBytes int
	Guarded     int
}

// AccountScopeRefusal is one merge the apply will refuse because its sources'
// scopes disagree: the ids it named, and each one's stored scope in the same
type AccountScopeRefusal struct {
	IDs    []string
	Scopes []map[string]string
}

// AccountLinkOverflow is one merge the apply will refuse because the decision
// links its sources carry, joined, do not fit in one source_ref: the ids it
type AccountLinkOverflow struct {
	IDs      []string
	Carriers []string
	Links    int
	Bound    int
}

// AccountRewrite is one rewrite line: the id it replaced, the text it wrote
// instead, and whether that text is one of the result's memories.
type AccountRewrite struct {
	ID      string
	Text    string
	In      bool
	Guarded bool
}

// AccountDrop is one drop line: the id the response disposed of, the reason it
// gave, the id it named as the replacement, and whether the drop guard found
type AccountDrop struct {
	ID        string
	Reason    string
	Successor string
	Guarded   bool
}

const (
	deletedNoCarrier = "no surviving output carries this row"
	deletedDuplicate = "an identical row is reused in its place"
)

// AccountDeleted is one input row an apply removes, and why.
type AccountDeleted struct {
	ID     string
	Reason string
}

// InputAccounting is every input id of one consolidation, in the buckets the
// report prints. The fields that name ids and the two counts partition the
type InputAccounting struct {
	Inputs        int
	Merges        []AccountMerge
	Refusals      []Refusal
	LinkOverflows []AccountLinkOverflow
	ScopeRefusals []AccountScopeRefusal
	Rewrites      []AccountRewrite
	Drops         []AccountDrop
	Kept          []string
	Passed        []string
	Deleted       []AccountDeleted
}

type claimCandidate struct {
	id       string
	category string
	created  string
}

type emission struct {
	content  string
	category string
}

func claimedRows(input []memory.Memory, emitted []emission) map[string]bool {
	reusable := make(map[string][]claimCandidate, len(input))
	for _, m := range input {
		reusable[m.Content] = append(reusable[m.Content], claimCandidate{
			id: m.ID, category: m.Category, created: m.CreatedAt,
		})
	}
	for content, bucket := range reusable {
		sorted := append([]claimCandidate(nil), bucket...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].created != sorted[j].created {
				return sorted[i].created < sorted[j].created
			}
			return sorted[i].id < sorted[j].id
		})
		reusable[content] = sorted
	}
	claimed := make(map[string]bool, len(input))
	for _, e := range emitted {
		bucket := reusable[e.content]
		if len(bucket) == 0 {
			continue
		}
		pick := 0
		for i, c := range bucket {
			if c.category == e.category {
				pick = i
				break
			}
		}
		claimed[memIDKey(bucket[pick].id)] = true
		reusable[e.content] = append(bucket[:pick:pick], bucket[pick+1:]...)
	}
	return claimed
}

// AccountInputs partitions the result's input ids into the buckets a report
// prints. It is pure: the same input and result always account the same way, and
func AccountInputs(input ReflectionInput, result ReflectionResult, guarded []DroppedGuarded) InputAccounting {
	present := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		present[m.Content] = true
	}
	flagged := make(map[string]bool, len(guarded))
	for _, g := range guarded {
		flagged[memIDKey(g.ID)] = true
	}

	byID := make(map[string]memory.Memory, len(input.ExistingMemories))
	order := make([]string, 0, len(input.ExistingMemories))
	for _, in := range input.ExistingMemories {
		key := memIDKey(in.ID)
		if _, dup := byID[key]; dup {
			continue
		}
		byID[key] = in
		order = append(order, key)
	}
	storedID := func(id string) string {
		if m, ok := byID[memIDKey(id)]; ok {
			return m.ID
		}
		return id
	}
	storedIDs := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, storedID(id))
		}
		return out
	}

	acc := InputAccounting{Inputs: len(order)}
	for _, r := range result.Refusals {
		r.IDs = storedIDs(r.IDs)
		acc.Refusals = append(acc.Refusals, r)
	}
	rows := make([]memory.Memory, 0, len(order))
	for _, key := range order {
		rows = append(rows, byID[key])
	}
	overflowLinks := make(map[int]int)
	overflowClaims := make(map[int][]string)
	scopeRefused := make(map[int]bool)
	refusedText := make(map[string]int)
	for i, m := range result.Merges {
		if !present[m.Text] {
			continue
		}
		named := make(map[string]bool, len(m.IDs))
		for _, id := range m.IDs {
			named[storedID(id)] = true
		}
		var refs, claims []string
		for _, key := range order {
			if row := byID[key]; memory.EmissionClaimsRow(named, m.Text, row.ID, row.Content) {
				refs = append(refs, row.SourceRef)
				claims = append(claims, key)
			}
		}
		if links, over := memory.DecisionLinksOverflow(refs); over {
			overflowLinks[i] = links
			overflowClaims[i] = claims
			refusedText[m.Text]++
			continue
		}
		sources := make([]memory.SourceScope, 0, len(m.IDs))
		for _, id := range m.IDs {
			if src, ok := byID[memIDKey(id)]; ok {
				sources = append(sources, memory.SourceScope{ID: src.ID, Scope: src.Scope})
			}
		}
		if _, err := memory.FoldSourceScopes(sources); memory.IsScopeConflict(err) {
			scopeRefused[i] = true
			refusedText[m.Text]++
		}
	}
	emitted := make([]emission, 0, len(result.Memories))
	for _, m := range result.Memories {
		if refusedText[m.Content] > 0 {
			refusedText[m.Content]--
			continue
		}
		emitted = append(emitted, emission{content: m.Content, category: m.Category})
	}
	claimed := claimedRows(rows, emitted)

	dropOf := make(map[string]AccountDrop, len(result.Drops))
	for _, d := range result.Drops {
		dropOf[memIDKey(d.ID)] = AccountDrop{
			ID:        storedID(d.ID),
			Reason:    d.Reason,
			Successor: storedID(d.Successor),
			Guarded:   flagged[memIDKey(d.ID)],
		}
	}
	rewriteOf := make(map[string]AccountRewrite, len(result.Replacements))
	for _, r := range result.Replacements {
		key := memIDKey(r.ID)
		if _, claimed := dropOf[key]; claimed {
			continue
		}
		rewriteOf[key] = AccountRewrite{ID: storedID(r.ID), Text: r.Text, In: present[r.Text], Guarded: flagged[key]}
	}
	refused := make(map[string]bool)
	for _, r := range result.Refusals {
		for _, id := range r.IDs {
			refused[memIDKey(id)] = true
		}
	}
	kept := make(map[string]bool, len(result.Kept))
	for _, id := range result.Kept {
		kept[memIDKey(id)] = true
	}
	mergeOf := make(map[string]int, len(result.Merges))
	scopeOwned := make(map[string]bool)
	overflowOwned := make(map[string]bool)
	overflowLine := make(map[int]int)
	for i, m := range result.Merges {
		if links, ok := overflowLinks[i]; ok {
			overflowLine[i] = len(acc.LinkOverflows)
			acc.LinkOverflows = append(acc.LinkOverflows, AccountLinkOverflow{
				IDs: storedIDs(m.IDs), Links: links, Bound: memory.MaxSourceRefLen,
			})
			for _, id := range m.IDs {
				overflowOwned[memIDKey(id)] = true
			}
			continue
		}
		if scopeRefused[i] {
			line := AccountScopeRefusal{IDs: storedIDs(m.IDs)}
			for _, id := range m.IDs {
				line.Scopes = append(line.Scopes, byID[memIDKey(id)].Scope)
				scopeOwned[memIDKey(id)] = true
			}
			acc.ScopeRefusals = append(acc.ScopeRefusals, line)
			continue
		}
		line := AccountMerge{IDs: storedIDs(m.IDs), Text: m.Text, In: present[m.Text]}
		for _, id := range m.IDs {
			if src, ok := byID[memIDKey(id)]; ok {
				line.SourceBytes += len(src.Content)
			}
			mergeOf[memIDKey(id)] = len(acc.Merges)
		}
		acc.Merges = append(acc.Merges, line)
	}
	mergeSource := make(map[string]bool)
	for _, m := range result.Merges {
		for _, id := range m.IDs {
			mergeSource[memIDKey(id)] = true
		}
	}
	for i := range result.Merges {
		idx, ok := overflowLine[i]
		if !ok {
			continue
		}
		for _, key := range overflowClaims[i] {
			if mergeSource[key] || refused[key] || overflowOwned[key] {
				continue
			}
			overflowOwned[key] = true
			acc.LinkOverflows[idx].Carriers = append(acc.LinkOverflows[idx].Carriers, byID[key].ID)
		}
	}

	for _, key := range order {
		if idx, ok := mergeOf[key]; ok {
			if flagged[key] {
				acc.Merges[idx].Guarded++
			}
			continue
		}
		if overflowOwned[key] {
			continue
		}
		if rw, ok := rewriteOf[key]; ok {
			acc.Rewrites = append(acc.Rewrites, rw)
			continue
		}
		if d, ok := dropOf[key]; ok {
			acc.Drops = append(acc.Drops, d)
			continue
		}
		if scopeOwned[key] {
			continue
		}
		if refused[key] {
			continue
		}
		row := byID[key]
		if claimed[key] {
			if kept[key] {
				acc.Kept = append(acc.Kept, row.ID)
			} else {
				acc.Passed = append(acc.Passed, row.ID)
			}
			continue
		}
		reason := deletedNoCarrier
		if present[row.Content] {
			reason = deletedDuplicate
		}
		acc.Deleted = append(acc.Deleted, AccountDeleted{ID: row.ID, Reason: reason})
	}
	return acc
}
