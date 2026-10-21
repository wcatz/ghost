package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
)

// TestAnExportThatLeftAnEvidenceRowOutAdvisesTheDeleteNotARekey: a memory whose
// evidence record carries a stamp no form reads is left out of the artifact,
// because the importer refuses it and the exporter asks the importer's own
// predicate. The id is fine and no tool edits an evidence row, so the report
// gives the delete-and-re-save and never the re-key advice, whose delete would
// lose the row's links and history.
func TestAnExportThatLeftAnEvidenceRowOutAdvisesTheDeleteNotARekey(t *testing.T) {
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
	                        VALUES ('m-evidence', 'p1', 'gotcha', 'a plain note', 'mcp', '[]', datetime('now'), datetime('now'))`)
	plantExportRow(t, db, `INSERT INTO memory_provenance (id, memory_id, kind, agent, observed_at, verified_at)
	                        VALUES ('ev-unreadable', 'm-evidence', 'verified', 'ops', 'not a date', 'not a date')`)

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)

	if err == nil {
		t.Error("an export that left an evidence row out exited 0")
	}
	if !strings.Contains(report, assemble.Token("m-evidence")) {
		t.Errorf("the report does not name the record:\n%s", report)
	}
	if !strings.Contains(report, evidenceAdvice) {
		t.Errorf("the report does not give the evidence repair:\n%s", report)
	}
	for _, bad := range []string{"under an id this build accepts", "Ghost cannot re-key a row"} {
		if strings.Contains(report, bad) {
			t.Errorf("the report carries the re-key advice %q:\n%s", bad, report)
		}
	}
}

// The refusal is the importer's own sentence, so the report names the field to
// fix rather than a second description of the rule.
func TestAnExportThatLeftAnEvidenceRowOutNamesTheField(t *testing.T) {
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, tags, created_at, updated_at)
	                        VALUES ('m-evidence', 'p1', 'gotcha', 'a plain note', 'mcp', '[]', datetime('now'), datetime('now'))`)
	plantExportRow(t, db, `INSERT INTO memory_provenance (id, memory_id, kind, agent, observed_at, verified_at)
	                        VALUES ('ev-unreadable', 'm-evidence', 'verified', 'ops', 'not a date', 'not a date')`)

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err := runExportCore(context.Background(), store, &summary, &warn, path, "")
	report := summary.String() + warn.String() + fmtErr(err)
	if !strings.Contains(report, "evidence[0].observed_at") {
		t.Errorf("the report does not name the field to fix:\n%s", report)
	}
}
