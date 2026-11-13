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

// TestRunCapsAMixedBucketByItsOwnPolicy is the assertion that justifies the
// retriever reporting which policy admitted a row.
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

// memoryCandidate is the subset of a candidate these tests set, kept as a small
// struct so each case reads as the two facts it is about.
type memoryCandidate struct {
	ID        string
	Score     float64
	FetchedBy string
}
