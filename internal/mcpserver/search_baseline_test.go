package mcpserver

import (
	"regexp"
	"strings"
	"testing"
)

var baselineIDRe = regexp.MustCompile("[0-9a-f]{32}")

// normalizeIDs replaces every memory id with a stable placeholder in order of
// first appearance, so a golden can pin the whole response, ids and all, even
// though the ids are random per run.
func normalizeIDs(s string) string {
	seen := map[string]string{}
	return baselineIDRe.ReplaceAllStringFunc(s, func(id string) string {
		if p, ok := seen[id]; ok {
			return p
		}
		p := "<id" + string(rune('A'+len(seen))) + ">"
		seen[id] = p
		return p
	})
}

// TestScopedSearchAnswerIsByteStable pins the formatted answer of a
// scope-filtered search: dev and production rows, limit 2, scope=production.
// It was written against the code BEFORE explain became a projection of the
// assembler's trace, and it must keep passing unedited: the explain work is
// not allowed to move a byte of the plain answer, its verdict line, or the
// advice a scope-emptied answer carries.
//
// The rows are worded to be lexically distinct from one another. The save path
// links a restatement as a `duplicate` edge at save time and #926 collapses
// such a pair in query mode, so two production rows that read like each other
// would come back as one and this golden would pin a two-row answer it no
// longer gets. This test owns the answer's bytes, not the dedup, so its
// fixtures hold claims the dedup leaves alone.
func TestScopedSearchAnswerIsByteStable(t *testing.T) {
	_, session := newCapSession(t)
	saveScoped(t, session, "database pool size is 10 in development", "development")
	saveScoped(t, session, "connection pool ceiling for the reporting database sits at fifty rows", "production")
	saveScoped(t, session, "database failover runs in production", "production")
	saveScoped(t, session, "nightly job populates the warehouse with sample rows", "development")

	got := normalizeIDs(searchScopedWithLimit(t, session, "database", map[string]any{"environment": "production"}, 2))

	if strings.Contains(got, "development") {
		t.Fatalf("a scope=production answer lists a development row:\n%s", got)
	}
	// The listing order and the exact verdict line, pinned whole.
	if n := strings.Count(got, "- ["); n != 2 {
		t.Fatalf("scope-filtered answer lists %d rows, want 2:\n%s", n, got)
	}
	machine := got[strings.LastIndex(got, "[ghost:outcome="):]
	const wantMachine = "[ghost:outcome=answerable reason=floor_met floor_fts_rank=3 abstain_cosine=off candidates=2 admitted=2 legs=fts:ok,vector:not_run tokens_est=27]\n"
	if machine != wantMachine {
		t.Errorf("verdict line changed:\n got: %q\nwant: %q", machine, wantMachine)
	}
}

// TestScopeEmptiedAnswerIsByteStable pins the other half: an answer emptied by
// its scope filter. (The "drop the scope filter" advice needs a hybrid store; it
// is pinned in internal/assemble and by the e2e suite.)
func TestScopeEmptiedAnswerIsByteStable(t *testing.T) {
	_, session := newCapSession(t)
	saveScoped(t, session, "database pool size is 10 in development", "development")

	got := normalizeIDs(searchScopedWithLimit(t, session, "database", map[string]any{"environment": "production"}, 2))
	const want = "This search is incomplete: the vector leg could not run, so only the keyword leg ran and it may be less complete than a hybrid one. The query was not wrong \u2014 the answer is incomplete, not absent.\n\n(Note: the scope filter was applied to a finite search window, so further matches may exist beyond the retrieved candidates \u2014 raise the limit.)\n[ghost:outcome=empty reason=vector_backend_unavailable floor_fts_rank=not_applied abstain_cosine=off candidates=0 admitted=0 legs=fts:ok,vector:not_run tokens_est=0]\n"
	if got != want {
		t.Errorf("scope-emptied answer changed:\n got: %q\nwant: %q", got, want)
	}
}
