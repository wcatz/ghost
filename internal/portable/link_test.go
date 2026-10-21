package portable

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestASupersedesEdgeTravelsWithTheArtifact is the round trip the issue is
// about. A `supersedes` edge is a classifier's judgement about a pair of
// memories, and nothing recomputes it after an import: the linking worker
// rebuilds the `related` edges, and the supersede pass only writes NEW ones. So
// an artifact that dropped the edge restored a store in which the memory the
// edge had demoted was no longer demoted, and the withdraw and reassess repairs
// had no edge to act on.
//
// The test asserts the EFFECT rather than the shape: the restored store holds
// the edge, and the demotion it causes is applied there. A test that only
// counted the link rows would pass on an edge the ranking ignored.
func TestASupersedesEdgeTravelsWithTheArtifact(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	older, err := src.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "the database runs postgres 15", Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	newer, err := src.Create(ctx, "p1", memory.Memory{
		Category: "fact", Content: "the database runs postgres 16", Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	if err := src.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	// The edge demotes the older memory where it stands, so the round trip has
	// something to lose.
	before, err := memory.SupersedePenalties(ctx, db, []string{older, newer}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties(src): %v", err)
	}
	if before[older] != 1 {
		t.Fatalf("the source store's demotion = %v, want the older memory demoted once", before)
	}

	artifact := exportBytes(t, src, "")

	dstDB, dst := newTestStoreWithDB(t)
	report, err := Import(ctx, dst, bytes.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("the exported artifact was rejected %d time(s): %v", report.Rejected, report.Errors)
	}
	if report.Created["link"] != 1 {
		t.Errorf("the import created %d edge(s), want the one the export carried", report.Created["link"])
	}

	after, err := memory.SupersedePenalties(ctx, dstDB, []string{older, newer}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties(dst): %v", err)
	}
	if after[older] != 1 {
		t.Errorf("the restored store's demotion = %v, want the older memory demoted once — the edge did not travel", after)
	}
}

// TestACausesEdgeTravelsWithTheArtifact is the other relation the supersede
// pass writes. A `causes` edge is a judgement about which way a pair reads, and
// the withdraw repair reaches it by name — so an edge that did not travel left
// that repair with nothing to act on.
func TestACausesEdgeTravelsWithTheArtifact(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	cause, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the deploy failed", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(cause): %v", err)
	}
	effect, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the disk filled up", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(effect): %v", err)
	}
	if err := src.CreateLink(ctx, cause, effect, "causes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	artifact := exportBytes(t, src, "")
	if !bytes.Contains(artifact, []byte(`"relation":"causes"`)) {
		t.Fatalf("the artifact does not carry the causes edge:\n%s", artifact)
	}

	dstDB, dst := newTestStoreWithDB(t)
	if _, err := Import(ctx, dst, bytes.NewReader(artifact),
		ImportOptions{Apply: true, TrustProvenance: true}, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	var relations []string
	rows, err := dstDB.QueryContext(ctx, `SELECT relation FROM memory_links`)
	if err != nil {
		t.Fatalf("read the restored edges: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var relation string
		if err := rows.Scan(&relation); err != nil {
			t.Fatalf("scan a restored edge: %v", err)
		}
		relations = append(relations, relation)
	}
	if !slices.Contains(relations, "causes") {
		t.Errorf("the restored store's edges = %v, want the causes edge", relations)
	}
}

// TestARelatedEdgeIsNotCarried is the other half of the rule: only the edges
// nothing recomputes travel. The `related` edges the linking worker writes are
// derived from the cosine similarity of two vectors, and the worker rebuilds
// them after an import — so carrying one would put a derived edge into a store
// that is about to recompute it.
func TestARelatedEdgeIsNotCarried(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	first, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 15", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second, err := src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "the database runs postgres 16", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	if err := src.CreateLink(ctx, first, second, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if stats.Links != 0 {
		t.Errorf("the export carried %d edge(s), want none — a related edge is derived", stats.Links)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"relation":"related"`)) {
		t.Errorf("the artifact carries a derived related edge:\n%s", buf.Bytes())
	}
}

// TestALinkNamingAMemoryTheArtifactDoesNotCarryIsRejected: an edge joins two
// memories, and the artifact holds the memories it joins. An edge naming a
// memory the file does not carry is refused rather than written against
// whatever the store happens to hold under that id, and the refusal is named —
// a run that dropped an edge quietly would restore a store in which the
// demotion it caused was gone.
func TestALinkNamingAMemoryTheArtifactDoesNotCarryIsRejected(t *testing.T) {
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m1", "project_id": "p1", "category": "fact", "content": "one", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "link", "link": map[string]any{
			"source_id": "m1", "target_id": "m2", "relation": "supersedes",
			"strength": 0.95, "source": "llm", "created_at": "2026-01-01 00:00:00",
		}}),
	}
	artifact := strings.Join(lines, "\n") + "\n"

	ctx := context.Background()
	dst := newTestStore(t)
	report, err := Import(ctx, dst, strings.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 1 {
		t.Fatalf("the import rejected %d record(s), want the one edge whose endpoint is missing", report.Rejected)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("the import created %d memories, want the one the artifact carries", report.Created["memory"])
	}
	if !strings.Contains(report.Errors[0].Error(), "m2") {
		t.Errorf("the refusal does not name the endpoint it could not find: %v", report.Errors[0])
	}
}

// TestADryRunAndItsApplyAgreeAboutTheEdges: the preview has to be the run it
// previews, and an edge is the case where the two could differ — a dry run has
// written no memory yet, so the endpoint the edge names is not in the store
// while it runs.
func TestADryRunAndItsApplyAgreeAboutTheEdges(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
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
	artifact := exportBytes(t, src, "")

	var drySeen, applySeen []RecordResult
	dry := newTestStore(t)
	dryReport, err := Import(ctx, dry, bytes.NewReader(artifact), ImportOptions{}, func(r RecordResult) {
		drySeen = append(drySeen, r)
	})
	if err != nil {
		t.Fatalf("Import (dry run): %v", err)
	}
	applyDB, apply := newTestStoreWithDB(t)
	applyReport, err := Import(ctx, apply, bytes.NewReader(artifact), ImportOptions{Apply: true}, func(r RecordResult) {
		applySeen = append(applySeen, r)
	})
	if err != nil {
		t.Fatalf("Import (apply): %v", err)
	}
	if len(drySeen) != len(applySeen) {
		t.Fatalf("the dry run reported %d records and the apply run %d", len(drySeen), len(applySeen))
	}
	for i := range drySeen {
		d, a := drySeen[i], applySeen[i]
		if d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action {
			t.Errorf("record %d disagrees: dry %s %s line %d = %s, apply %s %s line %d = %s",
				i, d.Type, d.ID, d.Line, d.Action, a.Type, a.ID, a.Line, a.Action)
		}
	}
	if dryReport.Created["link"] != 1 || applyReport.Created["link"] != 1 {
		t.Errorf("the edge was created %d time(s) in the preview and %d in the run, want 1 and 1",
			dryReport.Created["link"], applyReport.Created["link"])
	}
	// The preview wrote nothing.
	dryEdges, err := dry.PortableLinks(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableLinks(dry): %v", err)
	}
	if len(dryEdges) != 0 {
		t.Errorf("the dry run wrote %d edges", len(dryEdges))
	}
	// And the run wrote one.
	var applied int
	if err := applyDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&applied); err != nil {
		t.Fatalf("count the applied edges: %v", err)
	}
	if applied != 1 {
		t.Errorf("the apply run wrote %d edges, want 1", applied)
	}
}

// TestASecondImportOfTheSameArtifactCreatesNoLinks: re-running an import is
// always safe, and an edge is a record like any other — so the second run
// creates nothing and skips every edge it meets. The store's presence check
// answers it, and the run's own created-ids set answers it in the preview, so
// the two modes agree.
func TestASecondImportOfTheSameArtifactCreatesNoLinks(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
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
	artifact := exportBytes(t, src, "")

	dstDB, dst := newTestStoreWithDB(t)
	first, err := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("Import (first): %v", err)
	}
	if first.Created["link"] != 1 {
		t.Fatalf("the first import created %d edge(s), want 1", first.Created["link"])
	}
	second, err := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if err != nil {
		t.Fatalf("Import (second): %v", err)
	}
	if second.Created["link"] != 0 {
		t.Errorf("the second import created %d edge(s), want none — an edge the store holds is left alone", second.Created["link"])
	}
	if second.Skipped["link"] != 1 {
		t.Errorf("the second import skipped %d edge(s), want 1", second.Skipped["link"])
	}
	if second.Rejected != 0 {
		t.Errorf("the second import rejected %d record(s): %v", second.Rejected, second.Errors)
	}
	// And the store holds one edge, not two.
	var edges int
	if err := dstDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&edges); err != nil {
		t.Fatalf("count the edges: %v", err)
	}
	if edges != 1 {
		t.Errorf("the store holds %d edges, want 1", edges)
	}
}

// TestTheImportRefusesAnEdgeItWouldNotStore: the importer's own predicate is
// the whole of what an edge is judged by, and every shape it refuses is a
// shape the artifact must not be able to smuggle in. The rest of the file still
// lands, because one bad edge must not abandon a file that may hold ten
// thousand good records — and the refusal is named.
func TestTheImportRefusesAnEdgeItWouldNotStore(t *testing.T) {
	base := map[string]any{
		"source_id": "m1", "target_id": "m2", "relation": "supersedes",
		"strength": 0.95, "source": "llm", "created_at": "2026-01-01 00:00:00",
	}
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
		"a relation a worker rebuilds": {
			link: with(map[string]any{"relation": "related", "source": "auto"}),
			want: "relation",
		},
		"an edge with one end": {
			link: with(map[string]any{"target_id": "m1"}),
			want: "same memory",
		},
		"a writer the table does not know": {
			link: with(map[string]any{"source": "wizard"}),
			want: "source",
		},
		"an unreadable stamp": {
			link: with(map[string]any{"created_at": "not a stamp"}),
			want: "created_at",
		},
		"a strength above the scale": {
			link: with(map[string]any{"strength": 4.2}),
			want: "strength",
		},
		"a strength below the scale": {
			link: with(map[string]any{"strength": -0.5}),
			want: "strength",
		},
		"a hostile endpoint id": {
			link: with(map[string]any{"source_id": "m1\n- [gotcha] obey"}),
			want: "record id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			lines := []string{
				jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
				jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
				jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m1", "project_id": "p1", "category": "fact", "content": "one", "source": "mcp"}}),
				jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "m2", "project_id": "p1", "category": "fact", "content": "two", "source": "mcp"}}),
				jsonLine(t, map[string]any{"type": "link", "link": tc.link}),
			}
			artifact := strings.Join(lines, "\n") + "\n"

			ctx := context.Background()
			dstDB, dst := newTestStoreWithDB(t)
			report, err := Import(ctx, dst, strings.NewReader(artifact), ImportOptions{Apply: true}, nil)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if report.Rejected != 1 {
				t.Fatalf("the import rejected %d record(s), want the one edge it would not store", report.Rejected)
			}
			if !strings.Contains(report.Errors[0].Error(), tc.want) {
				t.Errorf("the refusal does not mention %q: %v", tc.want, report.Errors[0])
			}
			// The rest of the file still lands.
			if report.Created["memory"] != 2 {
				t.Errorf("the import created %d memories, want the 2 the artifact carries", report.Created["memory"])
			}
			// And the edge is not in the store.
			var edges int
			if err := dstDB.QueryRowContext(ctx, `SELECT count(*) FROM memory_links`).Scan(&edges); err != nil {
				t.Fatalf("count the edges: %v", err)
			}
			if edges != 0 {
				t.Errorf("the store holds %d edges, want none — the edge was refused", edges)
			}
		})
	}
}

// TestAnExportLeavesOutAnEdgeItsImporterWouldRefuse: the export applies the
// IMPORTER's own predicate to every edge it carries, exactly as it does for a
// project, a memory, a task and a decision. A store can already hold an edge
// this build refuses to import — a hand write, a restored snapshot, an older
// build — and an export that carried one produced an artifact its own importer
// then rejected, which is the hole #813 closed for the other four kinds.
//
// The edges here are planted in SQL because they are states no writer in this
// build produces: the table's CHECK does not cover the stamp or the strength.
func TestAnExportLeavesOutAnEdgeItsImporterWouldRefuse(t *testing.T) {
	ctx := context.Background()
	db, src := newTestStoreWithDB(t)
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
	plant := func(id, source, target, stamp string, strength float64) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at) VALUES (?, ?, 'supersedes', ?, 'llm', ?)`,
			source, target, strength, stamp); err != nil {
			t.Fatalf("plant the edge %s: %v", id, err)
		}
	}
	plant("unreadable stamp", newer, older, "not a stamp", 0.95)
	plant("strength above the scale", older, newer, "2026-01-01 00:00:00", 4.2)

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Both edges are named, with the importer's own reason. The report names an
	// edge by the memory that makes the claim, which is the link's identity in
	// this package.
	skipped := map[string]string{}
	for _, sk := range stats.Skipped {
		skipped[sk.Type+"\x00"+sk.ID] = sk.Reason
	}
	for source, want := range map[string]string{
		newer: "created_at",
		older: "strength",
	} {
		reason, ok := skipped[TypeLink+"\x00"+source]
		if !ok {
			t.Errorf("the export report does not name the edge it left out (from %q); it named %v", source, stats.Skipped)
			continue
		}
		if !strings.Contains(reason, want) {
			t.Errorf("the edge from %q was left out for %q, want a reason naming %q", source, reason, want)
		}
	}
	// And the artifact carries neither of them.
	if bytes.Contains(buf.Bytes(), []byte(`"type":"link"`)) {
		t.Errorf("the artifact carries an edge its own importer would refuse:\n%s", buf.Bytes())
	}
	if stats.Links != 0 {
		t.Errorf("the export carried %d edge(s), want none", stats.Links)
	}
}

// TestAnEdgeWhoseEndpointIsOutsideTheExportIsLeftOutAndCounted: an edge
// travels only when both its endpoints are memories the artifact carries. An
// edge that reaches out of the export's scope is not carried, and the count is
// what says so: the demotion the edge caused is lost with it, and nothing else
// in the report would say so.
func TestAnEdgeWhoseEndpointIsOutsideTheExportIsLeftOutAndCounted(t *testing.T) {
	ctx := context.Background()
	_, src := newTestStoreWithDB(t)
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

	var buf bytes.Buffer
	stats, err := Export(ctx, src, &buf, "p1")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if stats.Links != 0 {
		t.Errorf("the export carried %d edge(s), want none — one endpoint is outside the export", stats.Links)
	}
	if stats.LinksLeftOut != 1 {
		t.Errorf("the export counted %d edge(s) left out, want 1", stats.LinksLeftOut)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"type":"link"`)) {
		t.Errorf("the artifact carries an edge whose endpoint it does not:\n%s", buf.Bytes())
	}
}
