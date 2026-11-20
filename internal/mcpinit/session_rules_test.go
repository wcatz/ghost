package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

var fixtureWords = []string{"amber", "basalt", "cobalt", "dune", "ember", "fjord", "garnet", "harbor", "indigo", "juniper", "kelp", "lagoon", "marble", "nectar", "onyx", "prairie", "quartz", "ridge", "sorrel", "tundra", "umber", "velvet", "willow", "xenon", "yarrow", "zephyr", "alpine", "birch", "cedar", "delta", "estuary", "flint"}

// seedRuleStore writes a project p1 and the global bucket: projectRules
// low-importance rules (gotcha, convention, preference) and projectFacts high-importance facts in the
// project, globalRules low-importance rules (gotcha, convention, preference) and globalFacts high-importance
// facts in _global. It returns the project directory.
func seedRuleStore(t *testing.T, projectRules, projectFacts, globalRules, globalFacts int) string {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT OR IGNORE INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical)
	stamp := "2026-01-02 12:00:00"
	seq := 0
	// The assembler's diversity stage lets no single category take more than half
	// a bucket's slots, so rules are spread over the three rule categories.
	ruleCats := []string{"gotcha", "convention", "preference"}
	insert := func(project, prefix, category string, n int, importance float64) {
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s%02d", prefix, i)
			// distinct vocabulary per row, so no near-duplicate demotion
			// reorders the fixture
			k := seq
			seq++
			text := fmt.Sprintf("%s %s %s %s", strings.ToUpper(prefix), fixtureWords[k%32], fixtureWords[(k*5+1)%32], fixtureWords[k/32])
			cat := category
			if cat == "rule" {
				cat = ruleCats[i%len(ruleCats)]
			}
			exec(`INSERT INTO memories (id, project_id, category, content, source, importance, created_at) VALUES (?, ?, ?, ?, 'manual', ?, ?)`,
				id, project, cat, text, importance, stamp)
		}
	}
	insert("p1", "prule", "rule", projectRules, 0.1)
	insert("p1", "pfact", "fact", projectFacts, 0.9)
	insert("_global", "grule", "rule", globalRules, 0.5)
	insert("_global", "gfact", "fact", globalFacts, 0.9)
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projDir
}

func ruleSessionBlock(t *testing.T, projDir string) string {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	runSessionStartHook(t, string(input), &out)
	return out.String()
}

// The project fixture holds more rules than the old configured behavior floor
// (8) and many higher-ranked facts: origin/main keeps 8 of them, this branch's
// reserve keeps sessionRuleReserve.
func TestSessionStartReservesProjectRuleSlots(t *testing.T) {
	projDir := seedRuleStore(t, sessionRuleReserve+2, 20, 0, 0)
	got := strings.Count(ruleSessionBlock(t, projDir), "PRULE ")
	if got < sessionRuleReserve {
		t.Fatalf("project rules kept = %d, want at least the reserve %d", got, sessionRuleReserve)
	}
}

// Main gave the global bucket no rule floor, so facts at higher importance
// took every global slot.
func TestSessionStartReservesGlobalRuleSlots(t *testing.T) {
	projDir := seedRuleStore(t, 0, 0, globalsRuleReserve+2, globalsCap)
	got := strings.Count(ruleSessionBlock(t, projDir), "GRULE ")
	if got < globalsRuleReserve {
		t.Fatalf("global rules kept = %d, want at least the reserve %d", got, globalsRuleReserve)
	}
}

// The reserve numbers in docs/mcp.md are written by hand; the code derives
// them, so the doc must state exactly what the constants say.
func TestDocumentedRuleReserveMatchesCode(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("reserves %d of the %d project slots and %d of the %d global slots",
		sessionRuleReserve, sessionMemoriesCap, globalsRuleReserve, globalsCap)
	if !strings.Contains(string(doc), want) {
		t.Fatalf("docs/mcp.md must say %q", want)
	}
}
