package mcpinit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

func TestOverdueExcludesOverLimitProjects(t *testing.T) {
	statusEnv(t)
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "normal", "/tmp/normal", "normal"); err != nil {
		t.Fatalf("EnsureProject normal: %v", err)
	}
	if err := store.EnsureProject(ctx, "over", "/tmp/over", "over"); err != nil {
		t.Fatalf("EnsureProject over: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, "normal", memory.Memory{Category: "fact", Content: fmt.Sprintf("n %d", i), Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("create normal: %v", err)
		}
	}

	// Seed many consolidatable rows for over-limit project
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < reflection.MaxConsolidationInput+1; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			"over", fmt.Sprintf("over %05d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	overdue, err := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	var foundNormal, foundOver bool
	for _, o := range overdue {
		if o.ID == "normal" {
			foundNormal = true
		}
		if o.ID == "over" {
			foundOver = true
		}
	}
	if !foundNormal {
		t.Fatalf("normal project should be overdue, got %v", overdue)
	}
	if foundOver {
		t.Fatalf("over-limit project should not be in overdue list, got %v", overdue)
	}

	text, ok := FormatOverdueMaintenance(overdue, time.Now())
	if !ok {
		t.Fatalf("FormatOverdueMaintenance returned not ok")
	}
	if !strings.Contains(text, "ghost lifecycle --project") {
		t.Fatalf("expected lifecycle retry in overdue text: %q", text)
	}
	if strings.Contains(text, "resolve") || strings.Contains(text, "prune") {
		t.Fatalf("over-limit guidance should not appear in maintenance overdue: %q", text)
	}
	if strings.Contains(text, "(over)") || strings.Contains(text, "over (") {
		t.Fatalf("over-limit project should not be named in overdue: %q", text)
	}
}

func TestOverdueBoundaryExactlyAtLimit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dataDir, _ := config.DataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "at-limit", "/tmp/at-limit", "at-limit"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "over-limit", "/tmp/over-limit", "over-limit"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < reflection.MaxConsolidationInput; i++ {
		if _, err := tx.Exec(`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at) VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			"at-limit", fmt.Sprintf("a%05d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert at-limit %d: %v", i, err)
		}
	}
	for i := 0; i < reflection.MaxConsolidationInput+1; i++ {
		if _, err := tx.Exec(`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at) VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			"over-limit", fmt.Sprintf("b%05d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert over-limit %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	overdue, err := OverdueMaintenanceProjects(ctx, store, dataDir, time.Now(), 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	foundAtLimit, foundOver := false, false
	for _, o := range overdue {
		if o.ID == "at-limit" {
			foundAtLimit = true
		}
		if o.ID == "over-limit" {
			foundOver = true
		}
	}
	if !foundAtLimit {
		t.Fatalf("exactly at limit should be overdue")
	}
	if foundOver {
		t.Fatalf("over limit should not be overdue")
	}
}

func TestOverdueExcludesOverLimitAppearsInReflectStatusAndHealth(t *testing.T) {
	// Over-limit project missing from overdue must appear in reflect over-bound blocks
	statusEnv(t)
	dataHome := os.Getenv("XDG_DATA_HOME")
	ghostDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj-over", "/tmp/proj-over", "over-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// seed over limit
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < reflection.MaxConsolidationInput+1; i++ {
		if _, err := tx.Exec(`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at) VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			"proj-over", fmt.Sprintf("r%05d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert proj-over %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// overdue must not list it
	overdue, err := OverdueMaintenanceProjects(ctx, store, ghostDir, time.Now(), 30*time.Minute, true)
	if err != nil {
		t.Fatalf("OverdueMaintenanceProjects: %v", err)
	}
	for _, o := range overdue {
		if o.ID == "proj-over" {
			t.Fatalf("over-limit project should be excluded from overdue")
		}
	}

	// status reflect block must list it
	var buf bytes.Buffer
	reportReflectOverBound(&buf, store)
	out := buf.String()
	if !strings.Contains(out, "over-project") && !strings.Contains(out, "proj-over") {
		t.Fatalf("reflect over-bound missing project in status: %s", out)
	}
	if !strings.Contains(out, "resolve") || !strings.Contains(out, "prune") {
		t.Fatalf("reflect over-bound missing fix in status: %s", out)
	}

	// ghost_health Reflect over-bound block must also list it
	// Use the same predicate as mcpserver ghost_health (count via store view)
	var hbuf bytes.Buffer
	counts := map[string]int{}
	rows, err := db.QueryContext(ctx, `SELECT project_id, COUNT(*) FROM memories WHERE pinned=0 AND source NOT IN ('manual','builtin') AND retention != 'persistent' AND resolved_at IS NULL AND project_id != ? GROUP BY project_id`, memory.GlobalProjectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var pid string
		var c int
		if err := rows.Scan(&pid, &c); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		counts[pid] = c
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	type ob struct {
		name string
		id   string
		c    int
	}
	var obs []ob
	if n := counts["proj-over"]; n > reflection.MaxConsolidationInput {
		obs = append(obs, ob{name: "over-project", id: "proj-over", c: n})
	}
	if len(obs) == 0 {
		t.Fatalf("expected over-bound project for ghost_health")
	}
	fmt.Fprintf(&hbuf, "**Reflect:** %d project(s) above the %d consolidatable-memory limit for a single consolidation and skipped by unattended reflect — to consolidate, bring to at most %d: `ghost resolve <project> --apply` or `ghost prune --project <project> --apply` (pinned, manual and persistent memories do not count). Then run `ghost reflect <project> --apply`.\n", len(obs), reflection.MaxConsolidationInput, reflection.MaxConsolidationInput)
	for _, o := range obs {
		fmt.Fprintf(&hbuf, "- %s (%s): %d consolidatable memories\n", o.name, o.id, o.c)
	}
	hout := hbuf.String()
	if !strings.Contains(hout, "over-project") && !strings.Contains(hout, "proj-over") {
		t.Fatalf("reflect over-bound missing project in ghost_health: %s", hout)
	}
	if !strings.Contains(hout, "ghost resolve") || !strings.Contains(hout, "ghost prune") {
		t.Fatalf("reflect over-bound missing fix in ghost_health: %s", hout)
	}
}
