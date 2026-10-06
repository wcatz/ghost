package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The two behaviour changes `ghost://memories/global` gains by moving onto the
// assembler (#581), each pinned by its own fixture rather than by the golden beside
// them in globalmem_golden_test.go.
//
// They are here for the reason TestTheProjectContextDropsRowsWhoseWindowHasClosed
// and TestTheProjectContextEmptyBlockSaysRowsWereExcludedRatherThanThatNothingWasSaved
// are there for the project-context surface, and that file states it: the goldens
// seed no validity column, so a store whose rows carry a closed window renders
// identically before and after the migration. Re-recording the baseline to
// accommodate it would turn the golden from a parity proof into a diff record,
// which is the one thing it exists to be.
//
// The store is the validity suite's own, reached through `newValiditySession`, so
// these fixtures are the ones the project-context validity tests already agreed on
// rather than a second fixture shape with the same rows in it.

// saveGlobalValidityRow saves one GLOBAL memory through the real ghost_save_global
// tool with the given extra arguments.
//
// The tool rather than a direct INSERT, for the reason saveValidityRow gives: the
// write side is the other half of this contract, and a fixture that wrote
// `valid_until` in SQL would be testing a row the callers of this surface cannot
// produce. A global row is exactly the case that matters — it is the row the
// injected session-start block offers to every project, so a retired one that
// survives here is a lie told in every session rather than in one.
func saveGlobalValidityRow(t *testing.T, session *mcp.ClientSession, content string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"content": content, "category": "fact"}
	for k, v := range extra {
		args[k] = v
	}
	res := callTool(t, session, "ghost_save_global", args)
	if res.IsError {
		t.Fatalf("save global %q: %s", content, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("save global %q carried no memory id: %q", content, resultText(res))
	}
	return id
}

// TestTheGlobalMemoriesResourceWithholdsARowWhoseWindowHasClosed is the ONE
// selection change this migration makes on this surface.
//
// The reader it replaces was `GetTopMemories`, which filtered `resolved_at IS NULL`
// and nothing else, and whose renderer MARKED a closed window `expired` rather than
// dropping it — so a memory the store itself says has retired was listed here as a
// current cross-project fact. `assemble.Run` runs stage 2 unconditionally and there
// is no knob to decline it, which is why this cannot be a configuration choice and
// is only ever a change of reader.
//
// All five verdicts are asserted, not only the two that change. A test that pins
// `expired` and `future` and says nothing about the other three passes against an
// implementation that drops the wrong rows — the `unverified` one in particular,
// which is KEPT, because `verified_at` is a flag and not a predicate.
func TestTheGlobalMemoriesResourceWithholdsARowWhoseWindowHasClosed(t *testing.T) {
	srv, session := newValiditySession(t)

	live := saveGlobalValidityRow(t, session, "global: a durable cross-project preference", nil)
	openWindow := saveGlobalValidityRow(t, session, "global: a policy with an open window",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31", "verified": true})
	unverified := saveGlobalValidityRow(t, session, "global: a claim with a window nobody checked",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2999-12-31"})
	expired := saveGlobalValidityRow(t, session, "global: a migration that has been rolled back",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})
	future := saveGlobalValidityRow(t, session, "global: a policy that starts next year",
		map[string]any{"valid_from": "2999-01-01", "valid_until": "2999-12-31"})

	out := renderGlobalMemoriesResource(t, srv)

	for _, present := range []string{live, openWindow, unverified} {
		if !strings.Contains(out, present) {
			t.Errorf("%s is missing from the listing; a row inside its window must survive:\n%s", present, out)
		}
	}
	// The unverified row is KEPT and MARKED. Dropping it would be the second
	// plausible-looking implementation, and it is wrong: nobody re-checked the
	// claim, which is not the same as the claim being wrong.
	if !strings.Contains(out, "unverified") {
		t.Errorf("a live global row nobody re-verified lost its [unverified] marker, so the state is no longer reported:\n%s", out)
	}
	for _, absent := range []string{expired, future} {
		if strings.Contains(out, absent) {
			t.Errorf("%s reached the listing; a row whose validity window has closed, or has not opened, is not a current claim:\n%s", absent, out)
		}
	}
	// And the retired rows are not merely absent: a listing that rendered them with
	// a marker would satisfy the id check above, so the verdict wording is checked
	// too. Nothing in the answer may still describe a row stage 2 dropped.
	for _, marker := range []string{"expired", "not yet valid"} {
		if strings.Contains(out, marker) {
			t.Errorf("the listing still carries the %q verdict for a row stage 2 dropped, so the row reached the renderer:\n%s", marker, out)
		}
	}
}

// TestTheGlobalMemoriesResourceShowsCensusWhenAllRowsExpired verifies that
// when all global rows are expired, the SQL validity filter removes them before
// they reach the assembler, so the assembler sees an empty window and the census
// appears. This is the new behavior with the passive SQL validity filter: expired
// rows are filtered in SQL and never reach the assembler, so the assembler cannot
// produce a "withheld as out of date" verdict for them.
func TestTheGlobalMemoriesResourceShowsCensusWhenAllRowsExpired(t *testing.T) {
	// A store whose only global row has retired: the census appears because the
	// assembler sees an empty window (expired rows filtered in SQL).
	srv, session := newValiditySession(t)
	saveGlobalValidityRow(t, session, "global: the one cross-project row, long retired",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})

	census := renderGlobalMemoriesResource(t, srv)
	if !strings.Contains(census, "No global memories saved yet") {
		t.Errorf("the census should appear when all global rows are expired (filtered in SQL): %s", census)
	}

	// The other direction: a store with no global rows at all still gets the census.
	bare, _ := newValiditySession(t)
	if census := renderGlobalMemoriesResource(t, bare); !strings.Contains(census, "No global memories saved yet") {
		t.Errorf("a store holding no global rows lost the census: %s", census)
	}
}
