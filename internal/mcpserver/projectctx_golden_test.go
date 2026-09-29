package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// goldenProjectContextStore builds the fixture store both project-context goldens
// are recorded against. It is chosen to exercise every branch the migration
// touches, and — as on the session-start digest, where the same fixture shape
// earned the same objections — every one of those branches is ASSERTED rather
// than assumed, because a fixture that quietly stops building the shape it
// describes pins a parity proof of nothing.
//
// What it builds:
//
//   - 18 project rows across ALL EIGHT categories, so the decay groups (1.0 for
//     preference/convention/fact, 0.3 floored for pattern/architecture, 0.15
//     floored for gotcha/decision, and pinned exempted) each have a member and
//     the composite order is a function of the fixture.
//   - 20 `_global` rows, two of which outrank every project row, so the mixed
//     read admits globals — that is the shape `GetTopMemories`' `project_id = ?
//     OR project_id = '_global'` produces and the one a per-project bucket cannot
//     express.
//   - a pinned row, a scoped row, and a tagged row, so the renderer is pinned on
//     each of the three labels.
//   - a resolved project row and a resolved global, which `resolved_at IS NULL`
//     must keep out of both sections.
//   - a `supersedes` edge and a `duplicate` edge, each placed so that losing the
//     demotion MOVES A ROW OUT of a section rather than merely reordering one —
//     see the assertions in TestProjectContextGoldenFixtureExercisesItsBranches.
//
// created_at and updated_at are written explicitly rather than left to
// datetime('now'), so the ordering is a function of the fixture and not of when
// the test runs. That is stronger here than it looks: every decaying category is
// at its floor at these ages (a 2026-01 row read in 2026-09 is ~250 days old, and
// both decay branches bottom out well before that), so the composite score is
// importance × a CONSTANT and the whole order is decided by importance, then
// created_at, then id — with no dependence on the wall clock at all.
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

	// One stamp per row, ascending, so `created_at DESC` is a total order over
	// rows whose composite score ties — which two of these deliberately do.
	stamps := make([]string, 22)
	for i := range stamps {
		stamps[i] = "2026-01-" + twoDigits(i+2) + " 03:04:05"
	}

	// (category, importance, pinned, tags, scope)
	type row struct {
		cat    string
		imp    float64
		pinned bool
		tags   string
		scope  string
	}
	// tags is an empty JSON array rather than NULL on an untagged row, because
	// the column is bound as a plain string and a NULL there is a Scan error
	// rather than "no tags" — the same asymmetry passiveColumnsFor substitutes
	// around for `retention`.
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
	// A resolved project row: the fetch filters resolved_at IS NULL, so it must
	// appear in NEITHER section. It carries the HIGHEST importance of any row in
	// the fixture on purpose — a resolved row that also outranks everything is the
	// only version of this assertion that could fail, because a resolved row that
	// ranked last would be absent from the block for the wrong reason.
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, resolved_at, created_at, updated_at)
	                     VALUES ('pmemres', 'pcgold', 'preference', 'a resolved project memory', 'manual', 1.0, ?, ?, ?)`,
		stamps[0], stamps[0], stamps[0]); err != nil {
		t.Fatalf("insert resolved project memory: %v", err)
	}

	// The globals, every one of them ranked BELOW every project row, so the mixed
	// read fills its 20-cap with 18 project rows and 2 globals. That is the shape
	// `GetTopMemories`' `project_id = ? OR project_id = '_global'` produces and the
	// one a per-project bucket cannot express, and keeping the globals below the
	// whole project set is what makes the Global section a real 12-row block
	// rather than the two rows that happen to be left over.
	//
	// The spacing is 0.001 apart, which is above the 0.0005 the assembler
	// renders at fixed width for a COSINE — not a constraint here (nothing reads a
	// cosine on a passive block) but a reason the ordering below is stated in
	// terms of the composite score rather than of the printed importance.
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

	// The two demotion pairs, each built to be OBSERVABLE.
	//
	// `pmem10` replaces `pmem15`. pmem15 is the lowest-scoring PROJECT row and
	// sits inside the mixed 20-cap, so losing the supersede demotion moves a row
	// OUT of the section rather than merely down it — a reorder that stayed inside
	// the cap would leave the recorded block byte-identical and the pair untested,
	// which is the objection that caught the session-start fixture's first version.
	//
	// `gmem12` is a near-duplicate loser of `gmem03`. The Global section is where
	// both endpoints are visible, and its 15-cap sits just above gmem12, so a
	// near-duplicate demotion that stopped firing would put gmem12 back and change
	// the section's membership.
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

// renderProjectContextTool calls the real `ghost_project_context` tool over the
// MCP transport, so the golden is the bytes an agent receives rather than a
// function's return value.
func renderProjectContextTool(t *testing.T, srv *Server) string {
	t.Helper()
	return resultText(callTool(t, connectedClient(t, srv), "ghost_project_context", map[string]any{
		"project_id": "pcgold",
	}))
}

// renderProjectContextResource calls buildProjectContext, which is the body of
// both the ghost://project/{id}/context resource and the recall_project prompt.
// It is a second golden rather than a derivation of the first: the two surfaces
// have different budgets (a caller-supplied limit against fixed 20/15 caps) and
// the resource carries the two sections the tool does not.
func renderProjectContextResource(t *testing.T, srv *Server) string {
	t.Helper()
	text, err := srv.buildProjectContext(context.Background(), "pcgold")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	return text
}

// TestProjectContextToolBlockGolden is the BEFORE picture for the named surface:
// it pins the exact bytes `ghost_project_context` renders for a fixed fixture
// store, recorded on origin/main before any of this change existed.
//
// A whole-block string and not a set of substring assertions, on the same
// reasoning as the session-start golden: the migration changes which reader
// SELECTS the rows, so the thing a reviewer needs is a diff they can read, and a
// substring test passes under both shapes.
//
// The byte-identity claim is SCOPED, and the scope is stated here rather than
// left for a reader to discover: this fixture seeds no validity column, so it
// cannot see the one behaviour the migration changes. A store whose rows carry a
// closed window is pinned by TestTheProjectContextDropsRowsWhoseWindowHasClosed
// instead — and deliberately NOT here, because re-recording this baseline to
// accommodate it would turn the golden from a parity proof into a diff record,
// which is the one thing it exists to be.
func TestProjectContextToolBlockGolden(t *testing.T) {
	want := readProjectContextGolden(t, "projectctx_tool.golden")
	got := renderProjectContextTool(t, goldenProjectContextStore(t))
	if got != want {
		t.Errorf("ghost_project_context changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestProjectContextResourceBlockGolden is the same record for the resource and
// prompt body, whose 20/15 caps and two sections the tool does not have.
func TestProjectContextResourceBlockGolden(t *testing.T) {
	want := readProjectContextGolden(t, "projectctx_resource.golden")
	got := renderProjectContextResource(t, goldenProjectContextStore(t))
	if got != want {
		t.Errorf("ghost://project/{id}/context changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestProjectContextGoldenFixtureIsStableAcrossRuns is the guard on both
// goldens: a baseline recorded from a fixture that depends on the wall clock is
// not a baseline, and two renders of one store must be byte-identical or the
// comparison above proves nothing.
func TestProjectContextGoldenFixtureIsStableAcrossRuns(t *testing.T) {
	first := renderProjectContextTool(t, goldenProjectContextStore(t))
	second := renderProjectContextTool(t, goldenProjectContextStore(t))
	if first != second {
		t.Fatalf("the tool golden fixture is not deterministic across stores.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	firstRes := renderProjectContextResource(t, goldenProjectContextStore(t))
	secondRes := renderProjectContextResource(t, goldenProjectContextStore(t))
	if firstRes != secondRes {
		t.Fatalf("the resource golden fixture is not deterministic across stores.\n--- first ---\n%s\n--- second ---\n%s", firstRes, secondRes)
	}
}

// TestProjectContextGoldenFixtureExercisesItsBranches is the guard that keeps the
// two goldens from becoming pictures of a path nothing takes. Every claim the
// fixtures make is checked against the rendered bytes, and the two demotion pairs
// are checked by ABSENCE — the same argument as the session-start golden's, and
// for the same reason: asserting the order of the rows that remain would pass
// under a fixture where the demotions do nothing, while these two rows are the
// only ones whose MEMBERSHIP the demotions decide.
func TestProjectContextGoldenFixtureExercisesItsBranches(t *testing.T) {
	srv := goldenProjectContextStore(t)
	tool := renderProjectContextTool(t, srv)
	res := renderProjectContextResource(t, srv)

	for _, want := range []string{
		"## Memories",              // the memory section
		"[pinned]",                 // the pin exemption and its label
		`tags:["golden","pinned"]`, // the tag label
		"scope{area=payments}",     // the scope label
	} {
		if !strings.Contains(res, want) {
			t.Errorf("fixture does not exercise %q; resource block is:\n%s", want, res)
		}
	}
	if !strings.Contains(res, "## Global (applies to all projects)") {
		t.Errorf("fixture produced no Global section, so the _global read is untested:\n%s", res)
	}
	// Every category, so the composite order is exercised across all three decay
	// groups rather than only the non-decaying one.
	for _, cat := range []string{"preference", "convention", "fact", "architecture", "pattern", "gotcha", "decision"} {
		if !strings.Contains(tool, "- ["+cat+"] `pmem") {
			t.Errorf("no %s project row reached the block, so a decay group is untested:\n%s", cat, tool)
		}
	}
	// The resolved rows are excluded by the fetch, not by the cap.
	for _, resolved := range []string{"a resolved project memory", "a resolved global memory"} {
		if strings.Contains(res, resolved) {
			t.Errorf("%q reached the block; resolved_at IS NULL is not doing what the golden assumes:\n%s", resolved, res)
		}
	}
	// The demotions have to change MEMBERSHIP or the goldens pin the selection
	// order and nothing else.
	for _, absent := range []string{"project memory 15 content", "global memory 12 content"} {
		if strings.Contains(res, absent) {
			t.Errorf("%q is in the block, so its demotion is not observable and the golden does not test it:\n%s", absent, res)
		}
	}
	for _, present := range []string{"project memory 10 content", "global memory 03 content"} {
		if !strings.Contains(res, present) {
			t.Errorf("the surviving endpoint of a demotion pair (%q) is missing from the block:\n%s", present, res)
		}
	}
	// Both caps bind, or the budget stage is not exercised at all. The mixed read
	// holds 18 project rows and 2 globals and cuts at 20; the Global section holds
	// 20 globals and cuts at 15.
	if n := strings.Count(tool, "\n- ["); n != 20 {
		t.Errorf("the tool block carries %d memory lines, so the 20-cap is not binding and stage 8 is untested:\n%s", n, tool)
	}
	if n := strings.Count(res, "\n- [preference] `gmem"); n != 15 {
		t.Errorf("the Global section carries %d rows, so the 15-cap is not binding:\n%s", n, res)
	}
}

// readProjectContextGolden returns the recorded baseline for one surface, and
// FAILS rather than comparing against "" when the file is missing: an empty
// expected value compared with `!=` passes against an empty result, so a golden
// that silently went unread would make the one test that exists to prove parity
// pass on a block nobody wrote down.
//
// The baseline lives in a file rather than a Go constant because these blocks
// print every id inside backticks, which a raw string literal cannot hold — and
// because a file is the reviewable artefact. A reviewer reads the diff between
// the recorded block and the new one, which is the entire point of pinning one.
func readProjectContextGolden(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(raw)
}
