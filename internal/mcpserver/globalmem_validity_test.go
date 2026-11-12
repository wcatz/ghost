package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// assembler (#581), each pinned by its own fixture rather than by the golden beside

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
	if !strings.Contains(out, "unverified") {
		t.Errorf("a live global row nobody re-verified lost its [unverified] marker, so the state is no longer reported:\n%s", out)
	}
	for _, absent := range []string{expired, future} {
		if strings.Contains(out, absent) {
			t.Errorf("%s reached the listing; a row whose validity window has closed, or has not opened, is not a current claim:\n%s", absent, out)
		}
	}
	for _, marker := range []string{"expired", "not yet valid"} {
		if strings.Contains(out, marker) {
			t.Errorf("the listing still carries the %q verdict for a row stage 2 dropped, so the row reached the renderer:\n%s", marker, out)
		}
	}
}

func TestTheGlobalMemoriesResourceSaysRowsWereExcludedRatherThanThatNothingWasSaved(t *testing.T) {
	srv, session := newValiditySession(t)
	expired := saveGlobalValidityRow(t, session, "global: the one cross-project row, long retired",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})

	withheld := renderGlobalMemoriesResource(t, srv)
	if strings.Contains(withheld, "No global memories saved yet") {
		t.Errorf("the census claimed nothing had ever been saved, but Ghost holds %s and withheld it as out of date:\n%s", expired, withheld)
	}
	for _, want := range []string{"ghost_memories_list", "out of date"} {
		if !strings.Contains(withheld, want) {
			t.Errorf("the withheld answer does not say %q, so a caller is told what happened with nothing to do about it:\n%s", want, withheld)
		}
	}

	bare, _ := newValiditySession(t)
	if census := renderGlobalMemoriesResource(t, bare); !strings.Contains(census, "No global memories saved yet") {
		t.Errorf("a store holding no global rows lost the census, so an agent that has saved nothing is told nothing to do:\n%s", census)
	}
}
