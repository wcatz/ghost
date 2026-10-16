package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// The version-upgrade pair: a pin, then the upgrade that replaced it. They read
// alike, so the linker writes a cosine `related` edge between them, and one at
// strength 1.0 clears the 0.90 near-duplicate bar.
const (
	demotePgxV4 = "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere."
	demotePgxV5 = "go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5."
	// demotePgxV4Again restates the pin: no value changed, so it is a duplicate,
	// not a replacement.
	demotePgxV4Again = "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the code imports pgx/v4 everywhere and the pool is built with pgxpool.Connect."
)

// addStamped saves a fact row in project and stamps its created_at to the given
// day of September 2026, because the pair's chronology is the fact under test
// and Create stamps the wall clock.
func addStamped(t *testing.T, s *Store, project, content string, importance float32, day int, scope map[string]string) string {
	t.Helper()
	id, err := s.Create(context.Background(), project, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: importance, Scope: scope,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", content, err)
	}
	stamp := time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(StoredStampLayout)
	if _, err := s.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
		t.Fatalf("stamp %s: %v", id, err)
	}
	return id
}

func linkAt(t *testing.T, s *Store, from, to, relation string) {
	t.Helper()
	if err := s.CreateLink(context.Background(), from, to, relation, 1.0, "manual"); err != nil {
		t.Fatalf("CreateLink(%s -> %s, %s): %v", from, to, relation, err)
	}
}

func memIDs(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func hydrate(t *testing.T, s *Store, ids ...string) []Memory {
	t.Helper()
	got, err := s.GetByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	byID := make(map[string]Memory, len(got))
	for _, m := range got {
		byID[m.ID] = m
	}
	out := make([]Memory, len(ids))
	for i, id := range ids {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("row %s not hydrated", id)
		}
		out[i] = m
	}
	return out
}

// pairWindow is a fuseAndRank window in the order a query in the OLD wording
// ranks it: the replaced pin first, its replacement second, then an unrelated
// row. The near-duplicate demotion runs on exactly this order.
type pairWindow struct {
	s                *Store
	older, newer, at string
}

func newPairWindow(t *testing.T, olderScope, newerScope map[string]string) pairWindow {
	t.Helper()
	s := testStore(t)
	w := pairWindow{s: s}
	w.older = addStamped(t, s, testProject, demotePgxV4, 0.8, 20, olderScope)
	w.newer = addStamped(t, s, testProject, demotePgxV5, 0.7, 24, newerScope)
	w.at = addStamped(t, s, testProject, "The staging cluster drains its queue before every deploy.", 0.6, 22, nil)
	linkAt(t, s, w.older, w.newer, "related")
	return w
}

func (w pairWindow) rows(t *testing.T) []Memory {
	return hydrate(t, w.s, w.older, w.newer, w.at)
}

// TestDemoteNearDuplicatesDoesNotSinkALexicalReplacement: on the fuseAndRank
// paths (ghost_search_all, plain bench) a cosine `related` edge between a claim
// and its replacement made the replacement the near-duplicate loser of an
// old-wording query, and the demotion sank it below an unrelated row. A
// replacement is not a duplicate, so the relevance order stands.
func TestDemoteNearDuplicatesDoesNotSinkALexicalReplacement(t *testing.T) {
	w := newPairWindow(t, nil, nil)
	got := memIDs(w.s.demoteNearDuplicates(context.Background(), w.rows(t), DefaultSearchParams()))
	want := []string{w.older, w.newer, w.at}
	if !eqStrings(got, want) {
		t.Errorf("order %v, want the relevance order %v: the replacement was demoted as a near-duplicate", got, want)
	}
}

// TestDemoteNearDuplicatesStillSinksARestatement: the exemption is for a
// replacement pair only. A restatement linked the same way is still demoted.
func TestDemoteNearDuplicatesStillSinksARestatement(t *testing.T) {
	s := testStore(t)
	a := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	b := addStamped(t, s, testProject, demotePgxV4Again, 0.7, 24, nil)
	x := addStamped(t, s, testProject, "The staging cluster drains its queue before every deploy.", 0.6, 22, nil)
	linkAt(t, s, a, b, "related")
	got := memIDs(s.demoteNearDuplicates(context.Background(), hydrate(t, s, a, b, x), DefaultSearchParams()))
	if want := []string{a, x, b}; !eqStrings(got, want) {
		t.Errorf("order %v, want %v: a restatement must still sink", got, want)
	}
}

// TestDemoteNearDuplicatesAnExplicitEdgeWins: a live `contradicts` edge takes
// the pair out of the lexical rule, so the near-duplicate verdict applies as
// before.
func TestDemoteNearDuplicatesAnExplicitEdgeWins(t *testing.T) {
	w := newPairWindow(t, nil, nil)
	linkAt(t, w.s, w.older, w.newer, "contradicts")
	got := memIDs(w.s.demoteNearDuplicates(context.Background(), w.rows(t), DefaultSearchParams()))
	if want := []string{w.older, w.at, w.newer}; !eqStrings(got, want) {
		t.Errorf("order %v, want %v: an explicit edge must win over the lexical exemption", got, want)
	}
}

// TestDemoteNearDuplicatesADifferentScopePairIsNotExempt: a scoped row and an
// unscoped one do not conflict, so the edge still counts as a near-duplicate,
// and two different scopes are never a replacement, so nothing exempts it.
func TestDemoteNearDuplicatesADifferentScopePairIsNotExempt(t *testing.T) {
	w := newPairWindow(t, map[string]string{"environment": "production"}, nil)
	got := memIDs(w.s.demoteNearDuplicates(context.Background(), w.rows(t), DefaultSearchParams()))
	if want := []string{w.older, w.at, w.newer}; !eqStrings(got, want) {
		t.Errorf("order %v, want %v: a different-scope pair is not exempt", got, want)
	}
}

// TestDemoteNearDuplicatesACrossProjectPairIsNotExempt: ghost_search_all spans
// projects, and a pair needs one project, so the same two texts in two
// projects are not a replacement and the edge still demotes.
func TestDemoteNearDuplicatesACrossProjectPairIsNotExempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	older := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	newer := addStamped(t, s, "other-project", demotePgxV5, 0.7, 24, nil)
	x := addStamped(t, s, testProject, "The staging cluster drains its queue before every deploy.", 0.6, 22, nil)
	linkAt(t, s, older, newer, "related")
	got := memIDs(s.demoteNearDuplicates(ctx, hydrate(t, s, older, newer, x), DefaultSearchParams()))
	if want := []string{older, x, newer}; !eqStrings(got, want) {
		t.Errorf("order %v, want %v: a cross-project pair is not exempt", got, want)
	}
}

// failingExplicitEdges fails the replacement rule's explicit-edge read and
// passes every other statement through, so the near-duplicate read succeeds
// and only the exemption's own read breaks.
type failingExplicitEdges struct{ Queryer }

func (f failingExplicitEdges) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if strings.Contains(q, "relation IN ('supersedes', 'contradicts')") {
		return nil, errors.New("explicit edge read failed")
	}
	return f.Queryer.QueryContext(ctx, q, args...)
}

// TestNearDuplicateExemptionAFailedReadDemotesAsBefore: a demotion only
// reorders, so when the exemption's edge read fails the pair is demoted exactly
// as it was before the exemption existed, rather than left unranked on a guess.
func TestNearDuplicateExemptionAFailedReadDemotesAsBefore(t *testing.T) {
	w := newPairWindow(t, nil, nil)
	rows := w.rows(t)
	ids := memIDs(rows)
	db := failingExplicitEdges{w.s.queryDB()}
	pairs, err := nearDuplicatePenaltyRows(context.Background(), db, ids, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("nearDuplicatePenaltyRows: %v", err)
	}
	got, err := exemptReplacementPairs(context.Background(), db, pairs, rows, time.Now().UTC())
	if err == nil {
		t.Fatalf("the failing read must be reported")
	}
	penalty, _ := demotionVerdicts(got)
	if penalty[w.newer] != 1 {
		t.Errorf("penalty %v: a failed exemption read must leave the demotion as it was", penalty)
	}
}

// TestSearchHybridAllDoesNotSinkALexicalReplacement is the ghost_search_all
// path end to end: an old-wording keyword query over the pair and a third row
// that matches it less well. Before, the edge sank the replacement below that
// third row.
func TestSearchHybridAllDoesNotSinkALexicalReplacement(t *testing.T) {
	s := testStore(t)
	older := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	newer := addStamped(t, s, testProject, demotePgxV5, 0.7, 24, nil)
	weak := addStamped(t, s, testProject, "Deploy checklist: drain the staging queue, rotate the logs, confirm the dashboards are green, and note the pgx driver version in the release notes for the operators on call.", 0.6, 22, nil)

	ctx := context.Background()
	control, err := s.SearchHybridAll(ctx, "pgxpool.Connect pgx v4", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	ctl := memIDs(control)
	if indexOf(ctl, older) < 0 || indexOf(ctl, newer) < 0 || indexOf(ctl, weak) < 0 {
		t.Fatalf("precondition: control %v must hold all three rows", ctl)
	}
	if indexOf(ctl, newer) > indexOf(ctl, weak) {
		t.Fatalf("precondition: control %v must rank the replacement ahead of the weak row", ctl)
	}

	linkAt(t, s, older, newer, "related")
	got, err := s.SearchHybridAll(ctx, "pgxpool.Connect pgx v4", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	if ids := memIDs(got); !eqStrings(ids, ctl) {
		t.Errorf("order %v, want %v: the cosine edge sank the replacement", ids, ctl)
	}
}

// topFixture is a store whose top-memories window holds the pair and two
// unrelated rows, ranked by importance (fact rows do not decay): older, newer,
// then the two others.
func topFixture(t *testing.T, newerContent string) (s *Store, older, newer string) {
	t.Helper()
	s = testStore(t)
	older = addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	newer = addStamped(t, s, testProject, newerContent, 0.7, 24, nil)
	addStamped(t, s, testProject, "The staging cluster drains its queue before every deploy.", 0.6, 22, nil)
	addStamped(t, s, testProject, "Release notes are written before the tag is cut.", 0.5, 21, nil)
	linkAt(t, s, older, newer, "related")
	return s, older, newer
}

// TestGetTopMemoriesKeepsALexicalReplacementInTheCut: GetTopMemories demotes
// over the cap before it cuts, so a demoted replacement left the top-N list
// entirely while the claim it replaced stayed.
func TestGetTopMemoriesKeepsALexicalReplacementInTheCut(t *testing.T) {
	s, older, newer := topFixture(t, demotePgxV5)
	got, err := s.GetTopMemories(context.Background(), testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if ids := memIDs(got); !eqStrings(ids, []string{older, newer}) {
		t.Errorf("top 2 = %v, want [%s %s]: the replacement was demoted out of the cut", ids, older, newer)
	}
}

// TestGetTopMemoriesStillDemotesARestatement is the negative half.
func TestGetTopMemoriesStillDemotesARestatement(t *testing.T) {
	s, older, restated := topFixture(t, demotePgxV4Again)
	got, err := s.GetTopMemories(context.Background(), testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	ids := memIDs(got)
	if indexOf(ids, older) != 0 || indexOf(ids, restated) >= 0 {
		t.Errorf("top 2 = %v: the restatement %s must still be demoted out of the cut", ids, restated)
	}
}

// TestGetTopMemoriesAnExplicitEdgeWins: with a `contradicts` edge the pair is
// no replacement, and the near-duplicate verdict cuts the newer row as before.
func TestGetTopMemoriesAnExplicitEdgeWins(t *testing.T) {
	s, older, newer := topFixture(t, demotePgxV5)
	linkAt(t, s, older, newer, "contradicts")
	got, err := s.GetTopMemories(context.Background(), testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if ids := memIDs(got); indexOf(ids, newer) >= 0 {
		t.Errorf("top 2 = %v: an explicit edge must win over the lexical exemption", ids)
	}
}

// passiveOverCap is the session-start project bucket over its cap, so the
// near-duplicate REORDER branch runs.
func passiveOverCap() SlicePolicy {
	pol := projectPassivePolicy()
	pol.Bucket = testProject
	pol.TwoPass = false
	pol.BehaviorFloor = 0
	pol.DemoteOnlyWhenOverCap = true
	pol.ItemCap = 2
	return pol
}

func passiveOverCapIDs(t *testing.T, s *Store) []string {
	t.Helper()
	req := passiveRequest(testProject, passiveOverCap())
	req.Now = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	set, err := s.Candidates(context.Background(), req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	return passiveIDs(set)
}

// TestPassiveReorderDoesNotPushALexicalReplacementBehindTheCap: on a project
// bucket over its cap the near-duplicate demotion reorders rather than removes,
// and it moved the replacement to the back, behind the cap.
func TestPassiveReorderDoesNotPushALexicalReplacementBehindTheCap(t *testing.T) {
	s, older, newer := topFixture(t, demotePgxV5)
	ids := passiveOverCapIDs(t, s)
	if indexOf(ids, older) != 0 || indexOf(ids, newer) != 1 {
		t.Errorf("passive order %v, want %s then %s ahead of the cap: the replacement was demoted", ids, older, newer)
	}
}

// TestPassiveReorderStillDemotesARestatement is the negative half.
func TestPassiveReorderStillDemotesARestatement(t *testing.T) {
	s, older, restated := topFixture(t, demotePgxV4Again)
	ids := passiveOverCapIDs(t, s)
	if indexOf(ids, older) != 0 || indexOf(ids, restated) != len(ids)-1 {
		t.Errorf("passive order %v: the restatement %s must still sink to the back", ids, restated)
	}
}

// TestPassiveReorderAnExplicitEdgeWins: a `contradicts` edge takes the pair out
// of the lexical rule, so the reorder sinks the newer row as before.
func TestPassiveReorderAnExplicitEdgeWins(t *testing.T) {
	s, older, newer := topFixture(t, demotePgxV5)
	linkAt(t, s, older, newer, "contradicts")
	ids := passiveOverCapIDs(t, s)
	if indexOf(ids, newer) != len(ids)-1 {
		t.Errorf("passive order %v: an explicit edge must win over the lexical exemption", ids)
	}
}
