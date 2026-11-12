package mcpserver

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// the BEFORE picture for its migration (#581). It was the last reader in the tree
func goldenGlobalMemoriesStore(t *testing.T) *Server {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st := memory.NewStore(db, logger)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	stamps := make([]string, 24)
	for i := range stamps {
		stamps[i] = "2026-01-" + twoDigits(i+2) + " 03:04:05"
	}
	none := `'[]'`
	rows := []struct {
		cat    string
		imp    float64
		pinned bool
		tags   string
		scope  string
		score  float64
	}{
		{"preference", 1.00, true, `'["golden","pinned"]'`, "NULL", 1.000}, // g00 pinned, tagged
		{"convention", 0.96, false, none, "NULL", 0.960},                   // g01
		{"fact", 0.92, false, none, `'{"area":"payments"}'`, 0.920},        // g02 scoped
		{"preference", 0.88, false, none, "NULL", 0.880},                   // g03 the near-duplicate winner
		{"convention", 0.84, false, none, "NULL", 0.840},                   // g04
		{"preference", 0.80, false, none, "NULL", 0.800},                   // g05 the supersede winner
		{"preference", 0.62, false, none, "NULL", 0.620},                   // g06
		{"convention", 0.45, false, none, "NULL", 0.450},                   // g07 the lowest row decay cannot reach
		{"architecture", 0.90, false, none, "NULL", 0.270},                 // g08 45-day floor
		{"pattern", 0.86, false, none, "NULL", 0.258},                      // g09 45-day floor
		{"pattern", 0.82, false, none, "NULL", 0.246},                      // g10 the supersede casualty
		{"architecture", 0.60, false, none, "NULL", 0.180},                 // g11 45-day floor
		{"gotcha", 1.00, false, none, "NULL", 0.150},                       // g12 the cost decay does to the fixture
		{"decision", 0.98, false, none, "NULL", 0.147},                     // g13 30-day floor
		{"dependency", 0.96, false, none, "NULL", 0.144},                   // g14 the near-duplicate casualty
		{"gotcha", 0.90, false, none, "NULL", 0.135},                       // g15 30-day floor
		{"dependency", 0.88, false, none, "NULL", 0.132},                   // g16 30-day floor; the answer's only surviving dependency, since g14 is demoted out
		{"pattern", 0.30, false, none, "NULL", 0.090},                      // g17 below the cap
		{"architecture", 0.28, false, none, "NULL", 0.084},                 // g18 below the cap
		{"gotcha", 0.26, false, none, "NULL", 0.039},                       // g19 below the cap
		{"decision", 0.24, false, none, "NULL", 0.036},                     // g20 below the cap
		{"dependency", 0.22, false, none, "NULL", 0.033},                   // g21 below the cap
	}
	// `assemble.Data`, the one implementation, since #911). gmem06 takes the non-manual source,
	type goldenExtra struct {
		agent, srcRef, source string
		conf                  float64
		content               string
	}
	extras := map[int]goldenExtra{
		4: {
			agent: "claude-code", srcRef: "ghost#872", conf: 0.8,
			content: "global memory 04 holds «a» rule and a <<literal>>",
		},
		6: {source: "mcp"},
	}
	defaultContent := func(i int) string {
		return "global memory " + twoDigits(i) + " content for the global memories golden"
	}
	for i, r := range rows {
		id := "gmem" + twoDigits(i)
		x := extras[i]
		content := x.content
		if content == "" {
			content = defaultContent(i)
		}
		source := x.source
		if source == "" {
			source = "manual"
		}
		var agent, srcRef any
		var conf any
		if x.agent != "" {
			agent = x.agent
		}
		if x.srcRef != "" {
			srcRef = x.srcRef
		}
		if x.conf != 0 {
			conf = x.conf
		}
		_, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, tags, scope, agent, source_ref, confidence, created_at, updated_at)
		                   VALUES (?, '_global', ?, ?, ?, ?, ?, `+r.tags+`, `+r.scope+`, ?, ?, ?, ?, ?)`,
			id, r.cat, content, source, r.imp, boolToInt(r.pinned), agent, srcRef, conf, stamps[i], stamps[i])
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, resolved_at, created_at, updated_at)
	                     VALUES ('gmemres', '_global', 'preference', 'a resolved global memory', 'manual', 1.0, ?, ?, ?)`,
		stamps[0], stamps[0], stamps[0]); err != nil {
		t.Fatalf("insert resolved global memory: %v", err)
	}
	for _, e := range []struct{ from, to, rel string }{
		{"gmem05", "gmem10", "supersedes"},
		{"gmem03", "gmem14", "duplicate"},
	} {
		if err := st.CreateLink(ctx, e.from, e.to, e.rel, 1, "manual"); err != nil {
			t.Fatalf("link %s %s->%s: %v", e.rel, e.from, e.to, err)
		}
	}
	return New(st, logger, "test")
}
func renderGlobalMemoriesResource(t *testing.T, srv *Server) string {
	t.Helper()
	rr, err := connectedClient(t, srv).ReadResource(context.Background(), &mcp.ReadResourceParams{
		URI: "ghost://memories/global",
	})
	if err != nil {
		t.Fatalf("ReadResource ghost://memories/global: %v", err)
	}
	if len(rr.Contents) != 1 {
		t.Fatalf("expected 1 resource content, got %d", len(rr.Contents))
	}
	return rr.Contents[0].Text
}
func TestGlobalMemoriesResourceGolden(t *testing.T) {
	want := readGlobalMemoriesGolden(t, "globalmem_resource.golden")
	got := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	if got != want {
		t.Errorf("ghost://memories/global changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
func TestGlobalMemoriesGoldenFixtureIsStableAcrossRuns(t *testing.T) {
	first := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	second := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	if first != second {
		t.Fatalf("the global-memories golden fixture is not deterministic across stores.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
func TestGlobalMemoriesGoldenFixtureExercisesItsBranches(t *testing.T) {
	out := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	for _, want := range []string{
		"## Ghost Global Memories",           // the surface's own heading
		"- [preference] `gmem",               // the listing shape
		"[pinned]",                           // the pin exemption and its label
		`tags:["golden","pinned"]`,           // the tag label
		"scope{area=payments}",               // the scope label
		"confidence 0.8",                     // the confidence label
		"agent=«claude-code»",                // the agent label, delimited as stored text
		"source_ref=«ghost#872»",             // the source reference label
		"`gmem06` (0.6 source=mcp)",          // the ORIGIN label, on a non-manual row
		"«global memory 04 holds <<a>> rule", // « and » neutralised to << and >>
		"and a <<literal>>»",                 // and a literal << NOT escaped a second time
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fixture does not exercise %q; the answer is:\n%s", want, out)
		}
	}
	for _, cat := range []string{"preference", "convention", "fact", "gotcha", "decision", "dependency", "architecture", "pattern"} {
		if !strings.Contains(out, "- ["+cat+"] `gmem") {
			t.Errorf("no %s global row reached the answer, so its decay group is untested:\n%s", cat, out)
		}
	}
	if decayed, undamped := strings.Index(out, "`gmem07`"), strings.Index(out, "`gmem12`"); decayed < 0 || undamped < 0 || decayed > undamped {
		t.Errorf("gmem07 (convention, importance 0.45, no decay) must rank above gmem12 (gotcha, importance 1.00, 30-day floor 0.15); "+
			"gotcha at %d, convention at %d:\n%s", undamped, decayed, out)
	}
	if strings.Contains(out, "a resolved global memory") {
		t.Errorf("the resolved global reached the answer; resolved_at IS NULL is not doing what the golden assumes:\n%s", out)
	}
	for _, absent := range []string{"global memory 10 content", "global memory 14 content"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q is in the answer, so its demotion is not observable and the golden does not test it:\n%s", absent, out)
		}
	}
	for _, present := range []string{"global memory 15 content", "global memory 16 content"} {
		if !strings.Contains(out, present) {
			t.Errorf("%q was admitted only because the demotions pushed a row out, so the demotions are not observable:\n%s", present, out)
		}
	}
	for _, present := range []string{"global memory 05 content", "global memory 03 content"} {
		if !strings.Contains(out, present) {
			t.Errorf("the surviving endpoint of a demotion pair (%q) is missing from the answer:\n%s", present, out)
		}
	}
	if n := strings.Count(out, "\n- ["); n != globalMemoriesLimit {
		t.Errorf("the answer carries %d memory lines, so the %d-cap is not binding and the budget stage is untested:\n%s",
			n, globalMemoriesLimit, out)
	}
}
func readGlobalMemoriesGolden(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(raw)
}
func TestRecordGlobalMemoriesGolden(t *testing.T) {
	if os.Getenv("GHOST_RECORD_GOLDEN") == "" {
		t.Skip("set GHOST_RECORD_GOLDEN=1 to re-record; this is a reviewable decision, not a step")
	}
	name := filepath.Join("testdata", "globalmem_resource.golden")
	if _, err := os.Stat(name); err == nil {
		t.Fatalf("%s already exists; delete it deliberately and read the diff", name)
	}
	got := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	if err := os.WriteFile(name, []byte(got), 0o644); err != nil { //nolint:gosec // a testdata baseline, not a secret
		t.Fatalf("write %s: %v", name, err)
	}
	fmt.Printf("recorded %s (%d bytes)\n", name, len(got))
}
