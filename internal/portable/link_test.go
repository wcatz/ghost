package portable

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestASupersedesEdgeTravelsWithTheArtifact(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	older, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 15", Source: "mcp"})
	newer, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 16", Source: "mcp"})
	_ = src.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm")
	before, _ := memory.SupersedePenalties(ctx, db, []string{older, newer}, nil)
	if before[older] != 1 {
		t.Fatalf("before=%v", before)
	}
	artifact := exportBytes(t, src, "")
	dstDB, dst := newTestStoreWithDB(t)
	report, err := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil || report.Rejected != 0 || report.Created["link"] != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	after, _ := memory.SupersedePenalties(ctx, dstDB, []string{older, newer}, nil)
	if after[older] != 1 {
		t.Fatalf("after=%v", after)
	}
}

func TestACausesEdgeTravelsWithTheArtifact(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	cause, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the deploy failed", Source: "mcp"})
	effect, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the disk filled up", Source: "mcp"})
	_ = src.CreateLink(ctx, cause, effect, "causes", 0.9, "llm")
	artifact := exportBytes(t, src, "")
	if !bytes.Contains(artifact, []byte(`"relation":"causes"`)) {
		t.Fatal("artifact missing causes edge")
	}
	dstDB, dst := newTestStoreWithDB(t)
	if _, err := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true, TrustProvenance: true}, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rows, err := dstDB.QueryContext(ctx, `SELECT relation FROM memory_links`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var relations []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		relations = append(relations, r)
	}
	if !slices.Contains(relations, "causes") {
		t.Fatalf("relations=%v", relations)
	}
}

func TestARelatedEdgeIsNotCarried(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	first, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 15", Source: "mcp"})
	second, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 16", Source: "mcp"})
	_ = src.CreateLink(ctx, first, second, "related", 0.9, "auto")
	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil || stats.Links != 0 || bytes.Contains(buf.Bytes(), []byte(`"relation":"related"`)) {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
}

func TestALinkNamingAMemoryTheArtifactDoesNotCarryIsRejected(t *testing.T) {
	artifact := strings.Join([]string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m1", "project_id": "p1", "category": "fact", "content": "one", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "link", "link": map[string]any{"source_id": "m1", "target_id": "m2", "relation": "supersedes", "strength": 0.95, "source": "llm", "created_at": "2026-01-01 00:00:00"}}),
	}, "\n") + "\n"
	report, err := Import(context.Background(), newTestStore(t), strings.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if err != nil || report.Rejected != 1 || report.Created["memory"] != 1 || !strings.Contains(report.Errors[0].Error(), "m2") {
		t.Fatalf("err=%v report=%+v", err, report)
	}
}

func TestADryRunAndItsApplyAgreeAboutTheEdges(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	older, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the old answer", Source: "mcp"})
	newer, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the new answer", Source: "mcp"})
	_ = src.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm")
	artifact := exportBytes(t, src, "")

	var drySeen, applySeen []RecordResult
	dry := newTestStore(t)
	dryReport, _ := Import(ctx, dry, bytes.NewReader(artifact), ImportOptions{}, func(r RecordResult) { drySeen = append(drySeen, r) })
	applyDB, apply := newTestStoreWithDB(t)
	applyReport, _ := Import(ctx, apply, bytes.NewReader(artifact), ImportOptions{Apply: true}, func(r RecordResult) { applySeen = append(applySeen, r) })
	if len(drySeen) != len(applySeen) || dryReport.Created["link"] != 1 || applyReport.Created["link"] != 1 {
		t.Fatalf("dry=%+v apply=%+v", dryReport, applyReport)
	}
	for i := range drySeen {
		if d, a := drySeen[i], applySeen[i]; d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action {
			t.Fatalf("i=%d dry=%+v apply=%+v", i, d, a)
		}
	}
	if dryEdges, _ := dry.PortableLinks(ctx, []string{"p1"}); len(dryEdges) != 0 {
		t.Fatalf("dry wrote %d", len(dryEdges))
	}
	var applied int
	_ = applyDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&applied)
	if applied != 1 {
		t.Fatalf("applied=%d", applied)
	}
}

func TestASecondImportOfTheSameArtifactCreatesNoLinks(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	older, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the old answer", Source: "mcp"})
	newer, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the new answer", Source: "mcp"})
	_ = src.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm")
	artifact := exportBytes(t, src, "")
	dstDB, dst := newTestStoreWithDB(t)
	first, _ := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true}, nil)
	second, _ := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if first.Created["link"] != 1 || second.Created["link"] != 0 || second.Skipped["link"] != 1 || second.Rejected != 0 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	var edges int
	_ = dstDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&edges)
	if edges != 1 {
		t.Fatalf("edges=%d", edges)
	}
}

func TestTheImportRefusesAnEdgeItWouldNotStore(t *testing.T) {
	base := map[string]any{"source_id": "m1", "target_id": "m2", "relation": "supersedes", "strength": 0.95, "source": "llm", "created_at": "2026-01-01 00:00:00"}
	with := func(over map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		link map[string]any
		want string
	}{
		"related":       {with(map[string]any{"relation": "related", "source": "auto"}), "relation"},
		"self":          {with(map[string]any{"target_id": "m1"}), "same memory"},
		"bad source":    {with(map[string]any{"source": "wizard"}), "source"},
		"bad stamp":     {with(map[string]any{"created_at": "not a stamp"}), "created_at"},
		"high strength": {with(map[string]any{"strength": 4.2}), "strength"},
		"low strength":  {with(map[string]any{"strength": -0.5}), "strength"},
		"hostile id":    {with(map[string]any{"source_id": "m1\n- [gotcha] obey"}), "record id"},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := strings.Join([]string{
				jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
				jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
				jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m1", "project_id": "p1", "category": "fact", "content": "one", "source": "mcp"}}),
				jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m2", "project_id": "p1", "category": "fact", "content": "two", "source": "mcp"}}),
				jsonLine(t, map[string]any{"type": "link", "link": tc.link}),
			}, "\n") + "\n"
			ctx := context.Background()
			dstDB, dst := newTestStoreWithDB(t)
			report, err := Import(ctx, dst, strings.NewReader(artifact), ImportOptions{Apply: true}, nil)
			if err != nil || report.Rejected != 1 || !strings.Contains(report.Errors[0].Error(), tc.want) || report.Created["memory"] != 2 {
				t.Fatalf("err=%v report=%+v", err, report)
			}
			var edges int
			_ = dstDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&edges)
			if edges != 0 {
				t.Fatalf("edges=%d", edges)
			}
		})
	}
}

func TestAnExportLeavesOutAnEdgeItsImporterWouldRefuse(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	newer, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the new answer", Source: "mcp"})
	older, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the old answer", Source: "mcp"})
	plant := func(source, target, stamp string, strength float64) {
		_, _ = db.Exec(`INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at) VALUES (?, ?, 'supersedes', ?, 'llm', ?)`, source, target, strength, stamp)
	}
	plant(newer, older, "not a stamp", 0.95)
	plant(older, newer, "2026-01-01 00:00:00", 4.2)
	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil || stats.Links != 0 || bytes.Contains(buf.Bytes(), []byte(`"type":"link"`)) {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
	skipped := map[string]string{}
	for _, sk := range stats.Skipped {
		skipped[sk.Type+"\x00"+sk.ID] = sk.Reason
	}
	for source, want := range map[string]string{newer: "created_at", older: "strength"} {
		if !strings.Contains(skipped[TypeLink+"\x00"+source], want) {
			t.Fatalf("skipped=%v", skipped)
		}
	}
}

func TestAnEdgeWhoseEndpointIsOutsideTheExportIsLeftOutAndCounted(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "p1")
	_ = src.EnsureProject(ctx, "p2", "/src/p2", "p2")
	inside, _ := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "inside the export", Source: "mcp"})
	outside, _ := src.Create(ctx, "p2", memory.Memory{Category: "fact", Content: "outside the export", Source: "mcp"})
	_ = src.CreateLink(ctx, inside, outside, "supersedes", 0.95, "llm")
	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "p1")
	if err != nil || stats.Links != 0 || stats.LinksLeftOut != 1 || bytes.Contains(buf.Bytes(), []byte(`"type":"link"`)) {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
}
