package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// The ghost://memories/global resource, as a caller of the context assembler, and
// the BEFORE picture for its migration (#581). It was the last reader in the tree
// that chose its own rows outside `assemble.Run`: `Store.GetTopMemories` ranked
// and trimmed them in SQL, and the assembler's stages had nothing left to decide.
//
// The goldens here are recorded against origin/main, before this change.

// goldenGlobalMemoriesStore builds the fixture store the global-resource golden
// is recorded against. It is chosen to exercise every branch the migration
// touches, and — as on the project-context goldens, where the same fixture shape
// earned the same objections — every one of those branches is ASSERTED in
// TestGlobalMemoriesGoldenFixtureExercisesItsBranches rather than assumed,
// because a fixture that quietly stops building the shape it describes pins a
// parity proof of nothing.
//
// What it builds, and why each part is here:
//
//   - 22 `_global` rows so the fixed 15-cap BINDS: seven rows must fall out of the
//     answer, or the budget stage is never exercised.
//   - all eight categories, at ages far past every decay floor, so the composite
//     is `importance` times a per-category CONSTANT and the whole order is a
//     function of the fixture. A 2026-01 row read in 2026-10 is ~275 days old,
//     and the two decay branches bottom out at 105 days (45-day tau, floor 0.3)
//     and 170 days (30-day tau, floor 0.15), which is what makes the golden
//     independent of the wall clock — an age only grows, so no later run can
//     un-floor a row and reorder the block.
//   - every row that the cap EXCLUDES is a DECAYING category, and decay is
//     what REORDERS them rather than what puts them out: a `gotcha` at
//     importance 1.00 ranks below a `convention` at 0.45. Deleting the decay
//     CASE therefore does NOT change which rows are in the answer — the
//     undamped top fifteen is the same fifteen ids — so the fixture pins decay
//     by ORDER, and the positional assertion on gmem07 before gmem12 is what
//     catches it. Stated
//     here because the opposite claim is the one this fixture's shape invites:
//     gmem17-gmem21 sit below the cap undamped too (raw importance 0.30-0.22,
//     under gmem07's 0.45), so they are rows the cap excludes either way, and a
//     comment claiming they would have arrived is claiming a membership change
//     the numbers do not produce.
//   - a pinned row, a scoped row and a tagged row, so all three renderer labels
//     are pinned on the answer.
//   - one row carrying an agent, a source reference, a confidence and a non-manual
//     source, and one whose content holds the « » data delimiters, so the EIGHT
//     renderer fields are pinned by the recorded bytes rather than by a reader
//     comparing two label functions side by side. See `extras` below.
//   - a `supersedes` pair and a `duplicate` pair, each placed so that losing the
//     demotion changes the answer's MEMBERSHIP rather than only its order —
//     see the branch test.
//   - a resolved global carrying the HIGHEST importance in the fixture, which
//     `resolved_at IS NULL` must keep out. It outranks everything on purpose: a
//     resolved row that also ranked last would be absent for the wrong reason.
//
// created_at and updated_at are written explicitly rather than left to
// datetime('now'), so the ordering is a function of the fixture and not of when
// the test runs.
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

	// One stamp per row, ascending, so `created_at DESC` is a total order over
	// rows whose composite score ties. The day is in the past and far enough from
	// every decay floor that the clock cannot change any of them.
	stamps := make([]string, 24)
	for i := range stamps {
		stamps[i] = "2026-01-" + twoDigits(i+2) + " 03:04:05"
	}

	// (category, importance, pinned, tags, scope, composite score), listed in the
	// order they render in.
	//
	// The `score` column is the whole truth about the order and needs no
	// recomputation, because every one of those composites is floored: a pinned
	// row and a preference/convention/fact row score at raw importance, and the
	// rest score at importance x 0.3 (pattern/architecture) or x 0.15
	// (everything else). The branch test asserts the outcome, not this column —
	// the column is here so a reader can check the arithmetic rather than trust it.
	//
	// tags is an empty JSON array rather than NULL on an untagged row, because the
	// column is bound as a plain string and a NULL there is a Scan error rather
	// than "no tags" — the same asymmetry passiveColumnsFor substitutes around for
	// `retention`.
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
	// The four columns the renderer reads that the table above does not carry: the
	// agent, the source reference, the confidence, the origin source, and a content
	// string holding the data delimiters themselves.
	//
	// They ride on ROWS ALREADY IN THE ANSWER rather than on a new one, and that is
	// forced rather than chosen: the 15-cap is exactly full (fifteen of twenty
	// penalty-free rows are admitted), so a twenty-third row would displace an
	// admitted one and cost the demotion proof below its four-row membership
	// change. The fields under test are the same either way — the golden pins the
	// RENDERING of a row carrying them, and both renderers are handed the same
	// columns for the same row.
	//
	// gmem04 takes the labels and the delimiters: a `convention` row is otherwise
	// unremarkable, and the delimiter case is the one that has to be here rather
	// than reasoned about, because `assemble.Data` and `mcpserver.quoteData` are
	// two separate implementations of the SAME neutralisation and the golden is the
	// only thing that can say they agree. gmem06 takes the non-manual source,
	// because `source=` is the ORIGIN label — the one field whose value is computed
	// from `project_id` and `source` TOGETHER through
	// `memory.CanonicalOriginSourceForProject`, so it is the field most likely to
	// be computed differently on the two paths.
	type goldenExtra struct {
		agent, srcRef, source string
		conf                  float64
		content               string
	}
	extras := map[int]goldenExtra{
		4: {
			agent: "claude-code", srcRef: "ghost#872", conf: 0.8,
			// The « and » inside are the point: they must render as << and >>, and
			// the << that follows must NOT be escaped again, because the replacer
			// substitutes once in a single pass rather than repeatedly.
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
		// The three optional columns bind as parameters and are NULL when unset,
		// because that is what every real writer stores and what `scanMemories`
		// expects — unlike `tags`, which the row above has to substitute as SQL
		// text because the scanner binds it as a plain string and a NULL there is a
		// Scan error rather than "no tags".
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
	// The resolved global: the fetch filters resolved_at IS NULL, so it must appear
	// in NO answer. It carries importance 1.0, tying the highest live row for
	// attention, and a resolved row that also outranked everything is the only
	// version of this assertion that could fail.
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, resolved_at, created_at, updated_at)
	                     VALUES ('gmemres', '_global', 'preference', 'a resolved global memory', 'manual', 1.0, ?, ?, ?)`,
		stamps[0], stamps[0], stamps[0]); err != nil {
		t.Fatalf("insert resolved global memory: %v", err)
	}

	// The two demotion pairs, each built to be OBSERVABLE.
	//
	// `gmem05` replaces `gmem10`, and `gmem03` beats `gmem14` as a near-duplicate.
	// Both casualties sit INSIDE the 15-cap on composite score alone — gmem10 at
	// rank 11 of 22 and gmem14 at rank 15, with gmem14 exactly on the boundary —
	// so both would be in the answer with the demotions removed. Each therefore
	// has to be pushed OUT, and because demotion is a stable sort ascending by
	// penalty the two of them sink below all twenty penalty-free rows and take the
	// answer's last two slots with them.
	//
	// The pairs are therefore observable as a FOUR-row membership change: gmem10
	// and gmem14 leave, and gmem15 and gmem16 arrive. A pair placed below the cap
	// instead would reorder rows the cut discards anyway, leave the recorded block
	// byte-identical, and pin nothing.
	//
	// Two pairs of two different relations, because the two demotions are separate
	// lookups and a fixture carrying only one of them would leave the other's
	// removal untested.
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

// renderGlobalMemoriesResource reads the ghost://memories/global resource over the
// MCP transport, so the golden is the bytes an agent receives rather than a
// function's return value.
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

// TestGlobalMemoriesResourceGolden is the BEFORE picture for the named surface:
// it pins the exact bytes `ghost://memories/global` renders for a fixed fixture
// store, recorded on origin/main before any of this change existed.
//
// A whole-block string and not a set of substring assertions, on the same
// reasoning as the project-context and session-start goldens: the migration
// changes which reader SELECTS the rows, so the thing a reviewer needs is a diff
// they can read, and a substring test passes under both shapes.
//
// The byte-identity claim is SCOPED, and the scope is stated here rather than left
// for a reader to discover: this fixture seeds no validity column, so it cannot
// see the one behaviour the migration changes. A store whose rows carry a closed
// window is pinned by
// TestTheGlobalMemoriesResourceWithholdsARowWhoseWindowHasClosed instead — and
// deliberately NOT here, because re-recording this baseline to accommodate it
// would turn the golden from a parity proof into a diff record, which is the one
// thing it exists to be.
func TestGlobalMemoriesResourceGolden(t *testing.T) {
	want := readGlobalMemoriesGolden(t, "globalmem_resource.golden")
	got := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	if got != want {
		t.Errorf("ghost://memories/global changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestGlobalMemoriesGoldenFixtureIsStableAcrossRuns is the guard on the golden: a
// baseline recorded from a fixture that depends on the wall clock is not a
// baseline, and two renders of one store must be byte-identical or the comparison
// above proves nothing.
func TestGlobalMemoriesGoldenFixtureIsStableAcrossRuns(t *testing.T) {
	first := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	second := renderGlobalMemoriesResource(t, goldenGlobalMemoriesStore(t))
	if first != second {
		t.Fatalf("the global-memories golden fixture is not deterministic across stores.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestGlobalMemoriesGoldenFixtureExercisesItsBranches is the guard that keeps the
// golden from becoming a picture of a path nothing takes. Every claim the fixture
// makes is checked against the rendered bytes, and the two demotion pairs are
// checked by ABSENCE — the same argument as the project-context golden's: a
// reorder inside the cap would leave the bytes alone, so asserting the order of
// the rows that remain would pass under a fixture where the demotions do nothing.
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
	// Every category the fixture seeds must have a member in the answer, or a decay
	// group is untested. A group is what makes the answer's ORDER a function of
	// the composite rather than of importance alone: `gotcha`, `decision` and
	// `dependency` all decay on the 30-day curve and `architecture` and `pattern`
	// on the 45-day one, so a row that reaches the answer through a decayed group
	// proves the order took the decay into account rather than the importance
	// alone.
	for _, cat := range []string{"preference", "convention", "fact", "gotcha", "decision", "dependency", "architecture", "pattern"} {
		if !strings.Contains(out, "- ["+cat+"] `gmem") {
			t.Errorf("no %s global row reached the answer, so its decay group is untested:\n%s", cat, out)
		}
	}
	// Decay is then asserted AGAINST importance, by position rather than by
	// presence: gmem07 is a `convention` at importance 0.45 and gmem12 is a
	// `gotcha` at importance 1.00 — more than double — and the decayed row must
	// still come SECOND. The category loop above would pass under a reader that
	// ranked on importance alone and happened to admit both rows; this cannot,
	// and it is the same property the `score` column was designed around.
	if decayed, undamped := strings.Index(out, "`gmem07`"), strings.Index(out, "`gmem12`"); decayed < 0 || undamped < 0 || decayed > undamped {
		t.Errorf("gmem07 (convention, importance 0.45, no decay) must rank above gmem12 (gotcha, importance 1.00, 30-day floor 0.15); "+
			"gotcha at %d, convention at %d:\n%s", undamped, decayed, out)
	}
	// The resolved row is excluded by the fetch, not by the cap.
	if strings.Contains(out, "a resolved global memory") {
		t.Errorf("the resolved global reached the answer; resolved_at IS NULL is not doing what the golden assumes:\n%s", out)
	}
	// The demotions have to change MEMBERSHIP or the golden pins the selection
	// order and nothing else. Both casualties are absent, and the two rows the
	// demotions admit in their place are present: neither half is implied by the
	// other, so a demotion that moved nothing and a demotion that moved the wrong
	// row both fail here.
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
	// The cap binds, or the budget stage is never exercised: 22 live globals, 15
	// admitted.
	if n := strings.Count(out, "\n- ["); n != globalMemoriesLimit {
		t.Errorf("the answer carries %d memory lines, so the %d-cap is not binding and the budget stage is untested:\n%s",
			n, globalMemoriesLimit, out)
	}
}

// readGlobalMemoriesGolden returns the recorded baseline, and FAILS rather than
// comparing against "" when the file is missing: an empty expected value
// compared with `!=` passes against an empty result, so a golden that silently
// went unread would make the one test that exists to prove parity pass on a block
// nobody wrote down.
//
// The baseline lives in a file rather than a Go constant because the block prints
// every id inside backticks, which a raw string literal cannot hold — and because
// a file is the reviewable artefact. A reviewer reads the diff between the
// recorded block and the new one, which is the entire point of pinning one.
func readGlobalMemoriesGolden(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(raw)
}

// recordGlobalMemoriesGolden writes the current answer as the baseline. It is a
// test rather than a flag so it cannot be run by accident: `go test -run` selects
// it by name, and every other test in this package is a comparison against the
// file it writes.
//
// It refuses to overwrite an existing baseline, because a re-record is a decision
// about what the surface should now say and the diff of it is the review artefact
// — a flag that quietly refreshes a baseline turns the parity proof into a
// rubber stamp.
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
