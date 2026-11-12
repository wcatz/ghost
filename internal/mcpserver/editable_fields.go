package mcpserver

import (
	"reflect"
	"sort"
	"strings"
)

// EditableFields reports the column names a tool can write.
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

var toolArgStructs = map[string]any{
	"ghost_memory_update": updateArgs{},
	"ghost_task_update":   taskUpdateArgs{},
	"ghost_task_complete": taskCompleteArgs{},
}

var argumentNameToColumn = map[string]string{}

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
		if f.Anonymous {
			collectFieldNames(f.Type, out)
			continue
		}
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		if name == "-" {
			continue
		}
		*out = append(*out, name)
	}
}

// EditableFieldList renders the tool name as a report fragment.
func EditableFieldList(tool string) string {
	if len(EditableFields(tool)) == 0 {
		return ""
	}
	return "`" + tool + "`"
}

// HumanFieldList renders a field list for a sentence.
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
