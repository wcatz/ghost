package simulation_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// ── Scenario data ──────────────────────────────────────────────────────────

type simDay struct {
	day      int
	memories []memory.Memory
}

func tags(t ...string) []string { return t }

// scenario returns 30 days of realistic developer activity on a Go web project.
// Intentionally includes near-duplicates, evolving understanding, global facts,
// and noisy low-value memories to stress-test dedup + consolidation.
func scenario() []simDay {
	return []simDay{
		// === Week 1: Project setup, initial discoveries ===
		{day: 1, memories: []memory.Memory{
			{Category: "architecture", Content: "Project uses Go 1.24 with Chi router, SQLite for persistence, and HTMX for frontend interactivity", Importance: 0.9, Source: "chat", Tags: tags("go", "chi", "sqlite", "htmx")},
			{Category: "convention", Content: "All handlers live in internal/handler/ with one file per resource (users.go, posts.go, etc)", Importance: 0.7, Source: "chat", Tags: tags("handler", "convention")},
			{Category: "dependency", Content: "Using modernc.org/sqlite pure-Go driver, not mattn/go-sqlite3 which requires CGO", Importance: 0.8, Source: "chat", Tags: tags("sqlite", "driver")},
		}},
		{day: 2, memories: []memory.Memory{
			{Category: "architecture", Content: "Database migrations are embedded via go:embed in internal/db/schema.go and run on startup", Importance: 0.8, Source: "chat", Tags: tags("migration", "embed")},
			{Category: "pattern", Content: "Error handling pattern: wrap with fmt.Errorf and %w verb, return to handler which maps to HTTP status", Importance: 0.6, Source: "chat", Tags: tags("errors", "pattern")},
			{Category: "fact", Content: "CI runs on GitHub Actions with build-and-test job, uses golangci-lint", Importance: 0.5, Source: "chat", Tags: tags("ci", "github-actions")},
		}},
		{day: 3, memories: []memory.Memory{
			{Category: "preference", Content: "User prefers table-driven tests with subtests using t.Run, not separate test functions", Importance: 0.7, Source: "chat", Tags: tags("testing", "preference")},
			{Category: "convention", Content: "Commit messages follow conventional commits: feat(), fix(), chore(), docs:", Importance: 0.8, Source: "chat", Tags: tags("git", "commits")},
		}},
		{day: 4, memories: []memory.Memory{
			{Category: "gotcha", Content: "SQLite RETURNING clause requires modernc.org/sqlite v1.29+ — earlier versions silently return empty", Importance: 0.9, Source: "chat", Tags: tags("sqlite", "gotcha")},
			{Category: "architecture", Content: "Config loaded from YAML file with envconfig overlay: config.yaml → env vars → CLI flags", Importance: 0.7, Source: "chat", Tags: tags("config", "yaml")},
		}},
		{day: 5, memories: []memory.Memory{
			{Category: "pattern", Content: "All database queries use context.Context for cancellation, passed from HTTP request context", Importance: 0.6, Source: "chat", Tags: tags("context", "database")},
			{Category: "decision", Content: "Chose Chi over Gin because Chi is stdlib-compatible (net/http handlers) and has no global state", Importance: 0.8, Source: "chat", Tags: tags("chi", "gin", "router")},
			{Category: "fact", Content: "Dev server runs on port 8080, production behind Caddy reverse proxy on port 443", Importance: 0.5, Source: "chat", Tags: tags("server", "caddy")},
		}},

		// === Week 2: Feature development, first bugs ===
		{day: 8, memories: []memory.Memory{
			{Category: "architecture", Content: "Authentication uses session cookies stored in SQLite sessions table, 24h expiry with rolling refresh", Importance: 0.9, Source: "chat", Tags: tags("auth", "session", "cookies")},
			{Category: "gotcha", Content: "Chi middleware order matters: Logger must come before Recoverer, Auth before route handlers", Importance: 0.8, Source: "chat", Tags: tags("chi", "middleware", "order")},
			// Near-duplicate of day 1 architecture — should merge
			{Category: "architecture", Content: "The project uses Go with Chi router for HTTP, SQLite for data persistence, HTMX on the frontend", Importance: 0.7, Source: "chat", Tags: tags("go", "chi", "sqlite")},
		}},
		{day: 9, memories: []memory.Memory{
			{Category: "pattern", Content: "Repository pattern: each domain has a Store interface in internal/domain/ implemented by SQLite in internal/db/", Importance: 0.7, Source: "chat", Tags: tags("repository", "interface")},
			{Category: "gotcha", Content: "HTMX hx-swap='innerHTML' breaks event listeners — must use hx-swap='morph' or re-attach in htmx:afterSwap", Importance: 0.8, Source: "chat", Tags: tags("htmx", "swap", "events")},
		}},
		{day: 10, memories: []memory.Memory{
			{Category: "dependency", Content: "HTMX v2.0.4 loaded from /static/htmx.min.js, not CDN — for offline dev and version pinning", Importance: 0.6, Source: "chat", Tags: tags("htmx", "static")},
			{Category: "decision", Content: "Decided to use server-side rendering with html/template instead of a JS framework — simpler stack, faster loads", Importance: 0.7, Source: "chat", Tags: tags("ssr", "template", "decision")},
			// Duplicate preference — should strengthen
			{Category: "preference", Content: "User prefers table-driven tests using t.Run subtests over individual test functions", Importance: 0.6, Source: "chat", Tags: tags("testing")},
		}},
		{day: 11, memories: []memory.Memory{
			{Category: "gotcha", Content: "html/template auto-escapes but NOT in <script> tags — must use json.Marshal for inline JS data", Importance: 0.9, Source: "chat", Tags: tags("template", "xss", "security")},
			{Category: "pattern", Content: "Error handling: wrap errors with context using fmt.Errorf('verb noun: %w', err), handlers map to HTTP codes", Importance: 0.6, Source: "chat", Tags: tags("errors")},
		}},
		{day: 12, memories: []memory.Memory{
			{Category: "architecture", Content: "Background job runner uses a goroutine pool with buffered channels, graceful shutdown via context cancellation", Importance: 0.8, Source: "chat", Tags: tags("goroutine", "jobs", "shutdown")},
			{Category: "fact", Content: "Production database is at /var/lib/myapp/data.db, dev uses ./data/dev.db relative to project root", Importance: 0.5, Source: "chat", Tags: tags("database", "paths")},
		}},

		// === Week 3: Deeper work, more patterns, some global knowledge ===
		{day: 15, memories: []memory.Memory{
			{Category: "gotcha", Content: "SQLite WAL mode required for concurrent reads during writes — set via PRAGMA journal_mode=WAL on open", Importance: 0.9, Source: "chat", Tags: tags("sqlite", "wal", "concurrency")},
			// Global-scoped: user convention across all projects
			{Category: "preference", Content: "Always use feature branches and PRs, never commit directly to main across all projects", Importance: 1.0, Source: "chat", Tags: tags("git", "workflow")},
		}},
		{day: 16, memories: []memory.Memory{
			{Category: "architecture", Content: "Email notifications use a queue table + background worker, not synchronous sends in handlers", Importance: 0.7, Source: "chat", Tags: tags("email", "queue", "async")},
			{Category: "pattern", Content: "All SQL queries use parameterized statements via ?, never string concatenation — prevents SQL injection", Importance: 0.8, Source: "chat", Tags: tags("sql", "security")},
		}},
		{day: 17, memories: []memory.Memory{
			// Near-duplicate of day 4 config architecture
			{Category: "architecture", Content: "Configuration layering: YAML file is base, environment variables override, CLI flags override both", Importance: 0.7, Source: "chat", Tags: tags("config")},
			{Category: "gotcha", Content: "Chi URL params via chi.URLParam(r, 'id') return empty string (not error) for missing params — must validate", Importance: 0.7, Source: "chat", Tags: tags("chi", "params")},
		}},
		{day: 18, memories: []memory.Memory{
			{Category: "decision", Content: "Chose SQLite over Postgres for simplicity — single binary deployment, no separate database server needed", Importance: 0.8, Source: "chat", Tags: tags("sqlite", "postgres", "decision")},
			{Category: "dependency", Content: "Using slog (Go 1.21+) for structured logging instead of zerolog or zap — stdlib preference", Importance: 0.6, Source: "chat", Tags: tags("logging", "slog")},
		}},
		{day: 19, memories: []memory.Memory{
			{Category: "pattern", Content: "Middleware chain: Logger → Recoverer → CORS → Auth → RateLimit → handler. Order is load-bearing.", Importance: 0.8, Source: "chat", Tags: tags("middleware", "chain")},
			{Category: "fact", Content: "Rate limiter uses token bucket at 100 req/min per IP, stored in-memory with sync.Map", Importance: 0.6, Source: "chat", Tags: tags("ratelimit", "tokenbucket")},
			// Global knowledge: SSH access
			{Category: "fact", Content: "Production server SSH: ssh deploy@prod.example.com — used for all projects' deployments", Importance: 0.7, Source: "chat", Tags: tags("ssh", "deploy")},
		}},

		// === Week 4: Maturity, refactoring, knowledge solidifies ===
		{day: 22, memories: []memory.Memory{
			{Category: "architecture", Content: "API versioning via URL prefix /api/v1/ — v2 will coexist, not replace", Importance: 0.7, Source: "chat", Tags: tags("api", "versioning")},
			// Evolved understanding — should replace earlier simpler version
			{Category: "architecture", Content: "Auth flow: login POST → bcrypt verify → create session row → set HttpOnly Secure cookie → 302 redirect. Session table has user_id FK, expires_at, created_at.", Importance: 0.9, Source: "chat", Tags: tags("auth", "session", "bcrypt")},
		}},
		{day: 23, memories: []memory.Memory{
			{Category: "gotcha", Content: "go:embed directive must be in the same package as the embedded files — can't embed from parent directory", Importance: 0.7, Source: "chat", Tags: tags("embed", "gotcha")},
			{Category: "pattern", Content: "Repository interface pattern: domain defines Store interface, internal/db implements it, handler depends on interface", Importance: 0.7, Source: "chat", Tags: tags("repository", "interface", "di")},
		}},
		{day: 24, memories: []memory.Memory{
			{Category: "decision", Content: "Decided against adding Redis — SQLite handles current traffic (< 1000 rps), adding Redis would complicate deployment", Importance: 0.7, Source: "chat", Tags: tags("redis", "sqlite", "scaling")},
			{Category: "convention", Content: "All handler functions follow signature: func(w http.ResponseWriter, r *http.Request) — stdlib compatible", Importance: 0.6, Source: "chat", Tags: tags("handler", "signature")},
		}},
		{day: 25, memories: []memory.Memory{
			// Duplicate of existing Chi middleware gotcha
			{Category: "gotcha", Content: "Chi middleware ordering is critical — Logger must be outermost, Auth must wrap protected routes only", Importance: 0.8, Source: "chat", Tags: tags("chi", "middleware")},
			{Category: "fact", Content: "Test coverage target is 70%, currently at 65%. Missing coverage mostly in email worker and auth middleware.", Importance: 0.5, Source: "chat", Tags: tags("testing", "coverage")},
		}},
		{day: 26, memories: []memory.Memory{
			{Category: "architecture", Content: "Graceful shutdown: main() listens for SIGINT/SIGTERM, cancels root context, waits for HTTP server and job worker", Importance: 0.8, Source: "chat", Tags: tags("shutdown", "signal")},
			{Category: "pattern", Content: "Database transactions: store methods that touch multiple tables accept *sql.Tx, callers manage begin/commit/rollback", Importance: 0.7, Source: "chat", Tags: tags("transactions", "database")},
		}},

		// === Days 28-30: Late additions, some noise ===
		{day: 28, memories: []memory.Memory{
			{Category: "fact", Content: "Docker image is 12MB using multi-stage build: Go builder → scratch with just the binary + migrations", Importance: 0.6, Source: "chat", Tags: tags("docker", "image")},
			{Category: "dependency", Content: "Using modernc.org/sqlite v1.34.0 — pure Go, no CGO required, works on ARM64 and cross-compiles", Importance: 0.7, Source: "chat", Tags: tags("sqlite", "version")},
		}},
		{day: 29, memories: []memory.Memory{
			// Near-duplicate of multiple earlier architecture memories
			{Category: "architecture", Content: "Tech stack: Go 1.24, Chi router, SQLite (modernc), HTMX frontend, html/template SSR", Importance: 0.6, Source: "chat", Tags: tags("stack", "overview")},
			{Category: "gotcha", Content: "Must call rows.Close() after database queries or SQLite connection pool exhausts — use defer immediately after QueryContext", Importance: 0.8, Source: "chat", Tags: tags("sqlite", "rows", "leak")},
		}},
		{day: 30, memories: []memory.Memory{
			{Category: "preference", Content: "User always prefers to review diffs before committing — never auto-commit without showing changes", Importance: 0.8, Source: "chat", Tags: tags("git", "review")},
			{Category: "fact", Content: "Backup script runs nightly via cron: sqlite3 data.db '.backup /backups/data-$(date +%Y%m%d).db'", Importance: 0.5, Source: "chat", Tags: tags("backup", "cron")},
		}},
	}
}

// ── Simulation ─────────────────────────────────────────────────────────────

func TestSimulateMonthOfUsage(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()

	projectID := "sim-project"
	if err := store.EnsureProject(ctx, projectID, "/tmp/sim", "sim-webapp"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}

	days := scenario()
	rng := rand.New(rand.NewSource(42))

	var totalSaved, totalMerged int

	t.Log("╔══════════════════════════════════════════════════════════════╗")
	t.Log("║        30-DAY MEMORY SYSTEM SIMULATION                     ║")
	t.Log("║  Simulates a developer's month of work on a Go web app     ║")
	t.Log("║  Tests: Upsert dedup → Consolidation → Time decay → Search ║")
	t.Log("╚══════════════════════════════════════════════════════════════╝")

	consolidationDays := map[int]bool{15: true, 30: true}

	for _, day := range days {
		baseTime := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, day.day-1)

		for _, m := range day.memories {
			id, merged, err := store.Upsert(ctx, projectID, m.Category, m.Content, m.Source, m.Importance, m.Tags)
			if err != nil {
				t.Fatalf("Day %d Upsert: %v", day.day, err)
			}

			// Backdate to simulate real passage of time.
			ts := baseTime.Add(time.Duration(rng.Intn(8)) * time.Hour).Format(time.RFC3339)
			_, err = db.ExecContext(ctx, `UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id)
			if err != nil {
				t.Fatalf("backdate: %v", err)
			}

			if merged {
				totalMerged++
			} else {
				totalSaved++
			}
		}

		t.Logf("\n  Day %2d: +%d memories (%d new, %d merged so far)", day.day, len(day.memories), totalSaved, totalMerged)

		// Run consolidation at checkpoints.
		if consolidationDays[day.day] {
			runConsolidation(t, db, store, ctx, projectID, day.day)
		}
	}

	// ── Final report ───────────────────────────────────────────────────────
	t.Log("\n" + strings.Repeat("═", 65))
	t.Log("  FINAL REPORT — Day 30 (post-consolidation)")
	t.Log(strings.Repeat("═", 65))

	all, _ := store.GetAll(ctx, projectID, 200)
	globals, _ := store.GetAll(ctx, "_global", 200)

	t.Logf("\n  Project memories: %d", len(all))
	t.Logf("  Global memories:  %d", len(globals))
	t.Logf("  Total input saves: %d (%d new + %d FTS-merged)", totalSaved+totalMerged, totalSaved, totalMerged)
	t.Logf("  Compression ratio: %.0f%% (52 inputs → %d stored)", float64(len(all)+len(globals))/52*100, len(all)+len(globals))

	// Category distribution
	cats := make(map[string]int)
	for _, m := range all {
		cats[m.Category]++
	}
	t.Log("\n  Category distribution (project):")
	catOrder := []string{"architecture", "decision", "pattern", "convention", "gotcha", "dependency", "preference", "fact"}
	for _, cat := range catOrder {
		if n, ok := cats[cat]; ok {
			bar := strings.Repeat("█", n*2)
			t.Logf("    %-15s %s %d", cat, bar, n)
		}
	}

	// Global memories
	if len(globals) > 0 {
		t.Log("\n  Global memories (_global):")
		for i, m := range globals {
			t.Logf("    %d. [%s] (%.1f) %.65s", i+1, m.Category, m.Importance, m.Content)
		}
	}

	// Top 10 by time-decay score (simulating "now" = day 31)
	top, _ := store.GetTopMemories(ctx, projectID, 10)
	t.Log("\n  Top 10 by time-decay score:")
	for i, m := range top {
		age := daysSinceCreated(m.CreatedAt)
		t.Logf("    %2d. [%-12s] imp=%.1f age=%2dd  %.60s", i+1, m.Category, m.Importance, age, m.Content)
	}

	// Importance distribution
	var impBuckets [5]int
	for _, m := range all {
		bucket := int(m.Importance * 5)
		if bucket >= 5 {
			bucket = 4
		}
		impBuckets[bucket]++
	}
	t.Log("\n  Importance distribution:")
	labels := []string{"0.0-0.2", "0.2-0.4", "0.4-0.6", "0.6-0.8", "0.8-1.0"}
	for i, count := range impBuckets {
		bar := strings.Repeat("█", count*3)
		t.Logf("    %s: %s (%d)", labels[i], bar, count)
	}

	// Search quality
	t.Log("\n  Search quality:")
	searchTests := []struct {
		query    string
		wantHits bool
		desc     string
	}{
		{"SQLite WAL concurrent", true, "specific gotcha"},
		{"authentication session cookies", true, "auth architecture"},
		{"Chi middleware order", true, "middleware gotcha"},
		{"HTMX swap morph", true, "frontend gotcha"},
		{"repository interface pattern", true, "code pattern"},
		{"kubernetes pods", false, "unrelated concept"},
		{"React components", false, "wrong framework"},
	}
	searchPass := 0
	for _, st := range searchTests {
		results, _ := store.SearchFTS(ctx, projectID, st.query, 5)
		found := len(results) > 0
		status := "PASS"
		if found != st.wantHits {
			status = "FAIL"
			t.Errorf("    [%s] %s: query=%q found=%d want_hits=%v", status, st.desc, st.query, len(results), st.wantHits)
		} else {
			searchPass++
		}
		t.Logf("    [%s] %-25s query=%-35q hits=%d", status, st.desc, st.query, len(results))
	}
	t.Logf("  Search accuracy: %d/%d passed", searchPass, len(searchTests))

	// Overlap analysis
	t.Log("\n  Post-consolidation overlap analysis:")
	overlaps := analyzeOverlaps(t, all)

	// Snapshot health
	var snapCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = ?`, projectID).Scan(&snapCount); err != nil {
		t.Logf("  snapshot count query: %v", err)
	}
	t.Logf("\n  Snapshots retained: %d (max 3)", snapCount)

	// ── Assertions ─────────────────────────────────────────────────────────
	t.Log("\n" + strings.Repeat("─", 65))
	t.Log("  ASSERTIONS")
	t.Log(strings.Repeat("─", 65))

	if len(all)+len(globals) > 40 {
		t.Errorf("  FAIL: Memory bloat — %d total memories from 52 inputs (expected < 40)", len(all)+len(globals))
	} else {
		t.Logf("  PASS: No memory bloat (%d total from 52 inputs)", len(all)+len(globals))
	}

	if totalMerged < 3 {
		t.Errorf("  FAIL: Only %d FTS merges — expected ≥ 3 given intentional duplicates", totalMerged)
	} else {
		t.Logf("  PASS: FTS dedup working (%d merges)", totalMerged)
	}

	if len(globals) == 0 {
		t.Error("  FAIL: No global memories detected — scope classification broken")
	} else {
		t.Logf("  PASS: Global scope detection working (%d globals)", len(globals))
	}

	if overlaps > 5 {
		t.Errorf("  FAIL: %d overlapping pairs remain after consolidation", overlaps)
	} else {
		t.Logf("  PASS: Low overlap (%d pairs > 30%% Jaccard)", overlaps)
	}

	if searchPass < len(searchTests)-1 {
		t.Errorf("  FAIL: Search accuracy %d/%d", searchPass, len(searchTests))
	} else {
		t.Logf("  PASS: Search accuracy %d/%d", searchPass, len(searchTests))
	}

	if snapCount < 1 || snapCount > 3 {
		t.Errorf("  FAIL: Snapshot count %d (expected 1-3)", snapCount)
	} else {
		t.Logf("  PASS: Snapshot retention correct (%d)", snapCount)
	}
}

// runConsolidation executes SQLite-tier consolidation and applies results.
func runConsolidation(t *testing.T, db *sql.DB, store *memory.Store, ctx context.Context, projectID string, day int) {
	t.Helper()

	all, err := store.GetAll(ctx, projectID, 200)
	if err != nil {
		t.Fatalf("GetAll for consolidation: %v", err)
	}
	beforeCount := len(all)

	t.Logf("\n  ┌─── Consolidation cycle (Day %d) ───────────────────────────┐", day)
	t.Logf("  │ Input: %d memories                                        │", beforeCount)

	// Build reflection input.
	input := reflection.ReflectionInput{
		ExistingMemories: all,
		CurrentContext:   fmt.Sprintf("Go web app, day %d of development", day),
		ProjectName:      "sim-webapp",
		ProjectLanguage:  "go",
	}

	consolidator := reflection.NewSQLiteConsolidator()
	result, err := consolidator.Consolidate(ctx, input)
	if err != nil {
		t.Fatalf("Consolidation failed: %v", err)
	}

	// Split by scope.
	var projectMemories []reflection.ReflectMemory
	var globalMemories []reflection.ReflectMemory
	for _, m := range result.Memories {
		if m.Scope == "global" {
			globalMemories = append(globalMemories, m)
		} else {
			projectMemories = append(projectMemories, m)
		}
	}

	t.Logf("  │ Output: %d project + %d global = %d total                 │",
		len(projectMemories), len(globalMemories), len(result.Memories))
	t.Logf("  │ Compression: %d → %d (%.0f%% reduction)                    │",
		beforeCount, len(projectMemories),
		(1-float64(len(projectMemories))/float64(beforeCount))*100)

	// Data loss guard check.
	var existingNonManual int
	for _, m := range all {
		if m.Source != "manual" {
			existingNonManual++
		}
	}
	if existingNonManual >= 6 && len(projectMemories) < existingNonManual/2 {
		t.Logf("  │ ⚠ DATA LOSS GUARD would block this! (%d < %d/2)          │",
			len(projectMemories), existingNonManual)
		t.Logf("  └──────────────────────────────────────────────────────────┘")
		return
	}

	// Apply: upsert globals to _global.
	for _, m := range globalMemories {
		_, _, err := store.Upsert(ctx, "_global", m.Category, m.Content, "reflection", m.Importance, m.Tags)
		if err != nil {
			t.Logf("  │ Global upsert error: %v", err)
		}
	}

	// Apply: replace project memories.
	if len(projectMemories) > 0 {
		dbMems := make([]memory.Memory, len(projectMemories))
		for i, m := range projectMemories {
			dbMems[i] = memory.Memory{
				ProjectID:  projectID,
				Category:   m.Category,
				Content:    m.Content,
				Importance: m.Importance,
				Source:     "reflection",
				Tags:       m.Tags,
			}
		}
		if err := store.ReplaceNonManual(ctx, projectID, dbMems); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		// Backdate replaced memories to preserve time-decay simulation.
		// New reflection memories get "now" timestamps — backdate to mid-period.
		midpoint := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, day/2)
		_, err = db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE project_id = ? AND source = 'reflection'`,
			midpoint.Format(time.RFC3339), projectID)
		if err != nil {
			t.Logf("  │ Backdate warning: %v", err)
		}
	}

	afterAll, _ := store.GetAll(ctx, projectID, 200)
	afterGlobals, _ := store.GetAll(ctx, "_global", 200)

	// Show what survived.
	t.Logf("  │                                                          │")
	t.Logf("  │ After apply: %d project, %d global                        │", len(afterAll), len(afterGlobals))

	cats := make(map[string]int)
	for _, m := range afterAll {
		cats[m.Category]++
	}
	catParts := make([]string, 0)
	for cat, n := range cats {
		catParts = append(catParts, fmt.Sprintf("%s:%d", cat, n))
	}
	sort.Strings(catParts)
	t.Logf("  │ Categories: %s", strings.Join(catParts, " "))
	t.Logf("  └──────────────────────────────────────────────────────────┘")
}

// ── Analysis helpers ───────────────────────────────────────────────────────

func analyzeOverlaps(t *testing.T, memories []memory.Memory) int {
	t.Helper()
	count := 0
	for i := 0; i < len(memories); i++ {
		for j := i + 1; j < len(memories); j++ {
			if memories[i].Category != memories[j].Category {
				continue
			}
			sim := jaccardSimilarity(memories[i].Content, memories[j].Content)
			if sim > 0.3 {
				count++
				t.Logf("    OVERLAP (%.0f%%) [%s]:", sim*100, memories[i].Category)
				t.Logf("      A: %.70s", memories[i].Content)
				t.Logf("      B: %.70s", memories[j].Content)
			}
		}
	}
	if count == 0 {
		t.Log("    No significant overlaps (> 30% Jaccard) — clean memory set")
	}
	return count
}

func jaccardSimilarity(a, b string) float64 {
	tokA := tokenize(a)
	tokB := tokenize(b)
	if len(tokA) == 0 || len(tokB) == 0 {
		return 0
	}
	intersection := 0
	for w := range tokA {
		if tokB[w] {
			intersection++
		}
	}
	union := len(tokA) + len(tokB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,;:!?()[]{}\"'`—–-/")
		if len(w) > 1 {
			tokens[w] = true
		}
	}
	return tokens
}

func daysSinceCreated(createdAt string) int {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return -1
	}
	return int(math.Round(time.Since(t).Hours() / 24))
}

// Ensure json import is used (for potential future expansions).
var _ = json.Marshal
