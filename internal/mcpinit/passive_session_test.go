package mcpinit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// mcpinitSource returns the text of every non-test .go file in internal/mcpinit.
// It reads the files rather than reaching into the package's own identifiers on
// purpose: this test asserts on which SELECT statements the package still issues,
// and that has to be read off the source. A test that could not fail on an
// unchanged file would be the over-fix of a source assertion.
func mcpinitSource(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no non-test source found; the glob is looking in the wrong directory")
	}
	return out
}

// TestSessionStartSelectsItsMemoryRowsThroughTheAssembler is the test for THIS
// PR: the session-start block's memory rows must be chosen by `assemble.Run`, not
// by a private query in this package.
//
// The behavioural properties are covered elsewhere — the golden pins the rendered
// bytes, the passive latency test pins the read bound — and a behavioural test
// cannot tell which of two code paths produced identical bytes. A hook that
// rendered a byte-identical block from its own SELECT would satisfy every output
// test and would have migrated nothing. So the property has to be read off the
// source, and the form that reads it is the ABSENCE of the old shape rather than
// the presence of a new name: a rename satisfies "loadSessionPassive exists" and
// migrates nothing, while the two statements below are the whole of what was
// removed.
func TestSessionStartSelectsItsMemoryRowsThroughTheAssembler(t *testing.T) {
	src := mcpinitSource(t)

	// Every query this package still issues against the memories table must be an
	// AGGREGATE. A count is not a selection: it names no row, and the number the
	// block's "N of M" line prints is not a choice about which memories a session
	// is shown. A row projection is a choice, and a choice made here is a second
	// implementation of the assembler's stages — its caps, its order and its
	// near-duplicate pass — free to drift from the policies the budget now states
	// in one place.
	//
	// The form is a scan for the PROJECTION rather than a search for a function
	// name, because that is what actually pins the property. Asserting that some
	// named helper is gone would be satisfied by renaming it and inlining the very
	// query being removed, and a test that cannot fail on an unchanged file is the
	// over-fix of a source assertion.
	for name, text := range src {
		for _, projection := range memoryProjections(text) {
			if projection != "COUNT(*)" {
				t.Errorf("internal/mcpinit/%s issues a row projection against memories (%q); the session-start "+
					"block's rows must be chosen by assemble.Run, and only a COUNT may be read here", name, projection)
			}
		}
	}

	// The positive half, and the property that makes the negative one mean
	// something: the session-start path reaches the assembler, and it reaches it
	// with the empty Query that IS the passive shape. A non-empty query here would
	// fuse legs and answer a relevance question no session start asked.
	if !strings.Contains(src["session_passive.go"], "assemble.Run(") {
		t.Error("internal/mcpinit/session_passive.go never calls assemble.Run; the session-start block is not assembled")
	}
	if !strings.Contains(src["session_passive.go"], `Query:     "",`) {
		t.Error("internal/mcpinit/session_passive.go does not send the empty Query that IS the passive shape, so the " +
			"retriever would take its query branch and answer a relevance question this surface never asked")
	}
}

// TestTheSessionBudgetStatesBothBuckets is the half of the migration a golden
// cannot see: the two buckets' caps, orders, over-fetches and demotion
// thresholds are now ONE statement, so they cannot be one bucket's policy applied
// to the other by a copy that drifted.
//
// Each assertion below is a number the shipped loaders carried, and a silent
// change to any of them is a change to what a user's session is told.
func TestTheSessionBudgetStatesBothBuckets(t *testing.T) {
	budget := sessionPassiveBudget(mustHookConfig(t), "psomeproj")
	if len(budget.Slices) != 2 {
		t.Fatalf("the budget has %d slices, want 2: one per bucket, or the caps are stated for a bucket nothing reads", len(budget.Slices))
	}

	project, globals := budget.Slices[0], budget.Slices[1]
	if project.Bucket != "psomeproj" {
		t.Errorf("the project slice's bucket is %q, want the project id — a bucket policy IS the project predicate", project.Bucket)
	}
	if project.MaxItems != sessionMemoriesCap || project.OverFetch != sessionMemoriesCap*3 {
		t.Errorf("the project slice reads (MaxItems=%d, OverFetch=%d), want (%d, %d) — the cap and the over-fetch "+
			"are one budget and drifted apart once already", project.MaxItems, project.OverFetch, sessionMemoriesCap, sessionMemoriesCap*3)
	}
	if globals.Bucket != "_global" || globals.MaxItems != globalsCap || globals.OverFetch != globalsCap*2 {
		t.Errorf("the global slice is (bucket=%q, MaxItems=%d, OverFetch=%d), want (_global, %d, %d)",
			globals.Bucket, globals.MaxItems, globals.OverFetch, globalsCap, globalsCap*2)
	}
	if project.Order == globals.Order {
		t.Errorf("both buckets order by %q; the disagreement between them is policy, not drift, and one order means "+
			"one of the two changed", project.Order)
	}
	// The one real SELECTION difference between the buckets, and the reason the
	// threshold differs too: the project bucket reorders, the global bucket
	// drops. Equal thresholds would mean the second half went missing.
	if project.DropDemotedLosers {
		t.Error("the project slice drops its demoted losers, which the shipped loader did not: a demotion is a " +
			"reorder there, and a dropped row is a memory the block stopped offering")
	}
	if !globals.DropDemotedLosers {
		t.Error("the global slice does not drop its demoted losers, which the shipped loader did: a superseded " +
			"preference is not worth one of eight cross-project slots")
	}
	if globals.DemotionThreshold == project.DemotionThreshold {
		t.Errorf("both buckets demote at %v; the global bucket's threshold is deliberately lower (a live pair of "+
			"global preferences was observed linking just under the general one)", globals.DemotionThreshold)
	}
}

// TestTheSessionBudgetKeepsTheGlobalsWhenNoProjectMatched pins the one case the
// old shape got for free. loadGlobals ran at the hook level, so a directory that
// matched no project still rendered a Global section; a budget carrying only the
// project slice would silently drop it, and the block a user gets in a directory
// Ghost does not know would lose the cross-project rows it has always shown.
func TestTheSessionBudgetKeepsTheGlobalsWhenNoProjectMatched(t *testing.T) {
	budget := sessionPassiveBudget(mustHookConfig(t), "")
	if len(budget.Slices) != 1 {
		t.Fatalf("an unmatched directory produced %d slices, want 1", len(budget.Slices))
	}
	if budget.Slices[0].Bucket != "_global" {
		t.Errorf("the slice an unmatched directory keeps is %q, want the global bucket", budget.Slices[0].Bucket)
	}
}

// memoryProjections returns, for every query in text that reads the memories
// table, the projection it selected — whitespace-normalised, so the caller can
// compare it to a literal.
//
// The projection is found by walking BACK from each `FROM memories` to the
// nearest preceding `SELECT`, rather than by matching the whole statement in one
// pattern. A single pattern has to be told where a statement ends, and every rule
// for that is either a lookahead (which Go's regexp does not have) or a guess at
// the syntax in between; the backward walk has no such rule to get wrong, and it
// reports a statement that has no SELECT at all as an empty projection, which is
// itself a row fetch the caller should hear about.
func memoryProjections(text string) []string {
	var out []string
	const from = "FROM memories"
	for i := 0; ; {
		j := strings.Index(text[i:], from)
		if j < 0 {
			return out
		}
		j += i
		sel := strings.LastIndex(text[:j], "SELECT")
		projection := ""
		if sel >= 0 {
			projection = text[sel+len("SELECT") : j]
		}
		out = append(out, strings.Join(strings.Fields(projection), " "))
		i = j + len(from)
	}
}

// TestAFailedSessionStartReadIsReportedNotSilent is the regression test for a
// finding this PR's review raised, and it is the half of the finding that is
// reachable from here.
//
// The private loaders printed four failures on stderr — a schema version that
// could not be read, and three demotion-lookup failures — through handles this
// read still uses. The switch replaced them with an `io.Discard` store logger
// and a `slog.Debug`, and NEITHER reaches a user: nothing on the hook path calls
// slog.SetDefault, so the process default handler runs at Info and drops a Debug
// record without formatting it, and a discard handler drops everything. The block
// then renders exactly as a healthy store's would.
//
// Fail-open is the correct answer here and is preserved — a broken store must
// cost a session nothing rather than block it — and silence about WHY is what
// makes it correct. A store that cannot read its schema version silently drops
// its scope filter, its tier label and its expiry window; one that cannot read
// its demotion lookup re-offers a superseded preference and can show a
// near-duplicate pair as two independent lines. Nothing in the rendered output
// distinguishes either from the truth, which is why the code this replaced said
// so out loud.
//
// The trigger is a closed handle: the cheapest way to make a store this build
// genuinely cannot read. The capture goes through slog.SetDefault rather than by
// swapping os.Stderr, because the default logger binds its writer once at
// package init and re-reads nothing — which is also the mechanism this assertion
// is really about, since a reader that swaps the variable cannot redirect it.
func TestAFailedSessionStartReadIsReportedNotSilent(t *testing.T) {
	db, _ := openFileTestDB(t)
	store := sessionStore(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var got bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&got, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	// nil sink: this test asserts on the block a FAILED read produces, and a record
	// is not written for an errored Run in the first place. See passive_record_test.go
	// for the recording path.
	memories, globals, _ := loadSessionPassive(context.Background(), store, mustHookConfig(t), "psomeproj", time.Now(), nil, "")

	if len(memories) != 0 || len(globals) != 0 {
		t.Errorf("a failed read returned %d project and %d global rows, want none — the block is "+
			"rendered from what the read found, and a broken store found nothing", len(memories), len(globals))
	}
	if !strings.Contains(got.String(), "assembly failed") {
		t.Errorf("the session-start assembly failed and nothing said so; the log held:\n%s", got.String())
	}
	// The level, not just the record. This is the assertion a Debug-logger fix
	// fails, and it is here because the failure is silent for a reason that has
	// nothing to do with the message: the process default handler runs at Info, so
	// a Debug record is dropped before it is ever formatted.
	if strings.Contains(got.String(), "level=DEBUG") {
		t.Errorf("a session-start diagnosis was logged at DEBUG, which the default handler drops: %s", got.String())
	}
}

// TestTheSessionStoreIsNotLoggingIntoTheVoid covers the half of the review
// finding that no behavioural test can reach.
//
// A closed handle proves the assembly failure is reported, but it fails at "begin
// read snapshot" and so never reaches the store's own three Warn sites — the
// schema version it could not read, and the two demotion lookups. Those are the
// diagnoses that matter most, because each one degrades the block in a way
// nothing downstream can see: an unreadable version silently drops the scope
// filter, the tier label and the expiry window, and an unreadable demotion
// lookup re-offers a superseded preference and can show a near-duplicate pair as
// two independent lines.
//
// So this one is a source assertion, and deliberately. A store built with an
// `io.Discard` logger is invisible in a diff, fails no other test in the tree,
// and its only symptom is a block that looks healthy. Asserting that the
// handler's LEVEL is Warn would be asserting a constant against itself.
//
// It is scoped to sessionStore and not to the package, because a package-wide
// sweep is a different claim: init.go, status.go, stophook.go and plugin.go have
// always built discarding loggers, they never render a session block, and
// flagging them would be a test that fails on main for something this PR did not
// do. The claim worth making is the narrow one — THIS store, the one a degraded
// read would go quiet on.
func TestTheSessionStoreIsNotLoggingIntoTheVoid(t *testing.T) {
	body := funcBody(t, mcpinitSource(t)["session_passive.go"], "sessionStore")
	if strings.Contains(body, "io.Discard") {
		t.Errorf("sessionStore hands the store a discarding logger; a store this read cannot diagnose "+
			"renders a block byte-identical to a healthy one and says nothing. The body was:\n%s", body)
	}
}

// funcBody returns the text of the named top-level function, from its `func`
// line to the first line that closes it at column zero — which is how gofmt
// writes every top-level declaration in this tree, so no parsing is needed and
// none of this test's value depends on the Go type checker.
//
// The line ending is stripped on every line, and that is not defensive
// decoration: a checkout on Windows with autocrlf ends every line in \r, so the
// closing brace reads "}\r" and a test looking for "}" never finds it. That is
// not a hypothetical — it is how this helper failed the windows-plugin job the
// first time, while passing everywhere else.
func funcBody(t *testing.T, source, name string) string {
	t.Helper()
	lines := strings.Split(source, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimRight(l, "\r"), "func "+name+"(") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no top-level func %s in the source read", name)
	}
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimRight(lines[j], "\r") == "}" {
			return strings.Join(lines[start:j+1], "\n")
		}
	}
	t.Fatalf("func %s is not closed in the source read", name)
	return ""
}

// seedByteCapStore builds a store whose uncapped session-start block is well over
// the host's 10,000-character limit: a project with pinned rows, many unpinned
// rows, a learned summary, four tasks and three decisions, and eight globals.
func seedByteCapStore(t *testing.T) (dbPath, projectPath string, pinned []string) {
	t.Helper()
	ghostDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath = filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	projectPath = filepath.Join(t.TempDir(), "bigproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	exec(`INSERT INTO projects (id, path, name) VALUES ('pbig', ?, 'bigproj')`, canonical)
	exec(`INSERT INTO ghost_state (project_id, learned_context) VALUES ('pbig', ?)`, strings.Repeat("Learned summary sentence. ", 20))

	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	insert := func(id, project, category string, content string, imp float64, pin int) {
		exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, ?)`, id, project, category, content, imp, pin, ts, ts)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("ppin%02d", i)
		pinned = append(pinned, id)
		insert(id, "pbig", "preference", strings.Repeat(fmt.Sprintf("Pinned %02d content. ", i), 15), 0.2, 1)
	}
	for i := 0; i < 30; i++ {
		insert(fmt.Sprintf("pmem%02d", i), "pbig", "gotcha", strings.Repeat(fmt.Sprintf("Unpinned %02d content. ", i), 15), 0.9-float64(i)*0.01, 0)
	}
	for i := 0; i < 10; i++ {
		insert(fmt.Sprintf("gmem%02d", i), "_global", "preference", strings.Repeat(fmt.Sprintf("Global %02d content. ", i), 20), 0.7, 0)
	}
	for i := 0; i < 4; i++ {
		exec(`INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, 'pbig', ?, ?, 'pending', 1)`,
			fmt.Sprintf("task%02d", i), strings.Repeat("Task title words ", 10), strings.Repeat("Task description. ", 15))
	}
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status) VALUES (?, 'pbig', ?, ?, 'why', 'active')`,
			fmt.Sprintf("dec%02d", i), strings.Repeat("Decision title words ", 8), strings.Repeat("Decision body. ", 20))
	}
	return dbPath, projectPath, pinned
}

// TestSessionStartByteCapTrimsLargeBlockAndKeepsPinned: a ~12 KB store renders a
// block under the cap that holds every pinned row, the cap is on the bytes of the
// WHOLE rendered block (framing included), and every cut row is recorded as cut
// with its reason rather than as delivered.
func TestSessionStartByteCapTrimsLargeBlockAndKeepsPinned(t *testing.T) {
	dbPath, projectPath, pinned := seedByteCapStore(t)
	cfg := mustHookConfig(t)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

	block := SessionBlockAt(dbPath, projectPath, cfg, now)

	if len(block) > sessionStartByteCap {
		t.Errorf("block is %d bytes, over the %d cap", len(block), sessionStartByteCap)
	}
	if len(block) >= 10000 {
		t.Errorf("block is %d bytes, at or over the host's 10,000 limit", len(block))
	}
	for _, id := range pinned {
		if !strings.Contains(block, id) {
			t.Errorf("pinned row %s is not in the block", id)
		}
	}
	if !strings.Contains(block, "cut to keep this block under the host's output limit") {
		t.Errorf("the count line does not say rows were cut for size:\n%s", block)
	}
	if !strings.Contains(block, "task00") || !strings.Contains(block, "dec00") {
		t.Errorf("tasks and decisions are missing from the block")
	}

	// What the block shows and what the record says were delivered must agree.
	shown := 0
	for _, prefix := range []string{"pmem", "gmem", "ppin"} {
		shown += strings.Count(block, "`"+prefix) + strings.Count(block, "["+prefix)
	}
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var verdicts string
	if err := db.QueryRow(`SELECT verdicts FROM retrieval_record WHERE source = 'session_start' ORDER BY rowid DESC LIMIT 1`).Scan(&verdicts); err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rows []struct {
		ID     string `json:"id"`
		Kept   bool   `json:"kept"`
		Stage  string `json:"stage"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(verdicts), &rows); err != nil {
		t.Fatalf("decode verdicts %q: %v", verdicts, err)
	}
	cut, kept := 0, 0
	for _, r := range rows {
		inBlock := strings.Contains(block, r.ID)
		if r.Kept {
			kept++
			if !inBlock {
				t.Errorf("record says %s was delivered, the block does not hold it", r.ID)
			}
			continue
		}
		if inBlock {
			t.Errorf("record says %s was cut (%s), the block holds it", r.ID, r.Stage)
		}
		if r.Stage == "response_fit" {
			cut++
			if r.Reason == "" {
				t.Errorf("cut row %s has no reason", r.ID)
			}
		}
	}
	if cut == 0 {
		t.Fatalf("no response_fit cut recorded; verdicts: %s", verdicts)
	}
	for _, id := range pinned {
		for _, r := range rows {
			if r.ID == id && !r.Kept {
				t.Errorf("pinned row %s recorded as cut", id)
			}
		}
	}
	if kept <= len(pinned) || kept+cut < 23 {
		t.Errorf("expected unpinned rows delivered beside the pinned ones and 23 judged: kept %d cut %d", kept, cut)
	}
	t.Logf("block %d bytes, %d delivered, %d cut for size", len(block), kept, cut)
}

// A store that fits the cap loses nothing: no cut is recorded and no count-line
// clause about size appears.
func TestSessionStartUnderTheByteCapCutsNothing(t *testing.T) {
	dbPath, projectPath, _ := seedByteCapStore(t)
	db, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, q := range []string{
		`DELETE FROM tasks`, `DELETE FROM decisions`, `UPDATE ghost_state SET learned_context = ''`,
		`DELETE FROM memories WHERE id LIKE 'pmem%' AND id > 'pmem03'`,
		`DELETE FROM memories WHERE id LIKE 'gmem%' AND id > 'gmem01'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = db.Close()
	block := SessionBlockAt(dbPath, projectPath, mustHookConfig(t), time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
	if len(block) > sessionStartByteCap/2 {
		t.Fatalf("fixture is not small: %d bytes", len(block))
	}
	if strings.Contains(block, "output limit") {
		t.Errorf("a block under the cap mentions a size cut:\n%s", block)
	}
	for _, id := range []string{"pmem00", "pmem03", "gmem00", "gmem01", "ppin00"} {
		if !strings.Contains(block, id) {
			t.Errorf("%s is missing from a block under the cap", id)
		}
	}
}
