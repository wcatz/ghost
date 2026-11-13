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
// nothing reserved, and no other row crosses the cap.
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
