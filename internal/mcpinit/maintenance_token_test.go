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

const overdueTokenFixture = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

func TestOverdueDiagnosticsWithholdTokenShapedProjectValues(t *testing.T) {
	now := time.Now()
	overdue := []OverdueProject{
		{ID: overdueTokenFixture, Name: "tokname", Count: 1, HasLastRun: false, MinInterval: 0},
		{ID: "ordinary-id", Name: overdueTokenFixture, Count: 1, HasLastRun: false, MinInterval: 0},
		{ID: "ordinary-id-2", Name: "ordinary-name", Count: 1, HasLastRun: false, MinInterval: 0},
	}
	lines := overdueDiagnosticLines(overdue, now)
	text := strings.Join(lines, "\n")
	if strings.Contains(text, overdueTokenFixture) {
		t.Errorf("overdueDiagnosticLines printed a token-shaped project value whole:\n%s", text)
	}
	if !strings.Contains(text, "withheld") {
		t.Errorf("overdueDiagnosticLines did not withhold the token-shaped value:\n%s", text)
	}
	if !strings.Contains(text, "tokname") {
		t.Errorf("overdueDiagnosticLines lost an ordinary name:\n%s", text)
	}
	if !strings.Contains(text, "ordinary-name") || !strings.Contains(text, "ordinary-id-2") {
		t.Errorf("overdueDiagnosticLines changed an ordinary project:\n%s", text)
	}

	unsafe := []OverdueProject{
		{ID: overdueTokenFixture, Name: "unsafe-name", Count: 1, UnsafeID: true},
		{ID: "ordinary-unsafe-id", Name: overdueTokenFixture, Count: 1, UnsafeID: true},
	}
	lines = unmaintainableDiagnosticLines(unsafe)
	text = strings.Join(lines, "\n")
	if strings.Contains(text, overdueTokenFixture) {
		t.Errorf("unmaintainableDiagnosticLines printed a token-shaped project value whole:\n%s", text)
	}
	if !strings.Contains(text, "withheld") {
		t.Errorf("unmaintainableDiagnosticLines did not withhold the token-shaped value:\n%s", text)
	}
	if !strings.Contains(text, "ordinary-unsafe-id") {
		t.Errorf("unmaintainableDiagnosticLines changed an ordinary project:\n%s", text)
	}
}

func TestReflectionExclusionsWithholdTokenShapedProjectValues(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll home: %v", err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, overdueTokenFixture, "/tmp/tok-id", "tok-id-name"); err != nil {
		t.Fatalf("EnsureProject token id: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)`, "nameproj", "/tmp/nameproj", overdueTokenFixture, ""); err != nil {
		t.Fatalf("plant token-name project: %v", err)
	}
	if err := store.EnsureProject(ctx, "ordinary-proj", "/tmp/ordinary-proj", "ordinary-name"); err != nil {
		t.Fatalf("EnsureProject ordinary: %v", err)
	}
	for _, id := range []string{overdueTokenFixture, "nameproj", "ordinary-proj"} {
		if _, err := store.Create(ctx, id, memory.Memory{Category: "fact", Content: "exclusion row", Importance: 0.5, Source: "mcp"}); err != nil {
			t.Fatalf("Create %q: %v", id, err)
		}
	}
	var buf bytes.Buffer
	reportReflectionExclusions(&buf, store)
	out := buf.String()
	if strings.Contains(out, overdueTokenFixture) {
		t.Errorf("reportReflectionExclusions printed a token-shaped project value whole:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("reportReflectionExclusions did not withhold the token-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "ordinary-name") || !strings.Contains(out, "ordinary-proj") {
		t.Errorf("reportReflectionExclusions changed an ordinary project:\n%s", out)
	}
}

func TestReflectOverBoundWithholdsTokenShapedProjectID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	setHome(t, filepath.Join(tmp, "home"))
	t.Setenv("GHOST_REFLECTION_AUTO_REFLECT", "true")
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0700); err != nil {
		t.Fatalf("MkdirAll home: %v", err)
	}
	dataDir, err := config.DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("MkdirAll data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, overdueTokenFixture, "/tmp/tok-over", "tok2name"); err != nil {
		t.Fatalf("EnsureProject token id: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < reflection.MaxConsolidationInput+1; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			overdueTokenFixture, fmt.Sprintf("relay %05d listens on its own port", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var buf bytes.Buffer
	reportReflectOverBound(&buf, store)
	out := buf.String()
	if strings.Contains(out, overdueTokenFixture) {
		t.Errorf("reportReflectOverBound printed a token-shaped project id whole:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("reportReflectOverBound did not withhold the token-shaped id:\n%s", out)
	}
	if !strings.Contains(out, "tok2name") {
		t.Errorf("reportReflectOverBound lost an ordinary name:\n%s", out)
	}
}
