package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// transferTestStoreWithDB opens a store over a temp-dir database and hands back
// the handle too: the store exposes no exec, and the edges planted here are
// states no writer in this build produces.
func transferTestStoreWithDB(t *testing.T) (*sql.DB, *memory.Store) {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// seedMemories creates one fact per content in project and returns their ids.
func seedMemories(t *testing.T, store *memory.Store, project string, contents ...string) []string {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProject(ctx, project, "/src/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var ids []string
	for _, c := range contents {
		id, err := store.Create(ctx, project, memory.Memory{Category: "fact", Content: c, Source: "mcp"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// plantBadEdge inserts a supersedes edge with a created_at the importer refuses.
func plantBadEdge(t *testing.T, db *sql.DB, source, target string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at) VALUES (?, ?, 'supersedes', 0.95, 'llm', 'not a stamp')`,
		source, target); err != nil {
		t.Fatalf("plant the edge: %v", err)
	}
}

func artifactCarriesNoLink(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}
	if strings.Contains(string(body), `"type":"link"`) {
		t.Errorf("the artifact carries an edge it must not:\n%s", body)
	}
}

// TestTheExportAndImportReportsNameTheEdges: an edge that travelled is counted in
// the summary and named per record; one that did not is counted, not dropped.
func TestTheExportAndImportReportsNameTheEdges(t *testing.T) {
	ctx := context.Background()
	src := transferTestStore(t)
	ids := seedMemories(t, src, "p1", "the new answer", "the old answer")
	if err := src.CreateLink(ctx, ids[0], ids[1], "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	if err := runExportCore(ctx, src, &summary, &warn, path, ""); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	if !strings.Contains(summary.String(), "1 link") {
		t.Errorf("the export summary does not count the edge it carried:\n%s", summary.String())
	}
	var report strings.Builder
	if err := runImportCore(ctx, transferTestStore(t), path, portable.ImportOptions{Apply: true}, &report); err != nil {
		t.Fatalf("runImportCore: %v", err)
	}
	if !strings.Contains(report.String(), "1 link") || !strings.Contains(report.String(), "link:") {
		t.Errorf("the import report does not count and name the edge:\n%s", report.String())
	}
}

// TestTheExportReportNamesAnEdgeItLeftOut: an edge the importer would refuse is
// left out and NAMED, with its own advice, and each refused edge gets a distinct
// line (source, relation and its own target) so two from one source differ.
func TestTheExportReportNamesAnEdgeItLeftOut(t *testing.T) {
	db, src := transferTestStoreWithDB(t)
	ids := seedMemories(t, src, "p1", "the new answer", "the first old answer", "the second old answer")
	plantBadEdge(t, db, ids[0], ids[1])
	plantBadEdge(t, db, ids[0], ids[2])

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	if err := runExportCore(context.Background(), src, &summary, &warn, path, ""); err == nil {
		t.Error("the export exited zero with edges it left out of the artifact")
	}
	report := warn.String()
	for _, want := range []string{"created_at", "An edge left out"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not say %q:\n%s", want, report)
		}
	}
	var lines []string
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, "! left out: link ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || lines[0] == lines[1] {
		t.Fatalf("want 2 distinct left-out link lines, got %d:\n%s", len(lines), report)
	}
	for i, line := range lines {
		if !strings.Contains(line, ids[0]) || !strings.Contains(line, "supersedes") || !strings.Contains(line, ids[1+i]) && !strings.Contains(line, ids[2-i]) {
			t.Errorf("the left-out line names the wrong source, relation or target:\n%s", line)
		}
	}
	artifactCarriesNoLink(t, path)
}

// TestTheExportReportNamesTheEdgesItLeftOut: an edge travels only when both
// endpoints are in the artifact; one reaching out of the export's scope is
// counted, since the demotion it caused is lost with it.
func TestTheExportReportNamesTheEdgesItLeftOut(t *testing.T) {
	ctx := context.Background()
	src := transferTestStore(t)
	inside := seedMemories(t, src, "p1", "inside the export")[0]
	outside := seedMemories(t, src, "p2", "outside the export")[0]
	if err := src.CreateLink(ctx, inside, outside, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	if err := runExportCore(ctx, src, &summary, &warn, path, "p1"); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	if !strings.Contains(summary.String(), "1 link left out") {
		t.Errorf("the export summary does not say that an edge was left out:\n%s", summary.String())
	}
	artifactCarriesNoLink(t, path)
}
