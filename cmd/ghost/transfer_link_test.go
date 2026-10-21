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
// the database handle as well. The handle is what makes planting possible: the
// store exposes no exec, and the edges this file plants are states no writer in
// this build produces.
func transferTestStoreWithDB(t *testing.T) (*sql.DB, *memory.Store) {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// TestTheExportAndImportReportsNameTheEdges: the two reports are where an
// operator learns what an artifact holds, and an edge that travelled is a
// record like any other — so the summary counts it, the per-record line names
// it, and an edge that did NOT travel is counted rather than dropped in
// silence.
func TestTheExportAndImportReportsNameTheEdges(t *testing.T) {
	ctx := context.Background()
	src := transferTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	older, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the old answer", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	newer, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the new answer", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	if err := src.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
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
	if !strings.Contains(report.String(), "1 link") {
		t.Errorf("the import summary does not count the edge it restored:\n%s", report.String())
	}
	if !strings.Contains(report.String(), "link:") {
		t.Errorf("the import report has no per-record line for the edge:\n%s", report.String())
	}
}

// TestTheExportReportNamesAnEdgeItLeftOut: an edge the importer would refuse
// is left out of the artifact and NAMED, the way a project, a memory, a task
// and a decision are. The report also has to give the edge its own advice —
// the advice for the kinds that have no delete surface names a command per
// KIND, and an edge in that bucket would leave it naming nothing at all.
func TestTheExportReportNamesAnEdgeItLeftOut(t *testing.T) {
	ctx := context.Background()
	db, src := transferTestStoreWithDB(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	newer, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the new answer", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	older, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the old answer", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at) VALUES (?, ?, 'supersedes', 0.95, 'llm', 'not a stamp')`,
		newer, older); err != nil {
		t.Fatalf("plant the edge: %v", err)
	}

	path := filepath.Join(t.TempDir(), "artifact.jsonl")
	var summary, warn strings.Builder
	err = runExportCore(ctx, src, &summary, &warn, path, "")
	if err == nil {
		t.Error("the export exited zero with an edge it left out of the artifact")
	}
	report := warn.String()
	if !strings.Contains(report, "! left out: link ") {
		t.Errorf("the report does not name the edge it left out:\n%s", report)
	}
	if !strings.Contains(report, "created_at") {
		t.Errorf("the report does not name the field that stopped the edge:\n%s", report)
	}
	if !strings.Contains(report, "An edge left out") {
		t.Errorf("the report gives no advice for the edge it left out:\n%s", report)
	}
	// And the artifact carries no edge at all.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}
	if strings.Contains(string(body), `"type":"link"`) {
		t.Errorf("the artifact carries the edge its own importer would refuse:\n%s", body)
	}
}

// TestTheExportReportNamesTheEdgesItLeftOut: an edge travels only when both
// its endpoints are memories the artifact carries. An edge that reaches out of
// the export's scope is not carried, and the count is what says so — the
// demotion the edge caused is lost with it, and nothing else in the report
// would say so.
func TestTheExportReportNamesTheEdgesItLeftOut(t *testing.T) {
	ctx := context.Background()
	src := transferTestStore(t)
	for _, p := range []struct{ id, path string }{{"p1", "/src/p1"}, {"p2", "/src/p2"}} {
		if err := src.EnsureProject(ctx, p.id, p.path, p.id); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p.id, err)
		}
	}
	inside, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "inside the export", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(inside): %v", err)
	}
	outside, err := src.Create(ctx, "p2", memory.Memory{Category: "fact", Content: "outside the export", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(outside): %v", err)
	}
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
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the artifact: %v", err)
	}
	if strings.Contains(string(body), `"type":"link"`) {
		t.Errorf("the artifact carries an edge whose endpoint it does not:\n%s", body)
	}
}
