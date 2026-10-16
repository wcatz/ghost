package memory

import (
	"context"
	"testing"
	"time"
)

// The reverted-change arc's three decisions: the job backend moves from river
// to asynq, then back to river. The revert note restates the original's values,
// so the save path folds it into the original as its duplicate, and the
// reverted claim (asynq) is the one it replaces.
const (
	jobsRiver    = "Background jobs run on river: cmd/worker starts with JOB_BACKEND=river and job rows live in the river_job table in the main Postgres."
	jobsAsynq    = "Background jobs moved to asynq: cmd/worker starts with JOB_BACKEND=asynq and the queues live on jobs-redis-01.corp.example."
	jobsReverted = "Background jobs are back on river: the asynq move was reverted after it lost retries, and cmd/worker starts with JOB_BACKEND=river again."
)

// dropNow is the request clock the removal judges validity against: after
// every stamp the tests write.
var dropNow = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func addDecision(t *testing.T, s *Store, content string, importance float32, day int) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "decision", Content: content, Source: "mcp", Importance: importance,
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

func idSet(ms []Memory) map[string]bool {
	out := make(map[string]bool, len(ms))
	for _, m := range ms {
		out[m.ID] = true
	}
	return out
}

// TestDropNearDuplicatesKeepsTheRevertNoteThatReplacesTheRevertedClaim is the
// reverted-change arc at its fourth session. The revert note is linked to the
// original as its `duplicate` (the documented restatement fold), the original
// ranks above it, and the removal took the revert note out by rank alone. That
// removed the one row saying the asynq move was undone, so the answer showed
// asynq as current and the original as replaced by it. The later of two rows
// about one subject is never removed in favour of the earlier one: the earlier
// row is the one that leaves.
func TestDropNearDuplicatesKeepsTheRevertNoteThatReplacesTheRevertedClaim(t *testing.T) {
	s := testStore(t)
	river := addDecision(t, s, jobsRiver, 0.8, 20)
	asynq := addDecision(t, s, jobsAsynq, 0.7, 22)
	reverted := addDecision(t, s, jobsReverted, 0.7, 24)
	linkAt(t, s, reverted, river, "duplicate")

	rows := hydrate(t, s, asynq, river, reverted)

	// Preconditions: the pairs the marker names, so the test fails for the
	// reason it is about rather than for a pair rule that moved.
	cands := make([]Candidate, len(rows))
	for i, m := range rows {
		cands[i] = Candidate{Memory: m}
	}
	repl := lexicalReplacements(cands, nil, dropNow)
	if got := repl[river]; len(got) != 1 || got[0] != asynq {
		t.Fatalf("precondition: river replaced by %v, want [%s]", got, asynq)
	}
	if got := repl[asynq]; len(got) != 1 || got[0] != reverted {
		t.Fatalf("precondition: asynq replaced by %v, want [%s]", got, reverted)
	}
	for _, n := range repl[river] {
		if n == reverted {
			t.Fatalf("precondition: river and the revert note must not be a replacement pair")
		}
	}

	kept, removed, lostTo := s.dropNearDuplicates(context.Background(), rows, DefaultSearchParams(), dropNow)
	k := idSet(kept)
	if !k[reverted] {
		t.Fatalf("kept %v, removed %v: the revert note %s was removed in favour of the original it restates", memIDs(kept), memIDs(removed), reverted)
	}
	if !k[asynq] {
		t.Errorf("kept %v: the reverted claim %s must stay, marked", memIDs(kept), asynq)
	}
	if k[river] {
		t.Errorf("kept %v: the earlier row of the duplicate pair %s must be the one removed", memIDs(kept), river)
	}
	if got := lostTo[river]; len(got) != 1 || got[0] != reverted {
		t.Errorf("lostTo[river] = %v, want [%s]", got, reverted)
	}
	// The relevance order of the survivors is untouched.
	if got, want := memIDs(kept), []string{asynq, reverted}; !eqStrings(got, want) {
		t.Errorf("kept order %v, want the relevance order %v", got, want)
	}
}

// TestDropNearDuplicatesRemovesTheEarlierRowOfARestatement: two restatements,
// no value changed and no third row involved. The earlier one ranks first, and
// it is the earlier one that is removed, so the answer keeps the latest
// statement of the claim.
func TestDropNearDuplicatesRemovesTheEarlierRowOfARestatement(t *testing.T) {
	s := testStore(t)
	a := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	b := addStamped(t, s, testProject, demotePgxV4Again, 0.7, 24, nil)
	x := addStamped(t, s, testProject, "The staging cluster drains its queue before every deploy.", 0.6, 22, nil)
	linkAt(t, s, a, b, "related")

	kept, removed, lostTo := s.dropNearDuplicates(context.Background(), hydrate(t, s, a, b, x), DefaultSearchParams(), dropNow)
	if got, want := memIDs(kept), []string{b, x}; !eqStrings(got, want) {
		t.Errorf("kept %v, want %v: the later restatement was removed in favour of the earlier", got, want)
	}
	if got, want := memIDs(removed), []string{a}; !eqStrings(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	if got := lostTo[a]; len(got) != 1 || got[0] != b {
		t.Errorf("lostTo[%s] = %v, want [%s]", a, got, b)
	}
}

// TestDropNearDuplicatesKeepsALoserThatReplacesAThirdRow: X is a claim, Y
// replaces it, and Z restates Y in a narrower scope (so Z pairs with neither,
// and the marker cannot name it). Z ranks first and Y is the earlier of the two,
// so the chronology does not save Y; but Y is the lexical replacement of X,
// which is in the answer, and removing Y would leave X unmarked as though it
// were current.
func TestDropNearDuplicatesKeepsALoserThatReplacesAThirdRow(t *testing.T) {
	s := testStore(t)
	prod := map[string]string{"env": "prod"}
	x := addStamped(t, s, testProject, demotePgxV4, 0.6, 20, nil)
	y := addStamped(t, s, testProject, demotePgxV5, 0.7, 22, nil)
	z := addStamped(t, s, testProject, demotePgxV5, 0.8, 24, prod)
	linkAt(t, s, z, y, "duplicate")

	rows := hydrate(t, s, z, y, x)
	cands := make([]Candidate, len(rows))
	for i, m := range rows {
		cands[i] = Candidate{Memory: m}
	}
	repl := lexicalReplacements(cands, nil, dropNow)
	if got := repl[x]; len(got) != 1 || got[0] != y {
		t.Fatalf("precondition: %s replaced by %v, want only [%s]", x, got, y)
	}
	if len(repl[y]) != 0 || len(repl[z]) != 0 {
		t.Fatalf("precondition: Y and Z must pair with nothing else, got %v", repl)
	}

	kept, removed, _ := s.dropNearDuplicates(context.Background(), rows, DefaultSearchParams(), dropNow)
	if got, want := memIDs(kept), []string{z, y, x}; !eqStrings(got, want) {
		t.Errorf("kept %v (removed %v), want %v: the replacement of a row in the answer was removed", got, memIDs(removed), want)
	}
}

// TestDropNearDuplicatesStillCollapsesARestatementCluster: three restatements
// of one claim, each linked to the others. The removal still leaves exactly one
// of them, and it is the newest.
func TestDropNearDuplicatesStillCollapsesARestatementCluster(t *testing.T) {
	s := testStore(t)
	a := addStamped(t, s, testProject, demotePgxV4, 0.9, 20, nil)
	b := addStamped(t, s, testProject, demotePgxV4Again, 0.8, 22, nil)
	c := addStamped(t, s, testProject, "The code imports pgx/v4 everywhere; go.mod pins github.com/jackc/pgx/v4 at v4.18.1 and the pool is built with pgxpool.Connect.", 0.7, 24, nil)
	linkAt(t, s, a, b, "related")
	linkAt(t, s, b, c, "related")
	linkAt(t, s, a, c, "related")

	kept, _, _ := s.dropNearDuplicates(context.Background(), hydrate(t, s, a, b, c), DefaultSearchParams(), dropNow)
	if got, want := memIDs(kept), []string{c}; !eqStrings(got, want) {
		t.Errorf("kept %v, want only the newest restatement %v", got, want)
	}
}

// TestDropNearDuplicatesTiedStampsFallBackToRank: a pair with one created_at
// has no chronology, so the relevance rank decides the loser as before.
func TestDropNearDuplicatesTiedStampsFallBackToRank(t *testing.T) {
	s := testStore(t)
	a := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
	b := addStamped(t, s, testProject, demotePgxV4Again, 0.7, 20, nil)
	linkAt(t, s, a, b, "related")

	kept, _, _ := s.dropNearDuplicates(context.Background(), hydrate(t, s, a, b), DefaultSearchParams(), dropNow)
	if got, want := memIDs(kept), []string{a}; !eqStrings(got, want) {
		t.Errorf("kept %v, want the higher-ranked row %v", got, want)
	}
}

// TestDropNearDuplicatesNeverRemovesAProtectedEarlierRow: the earlier row is
// pinned. Protection keeps it, and the chronology keeps the later one, so both
// stay rather than either rule giving way.
func TestDropNearDuplicatesNeverRemovesAProtectedEarlierRow(t *testing.T) {
	for _, order := range []string{"earlier-first", "later-first"} {
		t.Run(order, func(t *testing.T) {
			s := testStore(t)
			a := addStamped(t, s, testProject, demotePgxV4, 0.8, 20, nil)
			b := addStamped(t, s, testProject, demotePgxV4Again, 0.7, 24, nil)
			if _, err := s.db.Exec(`UPDATE memories SET pinned = 1 WHERE id = ?`, a); err != nil {
				t.Fatalf("pin: %v", err)
			}
			linkAt(t, s, a, b, "related")
			ids := []string{a, b}
			if order == "later-first" {
				ids = []string{b, a}
			}
			kept, removed, _ := s.dropNearDuplicates(context.Background(), hydrate(t, s, ids...), DefaultSearchParams(), dropNow)
			if !idSet(kept)[a] {
				t.Errorf("kept %v: the pinned row %s was removed", memIDs(kept), a)
			}
			if !idSet(kept)[b] {
				t.Errorf("kept %v (removed %v): the later row %s was removed in favour of the earlier", memIDs(kept), memIDs(removed), b)
			}
		})
	}
}

// TestDropNearDuplicatesLaterRowRankedFirstIsUnchanged: the common case the
// rank rule already got right — the later restatement ranks first and the
// earlier one is removed.
func TestDropNearDuplicatesLaterRowRankedFirstIsUnchanged(t *testing.T) {
	s := testStore(t)
	a := addStamped(t, s, testProject, demotePgxV4, 0.7, 20, nil)
	b := addStamped(t, s, testProject, demotePgxV4Again, 0.8, 24, nil)
	linkAt(t, s, a, b, "related")

	kept, removed, _ := s.dropNearDuplicates(context.Background(), hydrate(t, s, b, a), DefaultSearchParams(), dropNow)
	if got, want := memIDs(kept), []string{b}; !eqStrings(got, want) {
		t.Errorf("kept %v (removed %v), want %v", got, memIDs(removed), want)
	}
}
