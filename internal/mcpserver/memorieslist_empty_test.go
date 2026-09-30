package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestTheMemoriesListNamesTheProjectItWasAskedAbout is the RED test for the
// `Project ""` defect, and both halves of it are real.
//
// `ResolveProject` answers a name no `projects` row matches with `("", "", nil)`,
// and this handler assigned that over `args.ProjectID` and then FORMATTED it. So
// `ghost_memories_list(project_id: "nope")` on a store with no matching rows
// answered `Project "" is not registered with Ghost yet` — a message that names
// nothing the caller can act on, and which reads as though Ghost held a project
// with an empty name.
//
// The second half is the worse one. The read is never guarded, so it is handed
// that empty id, and `ListMemories` widens its scope to
// `(project_id = ? OR project_id = '_global')` whenever a category or retention
// filter is present. So a FILTERED browse of a project Ghost has never heard of
// returned the `_global` rows — and then the not-registered sentence was
// unreachable, because the answer was never empty. An agent browsing "all the
// gotchas" in a project it misspelled was shown every project's gotchas.
func TestTheMemoriesListNamesTheProjectItWasAskedAbout(t *testing.T) {
	st := newMemoriesListStore(t)
	// A global row of the category the filtered browse below asks for, so the
	// widening is observable: without the guard the answer IS this row, and
	// "the sentence appeared" would be vacuously true on a store with no globals.
	if _, err := st.CreateWithIDFromCorpus(context.Background(), memory.GlobalProjectID, "leakyglobal", memory.Memory{
		Category: "gotcha", Content: "a global gotcha that must not leak into another project's browse",
		Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	_, session := validityServerFor(t, st)
	const wanted = "no-such-project-here"

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"a filtered browse", map[string]any{"project_id": wanted, "category": "gotcha"}},
		{"an unfiltered browse", map[string]any{"project_id": wanted}},
		{"a retention-filtered browse", map[string]any{"project_id": wanted, "retention": "project"}},
	} {
		out := resultText(callTool(t, session, "ghost_memories_list", c.args))
		if strings.Contains(out, "a global gotcha that must not leak") {
			t.Errorf("%s of an unknown project returned the _global rows; ListMemories widens to "+
				"`project_id = ? OR project_id = '_global'` when a filter is set:\n%s", c.name, out)
		}
		if strings.Contains(out, `Project ""`) {
			t.Errorf("%s of an unknown project quoted the RESOLVED id, which is empty:\n%s", c.name, out)
		}
		if !strings.Contains(out, wanted) {
			t.Errorf("%s of an unknown project did not name the project the caller asked for:\n%s", c.name, out)
		}
		if !strings.Contains(out, "is not registered with Ghost yet") {
			t.Errorf("%s of an unknown project did not reach the not-registered sentence:\n%s", c.name, out)
		}
	}

	// The control: a REGISTERED project with rows still lists them, so the guards
	// above are not just "always refuse".
	res := resultText(callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "mlist", "category": "fact",
	}))
	if !strings.Contains(res, "a registered project's own fact") {
		t.Errorf("a registered project's own memory did not appear in its browse:\n%s", res)
	}
}
