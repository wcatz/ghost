package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func goldenProjectContextStore(t *testing.T) *Server {
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
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('pcgold', '/tmp/pcgold', 'pcgold')`); err != nil {
		t.Fatalf("insert pcgold project: %v", err)
	}

	stamps := make([]string, 22)
	for i := range stamps {
		stamps[i] = "2026-01-" + twoDigits(i+2) + " 03:04:05"
	}

	type row struct {
		cat    string
		imp    float64
		pinned bool
		tags   string
		scope  string
	}
	none := `'[]'`
	rows := []row{
		{"preference", 0.99, true, `'["golden","pinned"]'`, "NULL"}, // 00 pinned, tagged
		{"convention", 0.98, false, none, "NULL"},                   // 01
		{"fact", 0.97, false, none, `'{"area":"payments"}'`},        // 02 scoped
		{"architecture", 0.96, false, none, "NULL"},                 // 03
		{"pattern", 0.95, false, none, "NULL"},                      // 04
		{"preference", 0.94, false, none, "NULL"},                   // 05
		{"convention", 0.93, false, none, "NULL"},                   // 06
		{"fact", 0.92, false, none, "NULL"},                         // 07
		{"architecture", 0.91, false, none, "NULL"},                 // 08
		{"pattern", 0.90, false, none, "NULL"},                      // 09
		{"gotcha", 1.00, false, none, "NULL"},                       // 10 the supersede survivor
		{"decision", 0.99, false, none, "NULL"},                     // 11
		{"preference", 0.84, false, none, "NULL"},                   // 12
		{"convention", 0.83, false, none, "NULL"},                   // 13
		{"fact", 0.82, false, none, "NULL"},                         // 14
		{"gotcha", 0.90, false, none, "NULL"},                       // 15 the supersede casualty
		{"decision", 0.80, false, none, "NULL"},                     // 16
		{"pattern", 0.78, false, none, "NULL"},                      // 17
	}
	for i, r := range rows {
		id := "pmem" + twoDigits(i)
		_, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, tags, scope, created_at, updated_at)
		                   VALUES (?, 'pcgold', ?, ?, 'manual', ?, ?, `+r.tags+`, `+r.scope+`, ?, ?)`,
			id, r.cat, "project memory "+twoDigits(i)+" content for the project context golden",
			r.imp, boolToInt(r.pinned), stamps[i], stamps[i])
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, resolved_at, created_at, updated_at)
	                     VALUES ('pmemres', 'pcgold', 'preference', 'a resolved project memory', 'manual', 1.0, ?, ?, ?)`,
		stamps[0], stamps[0], stamps[0]); err != nil {
		t.Fatalf("insert resolved project memory: %v", err)
	}

	for i := 0; i < 20; i++ {
		id := "gmem" + twoDigits(i)
		imp := 0.100 - float64(i)*0.001
		_, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
		                   VALUES (?, '_global', 'preference', ?, 'manual', ?, ?, ?)`,
			id, "global memory "+twoDigits(i)+" content for the project context golden",
			imp, stamps[i], stamps[i])
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
		{"pmem10", "pmem15", "supersedes"},
		{"gmem03", "gmem12", "duplicate"},
	} {
		if err := st.CreateLink(ctx, e.from, e.to, e.rel, 1, "manual"); err != nil {
			t.Fatalf("link %s %s->%s: %v", e.rel, e.from, e.to, err)
		}
	}
	return New(st, logger, "test")
}

func twoDigits(i int) string {
	const digits = "0123456789"
	return string([]byte{digits[(i/10)%10], digits[i%10]})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func renderProjectContextTool(t *testing.T, srv *Server) string {
	t.Helper()
	return resultText(callTool(t, connectedClient(t, srv), "ghost_project_context", map[string]any{
		"project_id": "pcgold",
	}))
}

func renderProjectContextResource(t *testing.T, srv *Server) string {
	t.Helper()
	text, err := srv.buildProjectContext(context.Background(), "pcgold", "", searchResponseMaxBytes)
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	return text
}

func TestProjectContextToolBlockGolden(t *testing.T) {
	want := readProjectContextGolden(t, "projectctx_tool.golden")
	got := renderProjectContextTool(t, goldenProjectContextStore(t))
	if got != want {
		t.Errorf("ghost_project_context changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func TestProjectContextResourceBlockGolden(t *testing.T) {
	want := readProjectContextGolden(t, "projectctx_resource.golden")
	got := renderProjectContextResource(t, goldenProjectContextStore(t))
	if got != want {
		t.Errorf("ghost://project/{id}/context changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func readProjectContextGolden(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(raw)
}
