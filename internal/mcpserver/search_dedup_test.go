package mcpserver

import (
	"context"
	"regexp"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSearchCollapsesNearDuplicatesAtALimitOfTwo is the issue's acceptance
// case, asserted over the live tool: a limit of 2 over a near-duplicate pair
// plus one distinct row must return the representative and the distinct row.
// Before collapse the window is simply the top-2 rows — the pair — and the
// distinct row, which is what the caller asked for, never appears.
func TestSearchCollapsesNearDuplicatesAtALimitOfTwo(t *testing.T) {
	srv, session := newCapSession(t)
	store, ok := srv.store.(*memory.Store)
	if !ok {
		t.Fatalf("test server store = %T, want *memory.Store", srv.store)
	}

	// The pair outranks the distinct row on the keyword leg (bm25 rewards the
	// shorter, denser pair), so at limit 2 the uncollapsed window would be the
	// pair. The pair's wordings stay below the save-time 0.5 Jaccard bar so
	// the single CreateLink edge below is the only verdict the window sees.
	dupA := saveMem(t, session, "the cache warmer runs on the read replica every hour", nil)
	dupB := saveMem(t, session, "the cache warmer restocks the secondary database nightly", nil)
	distinct := saveMem(t, session,
		"unrelated to the pair beyond the shared noun: the cache warmer is one "+
			"scheduled job among many in this deployment, and the rest of this entry "+
			"exists to lengthen the row so the keyword ranking puts the two short "+
			"wordings above it in bm25, which is what makes the limit-of-two case "+
			"actually exercise the collapse rather than passing by accident", nil)
	if err := store.CreateLink(context.Background(), dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(related): %v", err)
	}

	ids := listingIDs(t, session, "cache warmer", map[string]any{"limit": 2})
	if len(ids) != 2 {
		t.Fatalf("limit-2 search returned %d rows (%v), want the representative and the distinct row", len(ids), ids)
	}
	if !ids[distinct] {
		t.Errorf("limit-2 search omitted the distinct row; got %v, one of the pair and %s", ids, distinct)
	}
	if ids[dupA] == ids[dupB] {
		t.Errorf("limit-2 search returned %v, want exactly one of the near-duplicate pair", ids)
	}
}

// idRe matches a memory id in either case: the rendered listing shows the
// stored uppercase form, so the shared lowercase-only normalizer leaves those
// runs untouched.
var idRe = regexp.MustCompile("[0-9A-Fa-f]{32}")

// normalizeAllIDs replaces every memory id with a stable placeholder in order
// of first appearance so the whole answer, ids and all, can be pinned.
func normalizeAllIDs(s string) string {
	seen := map[string]string{}
	return idRe.ReplaceAllStringFunc(s, func(id string) string {
		if p, ok := seen[id]; ok {
			return p
		}
		p := "<id" + string(rune('A'+len(seen))) + ">"
		seen[id] = p
		return p
	})
}

// TestSearchWithoutNearDuplicatesIsByteStable pins the answer of a search that
// has nothing to collapse, whole. Collapse runs inside the same retrieval the
// plain answer renders, so this is the guard that a no-duplicate search did
// not move a byte: same rows, same order, same verdict line.
func TestSearchWithoutNearDuplicatesIsByteStable(t *testing.T) {
	// The saved rows carry the detected writer label, so the golden would
	// otherwise follow whatever harness happens to run `go test`.
	pinAgent(t, "claude-code")
	_, session := newCapSession(t)
	saveMem(t, session, "the billing service exposes a grpc endpoint on port 8080", nil)
	saveMem(t, session, "the metrics service scrapes prometheus every fifteen seconds", nil)
	saveMem(t, session, "the router service reloads its config when the file changes", nil)

	got := normalizeAllIDs(searchScopedWithLimit(t, session, "service", nil, 3))
	const want = "- [fact] `<idA>` (0.7 agent=«claude-code» source=mcp) «the metrics service scrapes prometheus every fifteen seconds»\n- [fact] `<idB>` (0.7 agent=«claude-code» source=mcp) «the billing service exposes a grpc endpoint on port 8080»\n- [fact] `<idC>` (0.7 agent=«claude-code» source=mcp) «the router service reloads its config when the file changes»\n\n[ghost:outcome=answerable reason=floor_met floor_fts_rank=3 abstain_cosine=off candidates=3 admitted=3 legs=fts:ok,vector:not_run tokens_est=44]\n"
	if got != want {
		t.Errorf("a search with nothing to collapse changed:\n got: %q\nwant: %q", got, want)
	}
}
