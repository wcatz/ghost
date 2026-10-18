package portable

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestImportRejectsAnUnknownFeature(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	body := `{"type":"header","schema_version":2,"features":["frob"]}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact","source":"manual","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"

	before, _ := store.PortableMemories(ctx, []string{"p1"})

	_, err := Import(ctx, store, strings.NewReader(body), ImportOptions{Apply: true}, nil)
	if err == nil {
		t.Fatal("import accepted an unknown feature")
	}
	if !strings.Contains(err.Error(), "frob") {
		t.Errorf("error must name the unknown feature, got %v", err)
	}
	after, _ := store.PortableMemories(ctx, []string{"p1"})
	if len(after) != len(before) {
		t.Errorf("a refused artifact wrote rows: before=%d after=%d", len(before), len(after))
	}
}

func TestImportReadsAHeaderWithoutFeatures(t *testing.T) {
	store := newTestStore(t)
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact from an old artifact","source":"manual","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"

	report, err := Import(context.Background(), store, strings.NewReader(body), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("import of an old header failed: %v", err)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("memory created = %d, want 1", report.Created["memory"])
	}
}

func TestImportAcceptsAKnownFeature(t *testing.T) {
	store := newTestStore(t)
	body := `{"type":"header","schema_version":2,"features":["retention"]}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact","source":"manual","retention":"project","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"

	report, err := Import(context.Background(), store, strings.NewReader(body), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("import of a known feature failed: %v", err)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("memory created = %d, want 1", report.Created["memory"])
	}
}

func TestExportImportRoundTripsDeclaredFeatures(t *testing.T) {
	src := newTestStore(t)
	ctx := context.Background()
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "a fact", Source: "manual"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data := exportBytes(t, src, "")
	if !strings.Contains(string(data), `"features":["retention"]`) {
		t.Fatalf("exported header missing declared features: %s", strings.SplitN(string(data), "\n", 2)[0])
	}
	dst := newTestStore(t)
	report, err := Import(ctx, dst, bytes.NewReader(data), ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("import of exported artifact failed: %v", err)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("memory created = %d, want 1", report.Created["memory"])
	}
}
