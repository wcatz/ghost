package mcpserver

import (
	"reflect"
	"sort"
	"strings"
)

// This file answers one question for a caller that cannot see the tools: WHICH
// FIELDS CAN A TOOL EDIT? It exists because a report elsewhere in Ghost has to name
// a fix, and a hand-written list of field names in that report is a second copy of
// each tool's argument struct — which is how the sentence came to promise that
// `ghost_memory_update` edits a memory's `agent` and `ghost_task_update` edits a
// task's `title`, neither of which is an argument either tool takes. A user who
// followed it got a rejected call, and the advice that was meant to be more useful
// than "delete the row" was worse than nothing.
//
// So the list is DERIVED, by reflecting over the same argument structs the handlers
// are registered with, and never transcribed. A field added to a tool is a field
// this reports; a field removed from one stops being reported. The mapping from
// json tag to the field name a report should use is spelled out below rather than
// guessed, because a guess would be the second rule again.

// EditableFields is the set of column names a tool can WRITE, or nil for a tool
// that writes no row of its own.
//
// It reads the tool's argument struct rather than its SQL, and that is deliberate:
// the argument struct is the tool's real contract with a caller, and it is the
// narrower of the two. `UpdateMemoryWithOptions` writes agent and session_id as
// well, but from the EDITING SESSION's provenance (provenanceFor) — a caller cannot
// name their own author, which is a design property and not an oversight. So those
// two are excluded here: a report that told a user to set a memory's agent with
// this tool would be describing something the tool refuses to do.
//
// The names are the COLUMN names, not the json tag names, because the callers are
// reports about columns. The two differ in exactly one place, `source_ref` vs
// `SourceRef`, and the mapping is a table below rather than a transformation so a
// future field has to be added consciously.
func EditableFields(tool string) []string {
	args, ok := toolArgStructs[tool]
	if !ok {
		return nil
	}
	var out []string
	for _, name := range reflectedFieldNames(args) {
		if column, ok := argumentNameToColumn[name]; ok {
			out = append(out, column)
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// toolArgStructs is the tool name to its argument struct. Every entry is the struct
// the handler is REGISTERED with, which is what makes the reflection above a
// statement about the tool rather than about a copy of it.
//
// taskUpdateArgs and taskCompleteArgs are declared inside their handlers as local
// types, so they are referenced through zero values taken at init. A struct of
// pointer-free fields is fully described by its type, so a nil pointer carries the
// whole shape and nothing is dereferenced.
var toolArgStructs = map[string]any{
	"ghost_memory_update": updateArgs{},
	"ghost_task_update":   taskUpdateArgs{},
	"ghost_task_complete": taskCompleteArgs{},
}

// argumentNameToColumn is where an argument's json name differs from the column it
// reaches, and it is EMPTY today — every argument name is already the column name.
//
// It is here anyway, and the emptiness is the point. A tool argument called `ref`
// that writes `source_ref` would otherwise produce a report telling a user to edit
// a column named `ref`, which does not exist; the fix is one entry here rather than
// a report that has to know the difference. An empty map means the rule is "the name
// IS the column", and TestTheColumnMappingIsEmptyUntilAnArgumentNeedsRenaming says
// so — so the first argument that needs renaming is a test failure about a deliberate
// decision rather than a report nobody can follow.
var argumentNameToColumn = map[string]string{}

// reflectedFieldNames is every json argument name on a struct, embedding a struct's
// own fields inline.
//
// Embedded structs are flattened because that is how the SDK sees them: the
// argument schema for ghost_memory_update carries `source_ref` and `confidence` at
// the top level, not nested under `validityArgs`, so a caller names them flat and a
// report that nested them would not match what the user types.
func reflectedFieldNames(v any) []string {
	var out []string
	collectFieldNames(reflect.TypeOf(v), &out)
	sort.Strings(out)
	return out
}

func collectFieldNames(t reflect.Type, out *[]string) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// An embedded struct is a json inline by default, so its fields are the
		// caller's own arguments.
		if f.Anonymous {
			collectFieldNames(f.Type, out)
			continue
		}
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			// No json name at all: the field is not part of the wire contract,
			// and naming it in a report would name something a caller cannot set.
			continue
		}
		if name == "-" {
			continue
		}
		*out = append(*out, name)
	}
}

// EditableFieldList renders EditableFields as a report fragment: the tool name in
// backticks, ready to be the subject of a sentence the caller writes. It is a
// function rather than a constant because the answer is derived, and a constant
// would let the two drift the moment a tool's arguments changed.
func EditableFieldList(tool string) string {
	if len(EditableFields(tool)) == 0 {
		return ""
	}
	return "`" + tool + "`"
}

// HumanFieldList renders a field list for a sentence: "a", "a or b", "a, b or c".
// It is exported because two reports in cmd/ghost need it and a third copy of the
// joining rules is a third thing to keep in step. Empty returns the empty string
// rather than a dangling conjunction.
func HumanFieldList(fields []string) string {
	switch len(fields) {
	case 0:
		return ""
	case 1:
		return fields[0]
	case 2:
		return fields[0] + " or " + fields[1]
	}
	return strings.Join(fields[:len(fields)-1], ", ") + " or " + fields[len(fields)-1]
}
