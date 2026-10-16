package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// addPair saves the version-upgrade pair the way one working day leaves it: the
// pin, then its upgrade half a minute later. Close stamps keep decay from
// deciding the order, so the old-wording query's keyword rank is what puts the
// replaced pin first, as it does in the storyline.
func addPair(r *replacementStore, older, newer memory.Memory) (string, string) {
	r.t.Helper()
	o := r.add(older, 24)
	n := r.add(newer, 24)
	stamp := time.Date(2026, 9, 24, 10, 0, 30, 0, time.UTC).Format(memory.StoredStampLayout)
	if _, err := r.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, n); err != nil {
		r.t.Fatalf("stamp %s: %v", n, err)
	}
	return o, n
}

// pgxQueryRequest is the search a reader runs in the OLD wording after the
// upgrade: an old import line, an old build log. The keyword leg ranks the
// replaced pin first, because the query is phrased in its words.
func pgxQueryRequest() Request {
	req := baseRequest()
	req.Query = "pgxpool.Connect pgx v4"
	req.Budget = Budget{MaxItems: 5}
	return req
}

// TestRunQueryOrdersAValueReplacementAheadOfTheRowItReplaced is the defect a
// search in the old wording shows before any supersede pass: both pins come
// back, the replaced one first, and the reader meets the stale claim before the
// current one. After the relevance verdict is decided, the newer row of a
// lexical replacement pair is shown directly ahead of the older one.
func TestRunQueryOrdersAValueReplacementAheadOfTheRowItReplaced(t *testing.T) {
	r := newReplacementStore(t)
	older, newer := addPair(r, dep(pgxV4, 0.8), dep(pgxV5, 0.7))

	res := run(t, r.s, pgxQueryRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (older %s, newer %s)", itemIDs(res.Items), older, newer)
	}
	if ni != oi-1 {
		t.Errorf("answer %v: the replacement %s must sit directly ahead of %s", itemIDs(res.Items), newer, older)
	}
	if old := itemByID(t, res.Items, older); len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
	if strings.Index(res.Response, pgxV5) > strings.Index(res.Response, pgxV4) {
		t.Errorf("the rendered answer shows the replaced pin first:\n%s", res.Response)
	}
}

// TestRunQueryKeepsAReplacementACosineEdgeCallsANearDuplicate: the linking
// worker writes a cosine `related` edge between exactly such rows, and one at or
// above the demotion threshold is a near-duplicate edge. Ranked below the older
// row by an old-wording query, the newer row was the pair's loser and the
// retriever removed it, so the reader got the stale claim alone. A lexical
// replacement is not a duplicate: both rows stay, newer first.
func TestRunQueryKeepsAReplacementACosineEdgeCallsANearDuplicate(t *testing.T) {
	r := newReplacementStore(t)
	older, newer := addPair(r, dep(pgxV4, 0.8), dep(pgxV5, 0.7))
	r.link(older, newer, "related")

	req := pgxQueryRequest()
	req.Explain = true
	res := run(t, r.s, req)
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if ni < 0 {
		var why string
		for _, row := range res.Explain.Rows {
			if row.ID == Token(newer) {
				why = row.Reason
			}
		}
		t.Fatalf("answer %v dropped the replacement %s: %s", itemIDs(res.Items), newer, why)
	}
	if oi < 0 {
		t.Fatalf("answer %v dropped the older row %s: a lexical guess never withholds a row", itemIDs(res.Items), older)
	}
	if ni != oi-1 {
		t.Errorf("answer %v: the replacement %s must sit directly ahead of %s", itemIDs(res.Items), newer, older)
	}
	for _, d := range res.Trace.Decisions {
		if d.Reason == reasonNearDuplicate {
			t.Errorf("a near-duplicate drop was recorded for the pair: %+v", d)
		}
	}
}

// TestRunQueryExplainNamesTheRowAReplacementMovedAheadOf: the explain row of
// the moved replacement says which row it was moved ahead of, and its rank is
// its place in the answer. The older row names nothing.
func TestRunQueryExplainNamesTheRowAReplacementMovedAheadOf(t *testing.T) {
	r := newReplacementStore(t)
	older, newer := addPair(r, dep(pgxV4, 0.8), dep(pgxV5, 0.7))
	req := pgxQueryRequest()
	req.Explain = true
	res := run(t, r.s, req)
	var gotNew, gotOld *memory.ExplainRow
	for i := range res.Explain.Rows {
		switch res.Explain.Rows[i].ID {
		case Token(newer):
			gotNew = &res.Explain.Rows[i]
		case Token(older):
			gotOld = &res.Explain.Rows[i]
		}
	}
	if gotNew == nil || gotOld == nil {
		t.Fatalf("explain is missing a row of the pair: %+v", res.Explain.Rows)
	}
	// The relevance order put the older row first: it is the keyword leg's top
	// hit for a query in its own words.
	if gotOld.FTSRank < 0 || gotNew.FTSRank >= 0 && gotNew.FTSRank < gotOld.FTSRank {
		t.Fatalf("precondition: fts ranks old=%d new=%d, want the older row ranked first", gotOld.FTSRank, gotNew.FTSRank)
	}
	if gotNew.MovedAheadOf != Token(older) {
		t.Errorf("newer row moved_ahead_of = %q, want %q", gotNew.MovedAheadOf, Token(older))
	}
	if gotOld.MovedAheadOf != "" {
		t.Errorf("older row moved_ahead_of = %q, want empty", gotOld.MovedAheadOf)
	}
	if gotNew.Rank != indexOf(res.Items, newer)+1 || gotOld.Rank != indexOf(res.Items, older)+1 {
		t.Errorf("explain ranks new=%d old=%d, want the answer's own places %d and %d",
			gotNew.Rank, gotOld.Rank, indexOf(res.Items, newer)+1, indexOf(res.Items, older)+1)
	}
}

// queryRows is a query page in relevance order, scored from the top down, with
// the row named old naming the row named new as its replacement.
func queryRows(ids ...string) []memory.Candidate {
	rows := make([]memory.Candidate, len(ids))
	for i, id := range ids {
		rows[i] = candidate(id, "proj", "fact", "database configuration "+id, 0.9-0.1*float64(i))
		rows[i].FTSRank = i
	}
	for i := range rows {
		if rows[i].ID == "old" {
			rows[i].MaybeReplacedBy = []string{"new"}
		}
	}
	return rows
}

func queryRequest(maxItems int) Request {
	req := baseRequest()
	req.Budget = Budget{MaxItems: maxItems}
	return req
}

// TestRunQueryMovesOnlyThePairedRow: the move takes the replacement to directly
// ahead of the row it replaced. Every other row keeps its relative order, a row
// above the pair keeps its place, and a page with no pair is not touched.
func TestRunQueryMovesOnlyThePairedRow(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{"adjacent pair", []string{"a", "old", "new", "b"}, []string{"a", "new", "old", "b"}},
		{"rows between shift down by one", []string{"a", "old", "b", "c", "new", "d"}, []string{"a", "new", "old", "b", "c", "d"}},
		{"already ahead", []string{"a", "new", "b", "old"}, []string{"a", "new", "b", "old"}},
		{"no pair on the page", []string{"a", "b", "c", "d"}, []string{"a", "b", "c", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, &fakeRetriever{set: ftsOnlySet(queryRows(tc.ids...)...)}, queryRequest(10))
			if got := itemIDs(res.Items); !eq(got, tc.want) {
				t.Errorf("answer %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunQueryMoveNeverCrossesThePageBoundary: a replacement the item cap cut is
// not pulled back onto the page, the row it would have replaced keeps its place,
// and the older row names nothing, because its replacement is not on the page.
func TestRunQueryMoveNeverCrossesThePageBoundary(t *testing.T) {
	res := run(t, &fakeRetriever{set: ftsOnlySet(queryRows("a", "old", "b", "new")...)}, queryRequest(3))
	if got := itemIDs(res.Items); !eq(got, []string{"a", "old", "b"}) {
		t.Fatalf("answer %v, want [a old b]: the move must not reach past the cap", got)
	}
	if old := itemByID(t, res.Items, "old"); len(old.MaybeReplacedBy) > 0 {
		t.Errorf("old names %v although its replacement is off the page", old.MaybeReplacedBy)
	}
}

// TestRunQueryByteCapCutsTheRelevanceLowestRow: the response-fit pass trims on
// relevance order, before the move, so the row it cuts is the relevance-lowest
// one — here the replacement — and not whichever row the move would have left
// last on the page.
func TestRunQueryByteCapCutsTheRelevanceLowestRow(t *testing.T) {
	// Rows long enough that cutting one frees more bytes than the cut notice
	// that says so costs.
	long := func() []memory.Candidate {
		rows := queryRows("a", "old", "new")
		for i := range rows {
			rows[i].Content += " " + strings.Repeat("pooling and retries ", 20)
		}
		return rows
	}
	full := run(t, &fakeRetriever{set: ftsOnlySet(long()...)}, queryRequest(10))
	if got := itemIDs(full.Items); !eq(got, []string{"a", "new", "old"}) {
		t.Fatalf("precondition: uncut answer %v, want [a new old]", got)
	}
	req := queryRequest(10)
	req.Budget.MaxBytes = full.Bytes - 1
	res := run(t, &fakeRetriever{set: ftsOnlySet(long()...)}, req)
	if got := itemIDs(res.Items); !eq(got, []string{"a", "old"}) {
		t.Fatalf("answer %v, want [a old]: the byte cap cuts the relevance-lowest row", got)
	}
	if old := itemByID(t, res.Items, "old"); len(old.MaybeReplacedBy) > 0 {
		t.Errorf("old names %v although its replacement was cut", old.MaybeReplacedBy)
	}
}

// TestRunQueryMoveLeavesTheVerdictAsRelevanceDecidedIt: the outcome, its reason,
// the abstention sentence, the machine line and the notes are decided on the
// relevance order, so the same page with and without the pair carries the same
// verdict, an answerable one and a weak one alike, and the same response bytes.
func TestRunQueryMoveLeavesTheVerdictAsRelevanceDecidedIt(t *testing.T) {
	build := func(paired, weak bool) *memory.CandidateSet {
		rows := queryRows("a", "old", "b", "new")
		if !paired {
			rows[1].MaybeReplacedBy = nil
		}
		if weak {
			for i := range rows {
				rows[i].FTSRank = 6 + i
			}
			return weakSet(rows...)
		}
		return hybridSet(rows...)
	}
	for _, weak := range []bool{false, true} {
		req := hybridRequest()
		req.Budget = Budget{MaxItems: 10}
		with := run(t, &fakeRetriever{set: build(true, weak)}, req)
		without := run(t, &fakeRetriever{set: build(false, weak)}, req)
		if weak && with.Outcome != OutcomeWeak {
			t.Fatalf("precondition: outcome = %q, want weak", with.Outcome)
		}
		if !weak && with.Outcome != OutcomeAnswerable {
			t.Fatalf("precondition: outcome = %q, want answerable", with.Outcome)
		}
		if got := itemIDs(with.Items); !eq(got, []string{"a", "new", "old", "b"}) {
			t.Fatalf("weak=%v: answer %v, want [a new old b]", weak, got)
		}
		if with.Outcome != without.Outcome || with.Reason != without.Reason ||
			with.Abstention != without.Abstention || with.Machine != without.Machine ||
			strings.Join(with.Notes, "|") != strings.Join(without.Notes, "|") {
			t.Errorf("weak=%v: the move changed the verdict:\nwith    %q %q %q %q\nwithout %q %q %q %q", weak,
				with.Outcome, with.Reason, with.Abstention, with.Machine,
				without.Outcome, without.Reason, without.Abstention, without.Machine)
		}
	}
}

// TestRunQueryDoesNotMoveAPinnedPair: the pinned rule is the passive one. A
// pinned older row keeps its place and is marked, and a pinned replacement is
// not pulled ahead of its older row.
func TestRunQueryDoesNotMoveAPinnedPair(t *testing.T) {
	for _, pinned := range []string{"old", "new"} {
		t.Run(pinned, func(t *testing.T) {
			rows := queryRows("a", "old", "b", "new")
			for i := range rows {
				if rows[i].ID == pinned {
					rows[i].Pinned = true
				}
			}
			res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, queryRequest(10))
			if got := itemIDs(res.Items); !eq(got, []string{"a", "old", "b", "new"}) {
				t.Errorf("answer %v, want the relevance order [a old b new]", got)
			}
			if old := itemByID(t, res.Items, "old"); len(old.MaybeReplacedBy) != 1 {
				t.Errorf("old MaybeReplacedBy = %v, want [new]: a pinned pair is still marked", old.MaybeReplacedBy)
			}
		})
	}
}

// TestRunQueryDoesNotMoveRowsWhoseScopesDiffer: one value in staging and
// another in production are two settings, not a replacement, so a search that
// returns both keeps them in relevance order and marks neither.
func TestRunQueryDoesNotMoveRowsWhoseScopesDiffer(t *testing.T) {
	r := newReplacementStore(t)
	v4 := dep(pgxV4, 0.8)
	v4.Scope = map[string]string{"environment": "production"}
	v5 := dep(pgxV5, 0.7)
	v5.Scope = map[string]string{"environment": "staging"}
	older, newer := addPair(r, v4, v5)

	plain := newReplacementStore(t)
	po, pn := addPair(plain, dep(pgxV4, 0.8), dep(pgxV5, 0.7))
	// The control: the same pair unscoped is moved, so the scoped pair's order
	// below is the scope rule's doing and not a ranking that already put the
	// replacement first.
	ctl := run(t, plain.s, pgxQueryRequest())
	if indexOf(ctl.Items, pn) > indexOf(ctl.Items, po) {
		t.Fatalf("precondition: the unscoped pair %v was not reordered", itemIDs(ctl.Items))
	}

	res := run(t, r.s, pgxQueryRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows", itemIDs(res.Items))
	}
	if ni < oi {
		t.Errorf("answer %v: a different-scope pair was moved", itemIDs(res.Items))
	}
	assertUnmarked(t, res.Items)
}

// TestRunQueryKeepsTheOlderRowOfAPairACosineEdgeLinks is the other half of the
// veto: when the replacement outranks the row it replaced, the older row was
// the near-duplicate loser and left the answer. The lexical guess never
// withholds a row, so it stays, behind its replacement and marked.
func TestRunQueryKeepsTheOlderRowOfAPairACosineEdgeLinks(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(pgxV4, 0.8), 20)
	newer := r.add(dep(pgxV5, 0.7), 24)
	r.link(older, newer, "related")
	res := run(t, r.s, pgxQueryRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (older %s, newer %s)", itemIDs(res.Items), older, newer)
	}
	if ni != oi-1 {
		t.Errorf("answer %v: the replacement %s must sit directly ahead of %s", itemIDs(res.Items), newer, older)
	}
	if old := itemByID(t, res.Items, older); len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("older row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
}

// TestRunQueryStillDropsANearDuplicateThatIsNotAReplacement: the veto is for a
// replacement pair only. A cosine near-duplicate of a restatement (no value
// changed) is still removed.
func TestRunQueryStillDropsANearDuplicateThatIsNotAReplacement(t *testing.T) {
	r := newReplacementStore(t)
	a := r.add(dep("go.mod pins github.com/jackc/pgx/v4 at v4.18.1 and the pool is built with pgxpool.Connect.", 0.8), 20)
	b := r.add(dep("The pool is built with pgxpool.Connect; go.mod pins github.com/jackc/pgx/v4 at v4.18.1 everywhere.", 0.7), 24)
	r.link(a, b, "related")
	res := run(t, r.s, pgxQueryRequest())
	if indexOf(res.Items, a) >= 0 && indexOf(res.Items, b) >= 0 {
		t.Errorf("answer %v holds both rows of a restatement pair: the near-duplicate removal stopped", itemIDs(res.Items))
	}
}

// TestRunPassiveGlobalBucketKeepsBothRowsOfAReplacementPair: the `_global`
// bucket DROPS its near-duplicate losers, so the same cosine edge removed one
// claim of a replacement pair from the session-start block before the pair was
// named. Both stay, the newer first.
func TestRunPassiveGlobalBucketKeepsBothRowsOfAReplacementPair(t *testing.T) {
	r := newReplacementStore(t)
	ctx := context.Background()
	if err := r.s.EnsureProject(ctx, memory.GlobalProjectID, "", memory.GlobalProjectID); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	add := func(content string, importance float32, day int) string {
		id, err := r.s.Create(ctx, memory.GlobalProjectID, memory.Memory{Category: "dependency", Content: content, Importance: importance, Source: "mcp"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		stamp := time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(memory.StoredStampLayout)
		if _, err := r.db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		return id
	}
	older := add(pgxV4, 0.8, 20)
	newer := add(pgxV5, 0.7, 24)
	r.link(older, newer, "related")
	res := run(t, r.s, passiveRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("block %v must hold both rows (older %s, newer %s)", itemIDs(res.Items), older, newer)
	}
	if ni > oi {
		t.Errorf("block %v: the replacement %s must come ahead of %s", itemIDs(res.Items), newer, older)
	}
}
