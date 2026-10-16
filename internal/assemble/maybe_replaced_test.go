package assemble

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The pgx pair the version-upgrade storyline saves, word for word: one save
// changes the value of the pin, the save path refuses to fold it (the value
// differs), and with no lifecycle pass the two are unlinked rows.
const (
	pgxV4 = "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere."
	pgxV5 = "go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5."
)

// replacementStore is a real store with one project, so the passive retrieval,
// its edge read and its order are the production ones.
type replacementStore struct {
	t  *testing.T
	db *sql.DB
	s  *memory.Store
}

func newReplacementStore(t *testing.T) *replacementStore {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(context.Background(), "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return &replacementStore{t: t, db: db, s: s}
}

// add saves a row created at the given day of September 2026 and returns its id.
// created_at is written directly, because the chronology is the fact under test
// and Create stamps the wall clock.
func (r *replacementStore) add(m memory.Memory, day int) string {
	r.t.Helper()
	if m.Source == "" {
		m.Source = "mcp"
	}
	id, err := r.s.Create(context.Background(), "proj", m)
	if err != nil {
		r.t.Fatalf("Create(%q): %v", m.Content, err)
	}
	stamp := time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(memory.StoredStampLayout)
	if _, err := r.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
		r.t.Fatalf("stamp %s: %v", id, err)
	}
	return id
}

func (r *replacementStore) link(from, to, relation string) {
	r.t.Helper()
	if err := r.s.CreateLink(context.Background(), from, to, relation, 1.0, "manual"); err != nil {
		r.t.Fatalf("CreateLink(%s -> %s, %s): %v", from, to, relation, err)
	}
}

func replacementPassiveRequest() Request {
	req := passiveRequest()
	req.Budget.Slices = []Slice{{Bucket: "proj", MaxItems: 10, ClampBytes: 400}}
	return req
}

func dep(content string, importance float32) memory.Memory {
	return memory.Memory{Category: "dependency", Content: content, Importance: importance}
}

func indexOf(items []Item, id string) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

func itemByID(t *testing.T, items []Item, id string) Item {
	t.Helper()
	if i := indexOf(items, id); i >= 0 {
		return items[i]
	}
	t.Fatalf("%s is not in the answer %v", id, itemIDs(items))
	return Item{}
}

func assertUnmarked(t *testing.T, items []Item) {
	t.Helper()
	for _, it := range items {
		if len(it.MaybeReplacedBy) > 0 || strings.Contains(it.Line(), MaybeReplacedMarker) {
			t.Errorf("%s is marked as maybe replaced by %v: %s", it.ID, it.MaybeReplacedBy, it.Line())
		}
	}
}

// TestRunPassiveOrdersAValueReplacementFirstAndMarksTheOlderRow is the defect a
// default install shows at session start: the replaced pin ranks first (it is
// the more important row, and nothing relates the two), and neither line says
// which one is current. The newer row must come first and the older one must
// say it may have been replaced, naming the row that replaced it. The older row
// is marked, not withheld.
func TestRunPassiveOrdersAValueReplacementFirstAndMarksTheOlderRow(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(pgxV4, 0.8), 20)
	newer := r.add(dep(pgxV5, 0.7), 24)

	res := run(t, r.s, replacementPassiveRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (older %s, newer %s): the older row is marked, not withheld", itemIDs(res.Items), older, newer)
	}
	if ni > oi {
		t.Errorf("answer %v puts the replaced pin %s (index %d) ahead of its replacement %s (index %d)", itemIDs(res.Items), older, oi, newer, ni)
	}
	old := res.Items[oi]
	if len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
	line := old.Line()
	prefix, _, _ := strings.Cut(line, "«")
	if !strings.Contains(prefix, MaybeReplacedMarker) || !strings.Contains(prefix, "maybe_replaced_by=`"+newer+"`") {
		t.Errorf("older line does not carry the marker and the replacement's id in its prefix: %s", line)
	}
	if n := res.Items[ni]; len(n.MaybeReplacedBy) > 0 || strings.Contains(n.Line(), MaybeReplacedMarker) {
		t.Errorf("the replacement itself is marked: %s", n.Line())
	}
}

// TestRunPassiveDoesNotPairRowsWhoseScopesDiffer: one setting with a different
// value in staging and in production is two settings, not a replacement, however
// similar the text. The scoped-setting storyline depends on it. Both rows name
// the same setting (DB_POOL), so the scope rule is the only one keeping them
// apart.
func TestRunPassiveDoesNotPairRowsWhoseScopesDiffer(t *testing.T) {
	r := newReplacementStore(t)
	staging := memory.Memory{Category: "fact", Importance: 0.8, Scope: map[string]string{"env": "staging"},
		Content: "DB_POOL is capped at max_conns=24; the load test showed extra connections only queue at the small database."}
	production := memory.Memory{Category: "fact", Importance: 0.7, Scope: map[string]string{"env": "production"},
		Content: "DB_POOL is capped at max_conns=180; below that the request queue saturates during the evening peak."}
	a := r.add(staging, 20)
	b := r.add(production, 24)
	res := run(t, r.s, replacementPassiveRequest())
	if indexOf(res.Items, a) < 0 || indexOf(res.Items, b) < 0 {
		t.Fatalf("answer %v must hold both scoped rows", itemIDs(res.Items))
	}
	if indexOf(res.Items, a) > indexOf(res.Items, b) {
		t.Errorf("answer %v reordered two rows of different scopes", itemIDs(res.Items))
	}
	assertUnmarked(t, res.Items)
}

// TestRunPassiveDoesNotPairARestatement: a newer row that only ADDS to the
// older one states no different value, so nothing replaced anything.
func TestRunPassiveDoesNotPairARestatement(t *testing.T) {
	r := newReplacementStore(t)
	a := r.add(memory.Memory{Category: "fact", Importance: 0.8, Content: "The cache TTL is 300 seconds for every tenant."}, 20)
	b := r.add(memory.Memory{Category: "fact", Importance: 0.7, Content: "The cache TTL is 300 seconds for every tenant, set in config.yaml."}, 24)
	res := run(t, r.s, replacementPassiveRequest())
	if got := itemIDs(res.Items); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("answer %v, want the rank order [%s %s] unchanged", got, a, b)
	}
	assertUnmarked(t, res.Items)
}

// TestRunPassiveDoesNotPairAnUnrelatedRowWithAVersion: a version number is a
// value token, but a row about something else shares too little text with the
// pin to be its replacement.
func TestRunPassiveDoesNotPairAnUnrelatedRowWithAVersion(t *testing.T) {
	r := newReplacementStore(t)
	r.add(memory.Memory{Category: "architecture", Importance: 0.9, Content: "quill-ledger is the double-entry ledger service; it talks to Postgres 16."}, 20)
	r.add(dep(pgxV5, 0.7), 24)
	r.add(dep("The CI image pins golang 1.24.3 and runs go vet before the unit tests.", 0.6), 25)
	res := run(t, r.s, replacementPassiveRequest())
	assertUnmarked(t, res.Items)
}

// TestRunPassiveDoesNotPairParallelFactsAboutDifferentSubjects: two notes in
// one template about two different things share almost every word and differ in
// the names, which is exactly what a value swap looks like to the similarity
// bar. Neither replaces the other, because they name no common subject.
func TestRunPassiveDoesNotPairParallelFactsAboutDifferentSubjects(t *testing.T) {
	r := newReplacementStore(t)
	a := r.add(memory.Memory{Category: "architecture", Importance: 0.8, Content: "auth-service runs on node-3 behind the internal load balancer."}, 20)
	b := r.add(memory.Memory{Category: "architecture", Importance: 0.7, Content: "billing-service runs on node-7 behind the internal load balancer."}, 24)
	res := run(t, r.s, replacementPassiveRequest())
	if got := itemIDs(res.Items); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("answer %v, want the rank order [%s %s] unchanged", got, a, b)
	}
	assertUnmarked(t, res.Items)
}

// TestRunPassiveLeavesAnExplicitlyRelatedPairToItsOwnHandling: a live
// `supersedes` or `contradicts` edge between the two rows is a decision someone
// made, and its own handling (the supersede demotion, stage 5's separation)
// wins over a guess read off the text.
func TestRunPassiveLeavesAnExplicitlyRelatedPairToItsOwnHandling(t *testing.T) {
	for _, relation := range []string{"supersedes", "contradicts"} {
		t.Run(relation, func(t *testing.T) {
			r := newReplacementStore(t)
			older := r.add(dep(pgxV4, 0.8), 20)
			newer := r.add(dep(pgxV5, 0.7), 24)
			r.link(newer, older, relation)
			res := run(t, r.s, replacementPassiveRequest())
			assertUnmarked(t, res.Items)
		})
	}
}

// TestRunPassiveIgnoresAResolvedRow: a resolved row already says it is retired.
func TestRunPassiveIgnoresAResolvedRow(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(pgxV4, 0.8), 20)
	r.add(dep(pgxV5, 0.7), 24)
	if _, err := r.db.Exec(`UPDATE memories SET resolved_at = '2026-09-25 10:00:00' WHERE id = ?`, older); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	res := run(t, r.s, replacementPassiveRequest())
	assertUnmarked(t, res.Items)
}

// TestRunPassiveMarksAPinnedOlderRowWithoutMovingIt: a pin is a slot guarantee,
// and the only relation the passive order lets reorder a pin is an explicit
// `supersedes` edge, which reserves its replacement a slot. A lexical guess gets
// what a newer contradicting row gets: the pinned row keeps its place and says
// a newer row on the page may have replaced it.
func TestRunPassiveMarksAPinnedOlderRowWithoutMovingIt(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(pgxV4, 0.3), 20)
	newer := r.add(dep(pgxV5, 0.9), 24)
	if _, err := r.db.Exec(`UPDATE memories SET pinned = 1 WHERE id = ?`, older); err != nil {
		t.Fatalf("pin: %v", err)
	}
	res := run(t, r.s, replacementPassiveRequest())
	if got := itemIDs(res.Items); len(got) != 2 || got[0] != older {
		t.Errorf("answer %v, want the pinned row %s first, where its pin put it", got, older)
	}
	if old := itemByID(t, res.Items, older); len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("pinned older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
}

// TestRunQueryMarksAValueReplacementInRelevanceOrder: ghost_memory_search
// renders through the same Item.Line, so the older row carries the same marker
// there. The order is the relevance answer and is left as the ranking made it.
func TestRunQueryMarksAValueReplacementInRelevanceOrder(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(pgxV4, 0.8), 20)
	newer := r.add(dep(pgxV5, 0.7), 24)
	req := baseRequest()
	req.Query = "pgxpool.Connect pgx v4"
	req.Budget = Budget{MaxItems: 5}
	res := run(t, r.s, req)
	if indexOf(res.Items, older) < 0 || indexOf(res.Items, newer) < 0 {
		t.Fatalf("answer %v must hold both rows", itemIDs(res.Items))
	}
	if old := itemByID(t, res.Items, older); len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
}

// TestRunMarksOnlyAReplacementOnThePage: the marker names a newer row only while
// that row is in the answer, so a reader is never pointed at a row they cannot
// see — and a row whose every replacement is gone is not marked at all.
func TestRunMarksOnlyAReplacementOnThePage(t *testing.T) {
	newer := projectCandidate("new1", 0.9)
	old := projectCandidate("old1", 0.1)
	old.MaybeReplacedBy = []string{"new1", "gone1"}
	alone := projectCandidate("old2", 0.05)
	alone.MaybeReplacedBy = []string{"gone2"}
	res := run(t, &fakeRetriever{set: passiveSet(newer, old, alone)}, passiveRequest())
	if got := itemByID(t, res.Items, "old1").MaybeReplacedBy; len(got) != 1 || got[0] != "new1" {
		t.Errorf("old1 MaybeReplacedBy = %v, want [new1]", got)
	}
	if got := itemByID(t, res.Items, "old2"); len(got.MaybeReplacedBy) > 0 || strings.Contains(got.Line(), MaybeReplacedMarker) {
		t.Errorf("old2 is marked although no replacement is on the page: %s", got.Line())
	}
}

func TestItemLineRendersMaybeReplacedBy(t *testing.T) {
	it := Item{ID: "A1", Category: "fact", Content: "x", MaybeReplacedBy: []string{"B1"}}
	l := it.Line()
	if !strings.Contains(l, " "+MaybeReplacedMarker) || !strings.Contains(l, " maybe_replaced_by=`B1`)") || strings.Count(l, "\n") != 0 {
		t.Errorf("Line() = %q", l)
	}
	it.MaybeReplacedBy = []string{"B1\nforged"}
	if strings.Count(it.Line(), "\n") != 0 {
		t.Errorf("a replacement id broke the line: %q", it.Line())
	}
	it.MaybeReplacedBy = nil
	if l := it.Line(); strings.Contains(l, MaybeReplacedMarker) || strings.Contains(l, "maybe_replaced_by") {
		t.Errorf("an unmarked row renders the marker: %q", l)
	}
}

// capRows is a project bucket of five rows under passiveRequest's 3-row cap:
// the older row sits inside the cap and its replacement behind it.
func capRows(mod func(rows []memory.Candidate)) []memory.Candidate {
	ids := []string{"r1", "old", "r3", "r4", "new"}
	rows := make([]memory.Candidate, len(ids))
	for i, id := range ids {
		c := projectCandidate(id, 0.9-0.1*float64(i))
		c.FetchedBy = "proj"
		rows[i] = c
	}
	rows[1].MaybeReplacedBy = []string{"new"}
	if mod != nil {
		mod(rows)
	}
	return rows
}

// TestRunPassiveReplacementTakesOnlyAnUnreservedSlot: a replacement pulled from
// behind the cap may displace one row, the lowest-ranked one inside the cap that
// nothing reserved, and no other row crosses the cap. Shifting every row down
// instead would cut the last row inside the cap whatever chose it.
func TestRunPassiveReplacementTakesOnlyAnUnreservedSlot(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(rows []memory.Candidate)
		want   []string
		marked bool
	}{
		{"the lowest unreserved row gives up its slot", nil, []string{"r1", "new", "old"}, true},
		{"a floor pick keeps its slot", func(rows []memory.Candidate) { rows[2].FloorReserved = true },
			[]string{"new", "old", "r3"}, true},
		{"a pinned row keeps its slot", func(rows []memory.Candidate) { rows[2].Pinned = true },
			[]string{"new", "old", "r3"}, true},
		{"no unreserved row: nothing moves and nothing is marked", func(rows []memory.Candidate) {
			rows[0].Pinned = true
			rows[2].FloorReserved = true
		}, []string{"r1", "old", "r3"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, &fakeRetriever{set: passiveSet(capRows(tc.mod)...)}, passiveRequest())
			got := itemIDs(res.Items)
			if !eq(got, tc.want) {
				t.Fatalf("answer %v, want %v", got, tc.want)
			}
			old := itemByID(t, res.Items, "old")
			if marked := len(old.MaybeReplacedBy) > 0; marked != tc.marked {
				t.Errorf("older row marked = %v (%v), want %v", marked, old.MaybeReplacedBy, tc.marked)
			}
		})
	}
}

// TestRunPassiveReplacementInsideTheCapShiftsNoRowOut: with both rows inside
// the cap the replacement moves up and the rows between stay on the page.
func TestRunPassiveReplacementInsideTheCapShiftsNoRowOut(t *testing.T) {
	rows := capRows(nil)
	rows[1].MaybeReplacedBy = []string{"r3"}
	res := run(t, &fakeRetriever{set: passiveSet(rows...)}, passiveRequest())
	if got := itemIDs(res.Items); !eq(got, []string{"r1", "r3", "old"}) {
		t.Errorf("answer %v, want [r1 r3 old]", got)
	}
}

// TestRunPassiveReplacementLeavesADiversityAdmittedRowItsSlot: stage 7 brought
// the gotcha into the window in place of a third fact; the replacement may not
// take that slot back.
func TestRunPassiveReplacementLeavesADiversityAdmittedRowItsSlot(t *testing.T) {
	mk := func(id, cat string, score float64) memory.Candidate {
		c := candidate(id, "proj", cat, "a project memory "+id, score)
		c.FetchedBy = "proj"
		return c
	}
	f1, old, f3, g1, newer := mk("f1", "fact", 0.9), mk("old", "fact", 0.8), mk("f3", "fact", 0.7), mk("g1", "gotcha", 0.6), mk("new", "fact", 0.5)
	old.MaybeReplacedBy = []string{"new"}
	res := run(t, &fakeRetriever{set: passiveSet(f1, old, f3, g1, newer)}, passiveRequest())
	if got := itemIDs(res.Items); !eq(got, []string{"new", "old", "g1"}) {
		t.Errorf("answer %v, want [new old g1]: the row stage 7 admitted keeps its slot", got)
	}
}
