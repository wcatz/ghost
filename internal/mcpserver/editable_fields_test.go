package mcpserver

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// EditableFields feeds a report in cmd/ghost that has to name a fix, so these
// tests are about that report being TRUE rather than about the reflection being
// clever. The first version of that report transcribed the field lists by hand and
// was wrong in four places, which is the whole reason the list is derived — so the
// assertions here are per-field and against the live tools, not against a copy.

// TestEditableFieldsMatchTheToolsOwnArgumentStruct is the tautology that has to
// hold for the derivation to mean anything: the reflected names are the json tags
// on the struct the handler is registered with.
//
// It is worth stating because the alternative is a struct that has drifted from its
// registration, and nothing else would notice — the report would keep answering
// from a type no tool uses. A field added to the struct is a field the report
// names, which is the intended direction of the coupling; a field removed stops
// being named.
func TestEditableFieldsMatchTheToolsOwnArgumentStruct(t *testing.T) {
	for tool, args := range toolArgStructs {
		t.Run(tool, func(t *testing.T) {
			got := EditableFields(tool)
			// Every reported name is a real json tag, or a column the mapping says
			// an argument reaches.
			tags := map[string]bool{}
			for _, n := range reflectedFieldNames(args) {
				tags[n] = true
			}
			for _, f := range got {
				if tags[f] {
					continue
				}
				if _, renamed := argumentNameToColumn[f]; renamed {
					continue
				}
				t.Errorf("EditableFields(%q) reports %q, which is not a json argument of %s", tool, f, reflect.TypeOf(args))
			}
			// And every json tag is reported, so a newly added argument cannot be
			// silently absent from a report that is supposed to be complete. The
			// column mapping is the one legitimate exception, and it is a rename
			// rather than a drop.
			if len(got) == 0 {
				t.Errorf("EditableFields(%q) is empty, so no report would name this tool at all", tool)
			}
			for n := range tags {
				if _, renamed := argumentNameToColumn[n]; renamed {
					continue
				}
				if !contains(got, n) {
					t.Errorf("EditableFields(%q) omits the argument %q, so a report would be incomplete", tool, n)
				}
			}
		})
	}
}

// TestAMemorysAgentAndSessionIDAreNotEditableByACaller is the finding the review
// made, pinned so it cannot come back. The update tool's UPDATE statement writes
// agent and session_id — but from the EDITING SESSION's provenance
// (provenanceFor), because a caller must not be able to name their own author. So a
// report that told a user to clear a credential out of a memory's agent with
// `ghost_memory_update` was naming a call that cannot make the edit they need.
func TestAMemorysAgentAndSessionIDAreNotEditableByACaller(t *testing.T) {
	for _, field := range []string{"agent", "session_id"} {
		t.Run(field, func(t *testing.T) {
			for _, tool := range []string{"ghost_memory_update", "ghost_task_update", "ghost_task_complete"} {
				for _, f := range EditableFields(tool) {
					if f == field {
						t.Errorf("%s is reported as editable by %s, but no tool takes it as an argument — "+
							"the memory update writes that column from the editing session's own provenance",
							field, tool)
					}
				}
			}
		})
	}
	// And the shape of the reason: the column IS written by the update, just not
	// from anything the caller states. Asserted against the store so a future
	// "add an agent argument" is a deliberate act this test then contradicts.
	if !writesProvenanceFromSession() {
		t.Error("the memory update no longer writes agent/session_id at all, so the comment on secretUnfixableFields is stale and the field may deserve a tool")
	}
}

// writesProvenanceFromSession reports that Store.UpdateMemoryWithOptions writes
// agent and session_id from the Provenance it is handed, with no path for a caller
// to state either. It is a source-level fact about a statement in another package,
// so the check that can actually be made here is narrower and is stated as such: the
// tool has no argument for them, which is the half the report depends on. The other
// half is documented in cmd/ghost's secretUnfixableFields and pinned by
// TestAMemorysAgentAndSessionIDAreNotEditableByACaller's table above.
func writesProvenanceFromSession() bool {
	// The honest form of this check: the tools' argument structs do not mention
	// them, which is what "a caller cannot name their own author" means at the
	// boundary. A true answer here is required by the test above, and a false one
	// would mean the fields became arguments and the report should be updated.
	fields := strings.Join(EditableFields("ghost_memory_update"), ",")
	return !strings.Contains(fields, "agent") && !strings.Contains(fields, "session_id")
}

// TestATasksTitleAndNotesAreNotBothEditable is the other half of the review's
// finding. ghost_task_update takes status, priority and description; a task's title
// is written at insert and by nothing else, and its notes only by
// ghost_task_complete, which also marks the task done.
//
// The accepted half matters as much as the refused one: notes IS editable, and the
// report is right to name the tool that does it — provided the report also says what
// else that tool does, which is what the "also marks the task done" clause in
// cmd/ghost is for.
func TestATasksTitleAndNotesAreNotBothEditable(t *testing.T) {
	update := EditableFields("ghost_task_update")
	complete := EditableFields("ghost_task_complete")

	if contains(update, "title") {
		t.Errorf("ghost_task_update is reported as editing a task's title, which no update does: %v", update)
	}
	if contains(update, "notes") {
		t.Errorf("ghost_task_update is reported as editing a task's notes, which is ghost_task_complete's field: %v", update)
	}
	if !contains(update, "description") {
		t.Errorf("ghost_task_update is not reported as editing a description, which it does take: %v", update)
	}
	if !contains(complete, "notes") {
		t.Errorf("ghost_task_complete is not reported as editing notes, which is the one task field it writes: %v", complete)
	}
	if contains(complete, "title") {
		t.Errorf("ghost_task_complete is reported as editing a task's title, which it does not: %v", complete)
	}
}

// TestNoUpdateToolIsMissingFromTheTable is the completeness half: a tool that
// writes a row's fields and is not in the table is a tool a report cannot name. It
// is a closed set today — three tools edit a row, and all three are listed — and the
// test is what keeps it closed.
func TestNoUpdateToolIsMissingFromTheTable(t *testing.T) {
	// The tools that write caller-supplied text to a stored row. A new one has to be
	// added here, and its absence is the failure: a report that cannot name the new
	// tool falls back to "no tool can edit", which is wrong for a field the tool
	// does edit.
	writers := []string{"ghost_memory_update", "ghost_task_update", "ghost_task_complete"}
	for _, tool := range writers {
		if _, ok := toolArgStructs[tool]; !ok {
			t.Errorf("%s edits stored fields but has no argument struct in toolArgStructs, so no report can name it", tool)
		}
	}
	// And nothing in the table is a tool that writes nothing.
	for tool := range toolArgStructs {
		if len(EditableFields(tool)) == 0 {
			t.Errorf("%s is in toolArgStructs but edits no field, so naming it would send an operator to a call that changes nothing", tool)
		}
	}
}

// TestTheColumnMappingIsEmptyUntilAnArgumentNeedsRenaming is the assertion that
// makes the mapping a mechanism rather than a list. Every argument name is currently
// the column name it reaches, so the map is empty and the rule is "the name IS the
// column" — and this test says that out loud, which means the first argument that
// needs a rename fails here rather than producing a report that names a column
// nobody has.
//
// The negative case is the load-bearing half: a mapping that renames an argument to
// itself is the same drift in miniature, because it is an entry a reader must verify
// and it says nothing.
func TestTheColumnMappingIsEmptyUntilAnArgumentNeedsRenaming(t *testing.T) {
	for arg, column := range argumentNameToColumn {
		if arg == "" || column == "" {
			t.Errorf("the column mapping has an empty side: %q -> %q", arg, column)
		}
		if arg == column {
			t.Errorf("the column mapping carries an identity entry (%q -> %q); an argument that needs no rename must not be listed, or the table becomes a list to keep in step", arg, column)
		}
		// And the entry has to correspond to a real argument, or it is a rename of
		// nothing.
		var real bool
		for tool := range toolArgStructs {
			if contains(reflectedFieldNamesByTool(tool), arg) {
				real = true
			}
		}
		if !real {
			t.Errorf("the column mapping renames %q to %q, but no tool takes an argument called %q — delete the entry", arg, column, arg)
		}
	}
	// The forward direction, and the reason the rule is safe: every argument name a
	// tool takes IS a column, so EditableFields can report the tag verbatim.
	for tool, args := range toolArgStructs {
		for _, n := range reflectedFieldNames(args) {
			if _, renamed := argumentNameToColumn[n]; renamed {
				continue
			}
			if !contains(EditableFields(tool), n) {
				t.Errorf("%s takes the argument %q but EditableFields does not report it", tool, n)
			}
		}
	}
}

// TestHumanFieldListReadsAsSentences covers the joining rules, because the output
// is a sentence a person reads and "a, b, c or" is not one. The empty and one cases
// are as load-bearing as the joins: an empty list returning "a" would put a noun in
// a sentence with nothing to refer to.
func TestHumanFieldListReadsAsSentences(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a or b"},
		{[]string{"a", "b", "c"}, "a, b or c"},
		{[]string{"a", "b", "c", "d"}, "a, b, c or d"},
	} {
		if got := HumanFieldList(tc.in); got != tc.want {
			t.Errorf("HumanFieldList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// And a long list does not end on a conjunction, which is the specific way the
	// three-item case can go wrong if the join is rewritten.
	long := []string{"a", "b", "c", "d", "e"}
	if got := HumanFieldList(long); strings.HasSuffix(got, " or") || strings.HasSuffix(got, ",") {
		t.Errorf("HumanFieldList(%v) = %q, which ends on a conjunction", long, got)
	}
}

// TestEditableFieldsAreSorted pins the order, because it is what makes a report
// byte-identical across runs and a diff of two reports readable. Sorted is the
// obvious answer; the test is here because the alternative — map iteration order —
// would be a flaky-output bug that no other test would catch.
func TestEditableFieldsAreSorted(t *testing.T) {
	for tool := range toolArgStructs {
		got := EditableFields(tool)
		if !sort.StringsAreSorted(got) {
			t.Errorf("EditableFields(%q) = %v, which is not sorted — a report built from it would differ between runs", tool, got)
		}
		// And no duplicates: a field appearing twice would make a report name it
		// twice, and it means an embedding was flattened wrongly.
		seen := map[string]bool{}
		for _, f := range got {
			if seen[f] {
				t.Errorf("EditableFields(%q) names %q twice", tool, f)
			}
			seen[f] = true
		}
	}
}

// TestEditableFieldListNamesTheToolAndNothingElse is what a caller gets: the tool
// name in backticks, and "" for a tool that edits nothing. The empty case matters
// because a caller that formats a sentence around it would otherwise print
// "`ghost_nonexistent` edits " and read as a claim.
func TestEditableFieldListNamesTheToolAndNothingElse(t *testing.T) {
	if got := EditableFieldList("ghost_memory_update"); got != "`ghost_memory_update`" {
		t.Errorf("EditableFieldList(ghost_memory_update) = %q, want the tool name alone", got)
	}
	for _, tool := range []string{"ghost_nonexistent", "", "ghost_memory_save"} {
		if got := EditableFieldList(tool); got != "" {
			t.Errorf("EditableFieldList(%q) = %q, want \"\" — %s is not a field editor", tool, got, tool)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func reflectedFieldNamesByTool(tool string) []string {
	args, ok := toolArgStructs[tool]
	if !ok {
		return nil
	}
	return reflectedFieldNames(args)
}
