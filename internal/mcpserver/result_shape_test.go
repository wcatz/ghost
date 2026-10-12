package mcpserver

import (
	"fmt"
	"strings"
	"testing"
)

// labeledID pulls the value out of an answer that names a label, e.g.
// `decision_id: ABC123`. The decision record answer prints both the decision and
// its companion memory this way, and a test that needs one of them should not
// re-implement the sentence's shape.
func labeledID(t *testing.T, answer, label string) string {
	t.Helper()
	i := strings.Index(answer, label)
	if i < 0 {
		t.Fatalf("the answer carries no %q: %s", label, answer)
	}
	rest := answer[i+len(label):]
	if end := strings.IndexAny(rest, ") \n"); end >= 0 {
		rest = rest[:end]
	}
	id := strings.TrimSpace(rest)
	if id == "" {
		t.Fatalf("the answer names %q with an empty id: %s", label, answer)
	}
	return id
}

// TestDecisionSupersedeOutsideTheProjectReportsPartialFailure is the RED test for
// the first shape #1041 names: a decision recorded with a `supersedes` id the new
// project cannot mark.
//
// Two writes happen and only one can succeed. The new decision is committed —
// `RecordDecision` ran and returned an id — and the supersession of the old one
// fails, because the old id belongs to a different project and
// `SupersedeDecisionReport` scopes its UPDATE to the project it is handed. The
// old answer buried that failure in a `WARNING:` on an ordinary success, so a
// caller that checks only `isError` reads "Decision recorded" and believes the
// reversal landed; the old decision stays live and keeps answering.
//
// The fix reports it as an error RESULT rather than a plain Go error, because the
// content is the one place the committed `decision_id` and `memory_id` live and a
// Go error would drop both — the caller would be told the call failed while a
// decision it cannot name sits in the store.
func TestDecisionSupersedeOutsideTheProjectReportsPartialFailure(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	// The old decision and its project exist on their own, so the supersession is
	// refused for exactly the projects-differ reason rather than a missing row.
	plantProject(t, db, "other", "other", "/p/other")
	plantDecision(t, db, "other", "OLDOTHER", "the old call", "we chose A", "because", "active")

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "abc123",
		"title":      "the new call",
		"decision":   "we chose B",
		"rationale":  "A failed",
		"supersedes": "OLDOTHER",
	})
	out := resultText(res)

	if !res.IsError {
		t.Fatalf("the partial failure was reported as a success; a caller checking only isError "+
			"reads the reversal as done:\n%s", out)
	}
	// It has to SAY the new decision landed, or the error reads as "nothing was
	// written" and the caller records it a second time.
	if !strings.Contains(out, "was committed") && !strings.Contains(out, "WAS committed") {
		t.Errorf("the answer does not say the new decision was committed:\n%s", out)
	}
	// And it has to say the old one is NOT marked, naming it through the safe
	// renderer so the caller knows which id to act on.
	if !strings.Contains(out, "NOT marked") {
		t.Errorf("the answer does not say the old decision was left unmarked:\n%s", out)
	}
	if !strings.Contains(out, "OLDOTHER") {
		t.Errorf("the answer does not name the id that was not superseded:\n%s", out)
	}
	// Both committed ids stay in the content: the error must not cost the caller
	// the decision it just wrote.
	newID := labeledID(t, out, "decision_id: ")
	labeledID(t, out, "memory_id: ")
	if strings.Contains(newID, " ") || newID == "OLDOTHER" {
		t.Errorf("the answer's decision_id is not the new decision's id: %q", newID)
	}

	// The old decision is still live: nothing was marked.
	var old string
	if err := db.QueryRow(`SELECT status FROM decisions WHERE id = 'OLDOTHER'`).Scan(&old); err != nil {
		t.Fatalf("read the old decision's status: %v", err)
	}
	if old != "active" {
		t.Errorf("the old decision's status = %q, want active — a failed supersession marked it anyway", old)
	}
	// The new decision WAS committed, so the error is not a claim that the call
	// wrote nothing.
	var newStatus string
	if err := db.QueryRow(`SELECT status FROM decisions WHERE id = ?`, newID).Scan(&newStatus); err != nil {
		t.Fatalf("the new decision %s was not committed: %v", newID, err)
	}
	if newStatus != "active" {
		t.Errorf("the new decision's status = %q, want active", newStatus)
	}
}

// TestTaskListRefusesAnUnknownStatus is the second shape #1041 names. The list
// tool handed the raw status to the store, and the store's `WHERE status = ?`
// simply matched nothing, so a misspelt status answered `No tasks found.` — the
// same sentence a genuinely empty project gets, which reads as a measurement about
// the project rather than a refusal of the argument.
func TestTaskListRefusesAnUnknownStatus(t *testing.T) {
	db, _, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	// A task IS present, so the buggy answer would be a lie about this project.
	plantTask(t, db, "abc123", "T1", "a real task", "d", "pending")

	res := callTool(t, session, "ghost_task_list", map[string]any{
		"project_id": "test-project", "status": "pendding",
	})
	out := resultText(res)

	if !res.IsError {
		t.Fatalf("an unknown status was answered instead of refused:\n%s", out)
	}
	if strings.Contains(out, "No tasks found") {
		t.Errorf("an unknown status was answered as an empty project:\n%s", out)
	}
	want := `invalid status "pendding" — must be one of: pending, active, blocked, done`
	if !strings.Contains(out, want) {
		t.Errorf("the refusal does not name the allowed statuses (%q):\n%s", want, out)
	}
	// The control: a valid status still filters, so this is not "always refuse".
	ok := resultText(callTool(t, session, "ghost_task_list", map[string]any{
		"project_id": "test-project", "status": "pending",
	}))
	if !strings.Contains(ok, "a real task") {
		t.Errorf("a valid status stopped listing:\n%s", ok)
	}
}

// TestMemoriesListRefusesAnUnknownCategory is the third shape #1041 names, and it
// is tested against an UNRESOLVED project because that is where the defect is
// reachable at the tool boundary: on a registered project the store already
// refuses the category (its `ListMemories` validates), but a browse of a project
// Ghost has never heard of never reached the store at all — it fell to the
// not-registered sentence with the category unexamined, so the caller was told
// about the project and not about the argument they actually got wrong.
func TestMemoriesListRefusesAnUnknownCategory(t *testing.T) {
	st := newMemoriesListStore(t)
	_, session := validityServerFor(t, st)

	want := `invalid category "bogus" — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact`
	for name, project := range map[string]string{
		"an unresolved project": "no-such-project-here",
		"a registered project":  "mlist",
	} {
		t.Run(name, func(t *testing.T) {
			res := callTool(t, session, "ghost_memories_list", map[string]any{
				"project_id": project, "category": "bogus",
			})
			out := resultText(res)
			if !res.IsError {
				t.Fatalf("an unknown category was answered instead of refused:\n%s", out)
			}
			if strings.Contains(out, "No memories found") || strings.Contains(out, "not registered") {
				t.Errorf("the answer is about the project, not the category:\n%s", out)
			}
			if !strings.Contains(out, want) {
				t.Errorf("the refusal does not name the allowed categories (%q):\n%s", want, out)
			}
		})
	}

	// The control: a valid category still browses.
	ok := resultText(callTool(t, session, "ghost_memories_list", map[string]any{
		"project_id": "mlist", "category": "fact",
	}))
	if !strings.Contains(ok, "a registered project's own fact") {
		t.Errorf("a valid category stopped listing:\n%s", ok)
	}
}

// TestEveryTagBearingWriteToolRefusesMoreTagsThanTheBound is the fourth shape
// #1041 names, driven through the four tools that take a tag list rather than
// through validateTags alone. The writer used to CUT the excess to ten and answer
// success, so the eleventh label vanished with nothing in the answer to say so —
// a caller that passed twelve had no way to learn that two were dropped. The bound
// is `tagMaxCount`, referenced rather than spelled, so the test and the writer
// cannot drift.
func TestEveryTagBearingWriteToolRefusesMoreTagsThanTheBound(t *testing.T) {
	over := make([]string, tagMaxCount+1)
	for i := range over {
		over[i] = fmt.Sprintf("t%d", i)
	}
	atBound := over[:tagMaxCount]

	refuse := []struct {
		name string
		args map[string]any
	}{
		{"ghost_memory_save", map[string]any{"project_id": "vproj", "content": "c", "category": "fact", "tags": over}},
		{"ghost_save_global", map[string]any{"content": "c", "category": "preference", "tags": over}},
		{"ghost_decision_record", map[string]any{"project_id": "vproj", "title": "t", "decision": "d", "rationale": "r", "tags": over}},
	}
	for _, tc := range refuse {
		t.Run(tc.name, func(t *testing.T) {
			_, session := newValiditySession(t)
			res := callTool(t, session, tc.name, tc.args)
			out := resultText(res)
			if !res.IsError {
				t.Fatalf("%s accepted %d tags, over the bound of %d:\n%s", tc.name, len(over), tagMaxCount, out)
			}
			if !strings.Contains(out, "at most") || !strings.Contains(out, "tag") {
				t.Errorf("%s refused the list without naming the bound:\n%s", tc.name, out)
			}
		})
	}
	t.Run("ghost_memory_update", func(t *testing.T) {
		_, session := newValiditySession(t)
		id, ok := extractID(resultText(callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "vproj", "content": "a claim to be re-tagged", "category": "fact",
		})))
		if !ok {
			t.Fatal("the seed save named no id")
		}
		res := callTool(t, session, "ghost_memory_update", map[string]any{
			"project_id": "vproj", "memory_id": id, "tags": over,
		})
		out := resultText(res)
		if !res.IsError {
			t.Fatalf("ghost_memory_update accepted %d tags, over the bound of %d:\n%s", len(over), tagMaxCount, out)
		}
		if !strings.Contains(out, "at most") || !strings.Contains(out, "tag") {
			t.Errorf("ghost_memory_update refused the list without naming the bound:\n%s", out)
		}
	})
	// The control: exactly the bound is accepted, so this is not "refuse long
	// lists" but "refuse one past the bound".
	t.Run("exactly the bound is accepted", func(t *testing.T) {
		_, session := newValiditySession(t)
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "vproj", "content": "c", "category": "fact", "tags": atBound,
		})
		if res.IsError {
			t.Fatalf("refused %d tags, at the bound:\n%s", len(atBound), resultText(res))
		}
	})
}
