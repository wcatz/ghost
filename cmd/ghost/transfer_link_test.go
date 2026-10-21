package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

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

// TestTheExportReportNamesTheEdgesItLeftOut: an edge travels only when both
// its endpoints are memories the artifact carries. One that reaches out of the
// export's scope is not carried, and the count is what says so — the demotion
// the edge caused is lost with it, and nothing else in the report would say so.
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
