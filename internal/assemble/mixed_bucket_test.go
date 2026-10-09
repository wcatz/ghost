package assemble

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mixingPassiveRequest is the project-context shape: one bucket, and it admits
// `_global` into its own read. It is the shape a caller cannot express as two
// buckets, because two buckets capped at N each admit 2N rows where the caller
// asked for N.
func mixingPassiveRequest() Request {
	return Request{
		ProjectID: "proj",
		Query:     "",
		Source:    SourceProjectCtx,
		Condition: CondFTSOnly,
		Now:       time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Budget: Budget{
			Slices: []Slice{{
				Bucket: "proj", MaxItems: 3, OverFetch: 6,
				Order: "decay", DemoteOnlyWhenOverCap: true, IncludeGlobal: true,
			}},
		},
	}
}

// TestRunServesAMixedPassiveBucket: the shape a whole-project listing needs. One
// slice, one cap, and `_global` rows admitted under the project's own policy —
// which is what `project_id = ? OR project_id = '_global'` has always read.
func TestRunServesAMixedPassiveBucket(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		candidate("p1", "proj", "fact", "a project memory", 0.9),
		candidate("g1", "_global", "preference", "a global memory", 0.8),
		candidate("p2", "proj", "fact", "another project memory", 0.7),
	)}
	res := run(t, f, mixingPassiveRequest())
	if len(res.Items) == 0 {
		t.Fatal("a mixed passive bucket admitted no rows")
	}
}

// TestRunCapsAMixedBucketByItsOwnPolicy is the assertion that justifies the
// retriever reporting which policy admitted a row.
//
// A `_global` row inside a project bucket does not name a slice by its own
// project, so a stage 9 keyed on the ROW's bucket finds no cap for it and leaves
// it unbounded — and "at most 3 rows" quietly becomes "at most 3 project rows,
// plus however many globals were nearby". Here the cap is 1 and the window
// carries a project row and two globals, so the wrong lookup admits all three.
func TestRunCapsAMixedBucketByItsOwnPolicy(t *testing.T) {
	rows := []memoryCandidate{
		{ID: "p1", Score: 0.9, FetchedBy: "proj"},
		{ID: "g1", Score: 0.8, FetchedBy: "proj"},
		{ID: "g2", Score: 0.7, FetchedBy: "proj"},
	}
	set := passiveSet()
	for _, r := range rows {
		c := candidate(r.ID, "proj", "fact", "a memory "+r.ID, r.Score)
		c.FetchedBy = r.FetchedBy
		set.Rows = append(set.Rows, c)
	}
	req := mixingPassiveRequest()
	req.Budget.Slices[0].MaxItems = 1
	res := run(t, &fakeRetriever{set: set}, req)
	if n := len(res.Items); n != 1 {
		t.Errorf("a mixed bucket capped at 1 admitted %d rows (%v): the cap is the POLICY's, and a global row "+
			"admitted under a project bucket does not name a slice by its own project", n, itemIDs(res.Items))
	}
}

// TestRunRefusesAPassiveBucketThatBothMixesAndFetchesGlobal is the refusal the
// two-layer shape needs.
//
// Two slices with DISTINCT bucket names can still describe one overlapping row
// set: one admits `_global` into the project read, the other fetches `_global` in
// its own right. `sliceBuckets` cannot see it — the names differ — and the result
// is every global row admitted twice, capped under two different slices, so
// neither slice's cap describes the block.
//
// It is a refusal rather than a de-duplication because a caller that wants both
// (the resource's Global section is exactly that caller) runs them as two
// REQUESTS, and silently serving the first would answer with rows the caller
// never chose to admit twice.
func TestRunRefusesAPassiveBucketThatBothMixesAndFetchesGlobal(t *testing.T) {
	req := mixingPassiveRequest()
	req.Budget.Slices = append(req.Budget.Slices, Slice{
		Bucket: "_global", MaxItems: 2, OverFetch: 4, Order: "pinned_importance_updated",
	})
	_, err := Run(context.Background(), &fakeRetriever{set: passiveSet()}, req)
	if err == nil {
		t.Fatal("a request that mixes _global into one bucket and fetches it in another was served; every global " +
			"row would be admitted twice under two different caps")
	}
	if !strings.Contains(err.Error(), "_global") {
		t.Errorf("the refusal does not name the bucket at fault: %v", err)
	}
}

// TestRunRejectsTheOverlapRefusalInBothOrders: the order of the two slices must
// not decide whether the shape is legal. A caller that lists the globals bucket
// first gets the same answer as one that lists the mixing bucket first, because
// the check has to see the whole budget rather than the slices it has reached.
func TestRunRejectsTheOverlapRefusalInBothOrders(t *testing.T) {
	for _, order := range [][]string{{"proj", "_global"}, {"_global", "proj"}} {
		req := mixingPassiveRequest()
		mixing := req.Budget.Slices[0]
		global := Slice{Bucket: "_global", MaxItems: 2, OverFetch: 4}
		if order[0] == "_global" {
			req.Budget.Slices = []Slice{global, mixing}
		} else {
			req.Budget.Slices = []Slice{mixing, global}
		}
		if _, err := Run(context.Background(), &fakeRetriever{set: passiveSet()}, req); err == nil {
			t.Errorf("slice order %v was served; the overlap must be refused whichever slice is named first", order)
		}
	}
}

// TestRunKeepsTheSharedExclusionWordingOnAPassiveBlock: the reason a passive
// block that stage 2 emptied is `all_invalid` and not `no_memories` is already
// pinned above. This is the half that is easy to break while fixing the sentence:
// the SEARCH half of it ("the candidates this search found", "the query was not
// wrong") is a claim about a query, and a passive surface never received one.
func TestRunKeepsTheSharedExclusionWordingOnAPassiveBlock(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")
	res := run(t, &fakeRetriever{set: passiveSet(expired)}, mixingPassiveRequest())
	if res.Outcome != OutcomeEmpty || res.Reason != reasonAllInvalid {
		t.Fatalf("verdict: got %s/%s, want empty/%s", res.Outcome, res.Reason, reasonAllInvalid)
	}
	// The shared phrase, because the design says an exclusion-caused empty says
	// "no sufficiently trustworthy memory found" rather than "no matching
	// memories" on EVERY surface.
	if !strings.Contains(strings.ToLower(res.Abstention), "no sufficiently trustworthy memory found") {
		t.Errorf("a passive exclusion must keep the shared exclusion wording: %q", res.Abstention)
	}
	for _, claim := range []string{"this search found", "the query was not wrong"} {
		if strings.Contains(res.Abstention, claim) {
			t.Errorf("the sentence claims %q about a block that was assembled from a window nobody queried: %q",
				claim, res.Abstention)
		}
	}
}

// memoryCandidate is the subset of a candidate these tests set, kept as a small
// struct so each case reads as the two facts it is about.
type memoryCandidate struct {
	ID        string
	Score     float64
	FetchedBy string
}
