package main

import (
	"fmt"
	"sort"
	"strings"
)

func notesBlock(saved []Record) string {
	var project, global []string
	for _, rec := range saved {
		line := "- «" + strings.TrimSpace(rec.Content) + "»" + notesAnnotations(rec)
		if rec.Global {
			global = append(global, line)
		} else {
			project = append(project, line)
		}
	}
	if len(project) == 0 && len(global) == 0 {
		return ""
	}
	var b strings.Builder
	if len(project) > 0 {
		b.WriteString("# Project notes (oldest first)\n\n")
		b.WriteString(strings.Join(project, "\n"))
		b.WriteString("\n")
	}
	if len(global) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("# User notes, all projects (oldest first)\n\n")
		b.WriteString(strings.Join(global, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func notesAnnotations(rec Record) string {
	var out []string
	if rec.ValidUntil != "" {
		out = append(out, "valid until "+rec.ValidUntil)
	}
	if len(rec.Scope) > 0 {
		keys := make([]string, 0, len(rec.Scope))
		for k := range rec.Scope {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("%s=%s", k, rec.Scope[k]))
		}
		out = append(out, "applies to "+strings.Join(pairs, ", "))
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, "; ") + ")"
}
