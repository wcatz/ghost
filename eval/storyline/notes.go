package main

import (
	"fmt"
	"sort"
	"strings"
)

// notesBlock renders the flat-notes arm's session block: every record saved so
// far, oldest first, as a plain markdown notes file — the shape of a host's
// built-in file memory (a project notes file plus a user-level one), which is
// the baseline Ghost has to beat rather than an empty block.
//
// The baseline is deliberately generous, so a Ghost win means something:
//
//   - every record is present, in save order, with nothing filtered — a notes
//     file keeps an expired or replaced line until someone edits it out;
//   - a record's validity window and scope are written beside it in the words a
//     person keeping notes would use, because a stale-fact arc whose content
//     never says it is old would otherwise be lost by construction rather than
//     by the notes;
//   - a global record goes under the user-level heading, because a user-level
//     notes file is loaded in every project.
//
// What it leaves out is exactly Ghost's own work: ids, categories, importance,
// replacement marks, and any filtering, ranking or budget. Each note is wrapped
// in « and » so the stage prompt's framing sentence holds for this arm too, and
// an empty store renders as an empty block, as the without-Ghost arm does.
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

// notesAnnotations writes a record's validity window and scope the way a
// person would beside a note: "(valid until 2025-01-31)", "(applies to
// environment=staging)". Scope keys are sorted so the block is deterministic.
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
