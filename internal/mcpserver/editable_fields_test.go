package mcpserver

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestEditableFieldsMatchTheToolsOwnArgumentStruct(t *testing.T) {
	for tool, args := range toolArgStructs {
		t.Run(tool, func(t *testing.T) {
			got := EditableFields(tool)
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
	if !writesProvenanceFromSession() {
		t.Error("the memory update no longer writes agent/session_id at all, so the comment on secretUnfixableFields is stale and the field may deserve a tool")
	}
}

func writesProvenanceFromSession() bool {
	fields := strings.Join(EditableFields("ghost_memory_update"), ",")
	return !strings.Contains(fields, "agent") && !strings.Contains(fields, "session_id")
}

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

func TestNoUpdateToolIsMissingFromTheTable(t *testing.T) {
	writers := []string{"ghost_memory_update", "ghost_task_update", "ghost_task_complete"}
	for _, tool := range writers {
		if _, ok := toolArgStructs[tool]; !ok {
			t.Errorf("%s edits stored fields but has no argument struct in toolArgStructs, so no report can name it", tool)
		}
	}
	for tool := range toolArgStructs {
		if len(EditableFields(tool)) == 0 {
			t.Errorf("%s is in toolArgStructs but edits no field, so naming it would send an operator to a call that changes nothing", tool)
		}
	}
}

func TestTheColumnMappingIsEmptyUntilAnArgumentNeedsRenaming(t *testing.T) {
	for arg, column := range argumentNameToColumn {
		if arg == "" || column == "" {
			t.Errorf("the column mapping has an empty side: %q -> %q", arg, column)
		}
		if arg == column {
			t.Errorf("the column mapping carries an identity entry (%q -> %q); an argument that needs no rename must not be listed, or the table becomes a list to keep in step", arg, column)
		}
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
	long := []string{"a", "b", "c", "d", "e"}
	if got := HumanFieldList(long); strings.HasSuffix(got, " or") || strings.HasSuffix(got, ",") {
		t.Errorf("HumanFieldList(%v) = %q, which ends on a conjunction", long, got)
	}
}

func TestEditableFieldsAreSorted(t *testing.T) {
	for tool := range toolArgStructs {
		got := EditableFields(tool)
		if !sort.StringsAreSorted(got) {
			t.Errorf("EditableFields(%q) = %v, which is not sorted — a report built from it would differ between runs", tool, got)
		}
		seen := map[string]bool{}
		for _, f := range got {
			if seen[f] {
				t.Errorf("EditableFields(%q) names %q twice", tool, f)
			}
			seen[f] = true
		}
	}
}

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
