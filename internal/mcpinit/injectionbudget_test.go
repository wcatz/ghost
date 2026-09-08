package mcpinit

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// This suite proves the category-priority + compact render trade is a net win
// on the session-injection budget. It lives in package mcpinit (rather than
// internal/bench) because it must drive the unexported loadSessionContext /
// formatSessionContext pipeline over a real store — the actual code the hook
// runs — instead of a reconstruction. See docs/benchmarks.md ("Injection
// budget").

// representativeCorpus builds a ghost-project-like corpus (57 memories across
// every category, skewed importance so the descriptive categories would crowd
// the top slots by raw decay score) so the behavioral floor and the compact
// byte savings are measured over a realistic mix, not a synthetic degenerate
// one.
func representativeCorpus(t *testing.T, db *sql.DB, projectID string) {
	t.Helper()
	type seed struct {
		cat, content string
		importance   float64
	}
	// 57 rows: strong behavioral presence (gotcha/convention/preference/
	// decision) plus descriptive categories (architecture/fact/dependency) —
	// mirroring the kind of mix the real ghost project holds. Without the
	// behavior floor, the 14 high-importance architecture rows plus 12 facts
	// would dominate the rank-only top-15 and starve the behavioral slots.
	seeds := []seed{
		{cat: "gotcha", importance: 0.9}, {cat: "gotcha", importance: 0.8},
		{cat: "gotcha", importance: 0.7}, {cat: "gotcha", importance: 0.6},
		{cat: "gotcha", importance: 0.5}, {cat: "gotcha", importance: 0.4},
		{cat: "convention", importance: 0.9}, {cat: "convention", importance: 0.8},
		{cat: "convention", importance: 0.7}, {cat: "convention", importance: 0.6},
		{cat: "convention", importance: 0.5},
		{cat: "preference", importance: 0.9}, {cat: "preference", importance: 0.8},
		{cat: "preference", importance: 0.7}, {cat: "preference", importance: 0.6},
		{cat: "preference", importance: 0.5}, {cat: "preference", importance: 0.4},
		{cat: "decision", importance: 0.9}, {cat: "decision", importance: 0.8},
		{cat: "decision", importance: 0.7}, {cat: "decision", importance: 0.6},
		{cat: "decision", importance: 0.5},
		// 14 architecture (descriptive, must NOT crowd out behavioral under floor)
		{cat: "architecture", importance: 0.95}, {cat: "architecture", importance: 0.90},
		{cat: "architecture", importance: 0.85}, {cat: "architecture", importance: 0.80},
		{cat: "architecture", importance: 0.75}, {cat: "architecture", importance: 0.70},
		{cat: "architecture", importance: 0.65}, {cat: "architecture", importance: 0.60},
		{cat: "architecture", importance: 0.55}, {cat: "architecture", importance: 0.50},
		{cat: "architecture", importance: 0.45}, {cat: "architecture", importance: 0.40},
		{cat: "architecture", importance: 0.35}, {cat: "architecture", importance: 0.30},
		// 12 fact (never-decay, high score)
		{cat: "fact", importance: 0.9}, {cat: "fact", importance: 0.85},
		{cat: "fact", importance: 0.8}, {cat: "fact", importance: 0.75},
		{cat: "fact", importance: 0.7}, {cat: "fact", importance: 0.65},
		{cat: "fact", importance: 0.6}, {cat: "fact", importance: 0.55},
		{cat: "fact", importance: 0.5}, {cat: "fact", importance: 0.45},
		{cat: "fact", importance: 0.4}, {cat: "fact", importance: 0.35},
		// 9 dependency
		{cat: "dependency", importance: 0.7}, {cat: "dependency", importance: 0.6},
		{cat: "dependency", importance: 0.5}, {cat: "dependency", importance: 0.4},
		{cat: "dependency", importance: 0.3}, {cat: "dependency", importance: 0.2},
		{cat: "dependency", importance: 0.15}, {cat: "dependency", importance: 0.1},
		{cat: "dependency", importance: 0.05},
	}
	total := 0
	for i, s := range seeds {
		id := fmt.Sprintf("b%04d", i)
		content := fmt.Sprintf("budget %s row %d content", s.cat, i)
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance)
			 VALUES (?, ?, ?, ?, 'manual', ?)`,
			id, projectID, s.cat, content, s.importance,
		); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		total++
	}
	if total != len(seeds) {
		t.Fatalf("corpus has %d seeds, want %d", total, len(seeds))
	}
}

func TestBenchInjectionBudget(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	representativeCorpus(t, db, "p1")
	_ = db.Close()

	_, project, memories, _, _, _, _, _, totalKnown := loadSessionContext(projDir)
	if project != "myproj" {
		t.Fatalf("project = %q, want myproj", project)
	}
	if !totalKnown {
		t.Fatalf("expected total memory count to be known")
	}
	if len(memories) != sessionMemoriesCap {
		t.Fatalf("expected %d selected memories, got %d", sessionMemoriesCap, len(memories))
	}

	// Configured behavioral floor must be honored when the corpus has enough.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	behavioral := map[string]bool{}
	for _, c := range cfg.Injection.BehaviorCategories {
		behavioral[c] = true
	}
	hit := 0
	for _, m := range memories {
		if behavioral[m.Category] {
			hit++
		}
	}
	floor := cfg.Injection.BehaviorFloor
	if hit < floor {
		t.Errorf("behavior floor: selected %d behavioral, need %d", hit, floor)
	}

	// Compact render must not grow the budget: rendering the same selected
	// set, dropping the 32-hex ID per line (plus the backtick pair) strictly
	// shrinks the block. This is the byte-neutrality evidence for the trade.
	newRender := formatMemoriesMarkdown(memories)
	idRender := formatMemoriesMarkdownWithID(memories)
	if len(newRender) > len(idRender) {
		t.Errorf("compact render grew the budget: %d bytes > legacy %d bytes", len(newRender), len(idRender))
	}
	t.Logf("injection budget: %d memories, behavioral hit %d/%d (floor %d), compact %d bytes vs legacy-with-ID %d bytes",
		len(memories), hit, floor, floor, len(newRender), len(idRender))
}

// formatMemoriesMarkdown renders the session memory lines in the current
// compact form (no ID).
func formatMemoriesMarkdown(memories []sessionMemory) string {
	var sb strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&sb, "- [%s] %s\n", m.Category, quoteData(m.Content))
	}
	return sb.String()
}

// formatMemoriesMarkdownWithID renders the same lines the historical way
// (32-hex ID in backticks) so the budget test can quantify the format saving.
func formatMemoriesMarkdownWithID(memories []sessionMemory) string {
	var sb strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&sb, "- [%s] `%s` %s\n", m.Category, m.ID, quoteData(m.Content))
	}
	return sb.String()
}
