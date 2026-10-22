package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ftsPlanLines returns EXPLAIN QUERY PLAN's detail lines for one statement.
func ftsPlanLines(t *testing.T, s *Store, query string, args []any) []string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return lines
}

// seedFTSBucketStore is a small project, a much larger foreign project and a
// _global row, all sharing their common words, so a statement that ranks the
// whole store's matches has something to rank.
func seedFTSBucketStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "foreign", "/x/foreign", "foreign"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ins := func(project, content string) {
		if _, err := s.db.Exec(`INSERT INTO memories (project_id, category, content, source) VALUES (?, 'fact', ?, 'manual')`, project, content); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		ins(testProject, fmt.Sprintf("the deploy is to staging small %d", i))
	}
	for i := 0; i < 200; i++ {
		ins("foreign", fmt.Sprintf("the deploy is to staging foreign %d", i))
	}
	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	ins(GlobalProjectID, "the deploy is to staging everywhere")
	return s
}

// TestFTSStatementsBoundTheScanToTheProject pins the plan of every project-scoped
// full-text statement: the project restriction is applied INSIDE the FTS5 scan
// (a rowid range on the virtual table, shown as '>' and '<' in its index string),
// and memories is reached only by rowid from the FTS rows. Before the bound the
// plan was `SCAN f VIRTUAL TABLE INDEX 0:M1`, which enumerates and ranks every
// project's matches before the project filter and the LIMIT apply, so a save's
// write-lock hold grew with the rows of other projects.
func TestFTSStatementsBoundTheScanToTheProject(t *testing.T) {
	s := seedFTSBucketStore(t)
	match := sanitizeFTSN("the deploy is to staging", 30, nil)
	scope := scopeJSON(map[string]string{"env": "prod"})
	scoped := "AND NOT " + scopesConflictSQL("m.scope", "?")
	now := time.Now()

	type stmt struct {
		name  string
		query string
		args  []any
	}
	var stmts []stmt
	add := func(name, q string, args []any) { stmts = append(stmts, stmt{name, q, args}) }
	q, a := dedupLookupStatement(false, "", nil, testProject, "fact", match)
	add("dedup same-category", q, a)
	q, a = dedupLookupStatement(true, "", nil, testProject, "fact", match)
	add("dedup cross-category", q, a)
	q, a = dedupLookupStatement(false, scoped, []any{scope, scope}, testProject, "fact", match)
	add("dedup same-category scoped", q, a)
	q, a = dedupLookupStatement(true, scoped+" AND "+replaceableRowSQL, []any{scope, scope}, testProject, "fact", match)
	add("dedup cross-category scoped replaceable", q, a)
	q, a = dedupLookupStatement(false, "", nil, GlobalProjectID, "fact", match)
	add("dedup same-category global save", q, a)
	q, a = ftsSearchStatement(testProject, match, 10, time.Time{}, false)
	add("search", q, a)
	q, a = ftsSearchStatement(testProject, match, 10, now, false)
	add("search validating", q, a)
	q, a = ftsSearchStatement(GlobalProjectID, match, 10, now, false)
	add("search from global", q, a)
	q, a = keywordValidityExcludedStatement(testProject, match, now, false)
	add("keyword validity count", q, a)

	for _, st := range stmts {
		t.Run(st.name, func(t *testing.T) {
			lines := ftsPlanLines(t, s, st.query, st.args)
			plan := strings.Join(lines, "\n")
			var ftsScans int
			for _, l := range lines {
				if strings.HasPrefix(l, "SCAN f VIRTUAL TABLE") || strings.HasPrefix(l, "SCAN memories_fts VIRTUAL TABLE") {
					ftsScans++
					idx := l[strings.LastIndex(l, ":")+1:]
					if !strings.Contains(idx, ">") || !strings.Contains(idx, "<") {
						t.Errorf("the FTS5 scan is not bounded to a project's rowid range: %q\nplan:\n%s", l, plan)
					}
				}
				if strings.HasPrefix(l, "SCAN m ") || l == "SCAN m" || strings.HasPrefix(l, "SCAN memories ") || l == "SCAN memories" {
					t.Errorf("memories is scanned rather than reached by rowid: %q\nplan:\n%s", l, plan)
				}
			}
			if ftsScans == 0 {
				t.Fatalf("no FTS5 scan in the plan:\n%s", plan)
			}
			// And the statement runs, which a plan alone does not prove.
			rows, err := s.db.Query(st.query, st.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			_ = rows.Close()
		})
	}
}

// ftsStoreWideSearchSQL is the keyword search as a store-wide scan: every
// project's matches enumerated and ranked, then the project filter. It is the
// reference the bucketed statements must agree with row for row and in order.
const ftsStoreWideSearchSQL = `
	SELECT m.id
	FROM memories_fts f
	CROSS JOIN memories m ON m.rowid = (f.rowid & ` + ftsRowidMask + `)
	WHERE memories_fts MATCH ?
	  AND (m.project_id = ? OR m.project_id = '_global')
	ORDER BY f.rank, m.importance DESC, m.rowid
	LIMIT ?`

// ftsStoreWideDedupSQL is the same-category duplicate lookup as a store-wide
// scan, the reference for the bucketed dedupLookupStatement.
const ftsStoreWideDedupSQL = `
	SELECT m.id
	FROM memories_fts f
	CROSS JOIN memories m ON m.rowid = (f.rowid & ` + ftsRowidMask + `)
	WHERE memories_fts MATCH ?
	  AND m.project_id = ?
	  AND m.category = ?
	ORDER BY f.rank, m.importance DESC, m.rowid
	LIMIT 15`

func queryIDs(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

// assertFTSInSync checks the index against its content: FTS5's own integrity
// check (which, with the rank argument, compares the index with the content
// view), one FTS row per memory, and exactly one bucket per project memories
// holds.
func assertFTSInSync(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO memories_fts(memories_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Fatalf("FTS integrity check: %v", err)
	}
	var fts, mem int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories_fts`).Scan(&fts); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM memories`).Scan(&mem); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if fts != mem {
		t.Errorf("memories_fts holds %d rows, memories %d", fts, mem)
	}
	var stray int
	if err := s.db.QueryRow(`
		SELECT (SELECT count(*) FROM memories_fts_projects k
		        WHERE NOT EXISTS (SELECT 1 FROM memories m WHERE m.project_id = k.project_id))
		     + (SELECT count(DISTINCT project_id) FROM memories m
		        WHERE NOT EXISTS (SELECT 1 FROM memories_fts_projects k WHERE k.project_id = m.project_id))
	`).Scan(&stray); err != nil {
		t.Fatalf("compare buckets: %v", err)
	}
	if stray != 0 {
		t.Errorf("memories_fts_projects and memories disagree on %d project(s)", stray)
	}
}

// TestBucketedFTSMatchesTheStoreWideScan: the bucketed statements find the same
// rows in the same order as a store-wide scan filtered afterwards, through every
// write that moves a row between buckets or out of the index — an insert, a
// content edit, a promotion to _global, a delete, a project delete — with tied
// ranks inside a project and across a project and _global.
func TestBucketedFTSMatchesTheStoreWideScan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, p := range []string{"foreign", "other", GlobalProjectID} {
		if err := s.EnsureProject(ctx, p, "/x/"+p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	ids := map[string]string{}
	ins := func(key, project, category, content string, importance float64) {
		var id string
		if err := s.db.QueryRow(`INSERT INTO memories (project_id, category, content, source, importance) VALUES (?, ?, ?, 'manual', ?) RETURNING id`,
			project, category, content, importance).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
		ids[key] = id
	}
	// Ties: the same text at the same importance in three projects, twice in one.
	for i, p := range []string{testProject, "foreign", GlobalProjectID, testProject, "other", GlobalProjectID} {
		ins(fmt.Sprintf("tie%d", i), p, "fact", "the deploy runs to staging nightly", 0.5)
	}
	for i := 0; i < 40; i++ {
		ins(fmt.Sprintf("foreign%d", i), "foreign", "fact", fmt.Sprintf("the deploy is to staging cluster %d", i), 0.4)
		ins(fmt.Sprintf("other%d", i), "other", "gotcha", fmt.Sprintf("staging deploy breaks the cache %d", i), 0.6)
	}
	for i := 0; i < 8; i++ {
		ins(fmt.Sprintf("own%d", i), testProject, []string{"fact", "gotcha"}[i%2], fmt.Sprintf("the staging deploy needs a cache warmup step %d", i), 0.3+float64(i)/20)
	}
	ins("moved", testProject, "fact", "promoted zebra note about staging deploy", 0.7)
	ins("edited", "foreign", "fact", "original wording quagga", 0.5)
	ins("gone", testProject, "fact", "doomed okapi note about staging", 0.5)

	queries := []string{"the deploy is to staging", "staging cache", "zebra", "quagga", "okapi", "rewritten", "nightly"}
	check := func(stage string) {
		t.Helper()
		assertFTSInSync(t, s)
		for _, raw := range queries {
			match := sanitizeFTS(raw, nil)
			for _, p := range []string{testProject, "foreign", "other", GlobalProjectID, "no-such-project"} {
				want := queryIDs(t, s, ftsStoreWideSearchSQL, match, p, 50)
				got, err := s.SearchFTS(ctx, p, raw, 50)
				if err != nil {
					t.Fatalf("%s: SearchFTS(%s, %q): %v", stage, p, raw, err)
				}
				if g := memoryIDs(got); strings.Join(g, ",") != strings.Join(want, ",") {
					t.Errorf("%s: SearchFTS(%s, %q)\n got %v\nwant %v", stage, p, raw, g, want)
				}
				for _, cat := range []string{"fact", "gotcha"} {
					dmatch := sanitizeFTSN(raw, 30, nil)
					want := queryIDs(t, s, ftsStoreWideDedupSQL, dmatch, p, cat)
					q, a := dedupLookupStatement(false, "", nil, p, cat, dmatch)
					got := queryIDs(t, s, "SELECT id FROM ("+q+")", a...)
					if strings.Join(got, ",") != strings.Join(want, ",") {
						t.Errorf("%s: dedup lookup (%s, %s, %q)\n got %v\nwant %v", stage, p, cat, raw, got, want)
					}
				}
			}
			n, err := s.keywordValidityExcluded(ctx, CandidateRequest{Query: raw, Now: time.Now()}, testProject, false)
			if err != nil {
				t.Fatalf("%s: keywordValidityExcluded: %v", stage, err)
			}
			if n != 0 {
				t.Errorf("%s: keywordValidityExcluded(%q) = %d, want 0 (no row has a window)", stage, raw, n)
			}
		}
	}
	check("inserted")

	if err := s.PromoteToGlobal(ctx, testProject, ids["moved"]); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET content = 'rewritten wording' WHERE id = ?`, ids["edited"]); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := s.Delete(ctx, ids["gone"]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	check("moved, edited, deleted")
	// The promoted row answers from _global (and so from every project's scope)
	// and the edited row by its new words only.
	if got, _ := s.SearchFTS(ctx, "foreign", "zebra", 5); len(got) != 1 || got[0].ID != ids["moved"] {
		t.Errorf("the promoted row is not found from another project's scope: %v", memoryIDs(got))
	}
	if got, _ := s.SearchFTS(ctx, "foreign", "quagga", 5); len(got) != 0 {
		t.Errorf("the edited row still matches its old wording: %v", memoryIDs(got))
	}

	if _, err := s.DeleteProject(ctx, "other", true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	check("project deleted")
	var buckets int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories_fts_projects WHERE project_id = 'other'`).Scan(&buckets); err != nil || buckets != 0 {
		t.Errorf("a deleted project keeps its bucket: n=%d err=%v", buckets, err)
	}

	// A project that comes back gets a bucket again and is indexed under it.
	if err := s.EnsureProject(ctx, "other", "/x/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ins("back", "other", "fact", "returning yak note about staging", 0.5)
	check("project recreated")
	if got, _ := s.SearchFTS(ctx, "other", "yak", 5); len(got) != 1 || got[0].ID != ids["back"] {
		t.Errorf("a recreated project's row is not found: %v", memoryIDs(got))
	}
}

// TestSaveDedupFollowsTheRowsBucket: a save folds into a duplicate in its own
// project and never into one in another project, and a promoted row is a
// duplicate candidate for _global saves — and no longer for its old project's.
// The project predicate always said so; with the bucket bound it is the index
// that has to, so a move the triggers missed would show here.
func TestSaveDedupFollowsTheRowsBucket(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, p := range []string{"foreign", GlobalProjectID} {
		if err := s.EnsureProject(ctx, p, "/x/"+p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	const text = "the nightly staging deploy restarts the ingest workers after the schema check"
	foreignID, _, _, err := s.Upsert(ctx, "foreign", "fact", text, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert foreign: %v", err)
	}
	ownID, dup, _, err := s.Upsert(ctx, testProject, "fact", text, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert own: %v", err)
	}
	if dup != "" {
		t.Fatalf("a save folded into another project's row %s", dup)
	}
	if _, dup, _, err = s.Upsert(ctx, testProject, "fact", text, "manual", 0.5, nil); err != nil || dup != ownID {
		t.Fatalf("a re-save did not fold into its own project's row: dup=%q want %q err=%v", dup, ownID, err)
	}
	// Cross-category: the same text as a gotcha folds into the project's fact.
	if _, dup, _, err = s.Upsert(ctx, testProject, "gotcha", text, "manual", 0.5, nil); err != nil || dup != ownID {
		t.Fatalf("a cross-category re-save did not fold into its own project's row: dup=%q want %q err=%v", dup, ownID, err)
	}
	if _, dup, _, err = s.Upsert(ctx, GlobalProjectID, "fact", text, "manual", 0.5, nil); err != nil {
		t.Fatalf("Upsert global: %v", err)
	} else if dup != "" {
		t.Fatalf("a _global save folded into a project row %s", dup)
	}

	// Promote the foreign row: it leaves the foreign bucket for _global's.
	if err := s.EnsureProject(ctx, "promo", "/x/promo", "promo"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const promoText = "the promotion candidate explains how the ingest workers warm their caches"
	promoID, _, _, err := s.Upsert(ctx, "promo", "fact", promoText, "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert promo: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, "promo", promoID); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if _, dup, _, err = s.Upsert(ctx, GlobalProjectID, "fact", promoText, "manual", 0.5, nil); err != nil || dup != promoID {
		t.Fatalf("a _global save did not fold into the promoted row: dup=%q want %q err=%v", dup, promoID, err)
	}
	if _, dup, _, err = s.Upsert(ctx, "promo", "fact", promoText, "manual", 0.5, nil); err != nil {
		t.Fatalf("Upsert promo again: %v", err)
	} else if dup == promoID {
		t.Fatalf("a save in the old project folded into the row that left it")
	}
	_ = foreignID
	assertFTSInSync(t, s)
}
