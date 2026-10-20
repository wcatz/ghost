package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/mcpserver"
)

// TestAnExportThatLeftAValidityRowOutAdvisesAnUpdateNotARekey: a memory left
// out for an unreadable stamp, an inverted or empty window, or a confidence
// outside 0 to 1 has a fine id, and ghost_memory_update repairs it in place. So
// the report gives that repair, says the tool cannot remove a stamp, and never
// gives the re-key advice, whose delete would lose the row's links and history.
func TestAnExportThatLeftAValidityRowOutAdvisesAnUpdateNotARekey(t *testing.T) {
	for _, tc := range []struct {
		name, set, reason string
	}{
		{"unreadable stamp", `valid_until = 'next tuesday'`, "valid_until must be RFC 3339"},
		{"inverted window", `valid_from = '2026-12-01 00:00:00', valid_until = '2026-01-01 00:00:00'`, "is not after valid_from"},
		{"equal window", `valid_from = '2026-06-01 00:00:00', valid_until = '2026-06-01 00:00:00'`, "is not after valid_from"},
		{"confidence out of range", `confidence = 7`, "confidence must be between 0 and 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := exportTestStore(t)
			plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
			                        VALUES ('m-validity', 'p1', 'gotcha', 'a plain note', 'mcp', '[]', datetime('now'), datetime('now'))`)
			plantExportRow(t, db, `UPDATE memories SET `+tc.set+` WHERE id = 'm-validity'`)

			path := filepath.Join(t.TempDir(), "artifact.jsonl")
			var summary, warn strings.Builder
			err := runExportCore(context.Background(), store, &summary, &warn, path, "")
			report := summary.String() + warn.String() + fmtErr(err)

			if err == nil {
				t.Error("an export that left a validity row out exited 0")
			}
			if !strings.Contains(report, assemble.Token("m-validity")) || !strings.Contains(report, tc.reason) {
				t.Errorf("the report does not name the record and its reason %q:\n%s", tc.reason, report)
			}
			if !strings.Contains(report, validityAdvice) {
				t.Errorf("the report does not give the in-place repair:\n%s", report)
			}
			for _, bad := range []string{"under an id this build accepts", "Ghost cannot re-key a row"} {
				if strings.Contains(report, bad) {
					t.Errorf("the report carries the re-key advice %q:\n%s", bad, report)
				}
			}
		})
	}
}

// TestAValidityRowBesideAnIDRowGetsBothRepairs: the advice is collected per
// batch, so a validity refusal does not swallow the re-key advice an id-shape
// refusal in the same export still needs.
func TestAValidityRowBesideAnIDRowGetsBothRepairs(t *testing.T) {
	store, db := exportTestStore(t)
	ins := `INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
	         VALUES (?, 'p1', 'gotcha', ?, 'mcp', '[]', datetime('now'), datetime('now'))`
	plantExportRow(t, db, ins, "m-validity", "a plain note")
	plantExportRow(t, db, `UPDATE memories SET confidence = 7 WHERE id = 'm-validity'`)
	plantExportRow(t, db, ins, "M BAD", "another plain note")

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)
	for _, want := range []string{validityAdvice, "under an id this build accepts"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
}

// TestTheValidityAdviceNamesFieldsTheToolTakes: the sentence names
// ghost_memory_update for four fields, so each must be one of that tool's own
// arguments, read from the argument struct it is registered with.
func TestTheValidityAdviceNamesFieldsTheToolTakes(t *testing.T) {
	fields := mcpserver.EditableFields("ghost_memory_update")
	for _, f := range []string{"valid_from", "valid_until", "verified_at", "confidence"} {
		if !slices.Contains(fields, f) {
			t.Errorf("the advice sends %s to ghost_memory_update, which takes %v", f, fields)
		}
		if !strings.Contains(validityAdvice, f) {
			t.Errorf("the advice does not name %s", f)
		}
	}
}
