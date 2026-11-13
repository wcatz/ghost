package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

var _ = memory.CondFTSOnly

// historicalRequest is baseRequest with as_of bound, the shape the MCP surfaces
// build. Now is left at the base request's own instant on purpose in one test and
// moved in another: the binding is supposed to make the two indistinguishable.
func historicalRequest(asOf time.Time) Request {
	req := baseRequest()
	req.AsOf = &asOf
	return req
}

func at(sec int) time.Time {
	return time.Date(2027, 2, 1, 12, 0, sec, 0, time.UTC)
}

// TestAsOfWithNoExtractableTermAnswersNothing: a historical keyword search whose
// query yields no term used to match every in-scope memory, because the empty
// term list read as "no keyword filter". The same request over the present
// returns nothing, and a caller cannot tell the two apart from the rows alone:
// both are lists of memories, and only the outcome says one was a search and the
// other was the whole project.
func TestAsOfWithNoExtractableTermAnswersNothing(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, content := range []string{
		"the deploy key rotation runbook lives in the ops wiki",
		"the retention window is seven days for session rows",
	} {
		if _, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		}); err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
	}

	now := time.Now().UTC()
	asOf := now.Add(time.Minute)
	for _, query := range []string{"!!!", "€"} {
		req := baseRequest()
		req.Query = query
		req.Now = now

		current := run(t, s, req)
		if current.Outcome != OutcomeEmpty || len(current.Items) != 0 {
			t.Errorf("current query %q = outcome %q with %d items, want empty with none: this is the answer the historical read has to match", query, current.Outcome, len(current.Items))
		}

		historical := req
		historical.AsOf = &asOf
		res := run(t, s, historical)
		if res.Outcome != OutcomeEmpty || len(res.Items) != 0 {
			t.Errorf("as_of query %q = outcome %q with %d items (%v), want empty with none: no term was extracted, so nothing matched", query, res.Outcome, len(res.Items), itemIDs(res.Items))
		}
	}
}

// TestAsOfReportsMemoriesWithNoRecordedVersion: the retriever counts the
// in-scope rows it could not place, and the block says so. A shorter set with
// nothing said about the gap is the failure this exists to prevent — a reader
// takes the set for the whole truth at that instant, and acts on a memory's
// absence that is only an absence of a record.
func TestAsOfReportsMemoriesWithNoRecordedVersion(t *testing.T) {
	set := setOf(candidate("m1", "proj", "fact", "database configuration m1", 1))
	set.Unrecorded = 1
	res := run(t, &fakeRetriever{set: set}, historicalRequest(at(0)))

	joined := strings.Join(res.Qualifiers, "\n")
	if !strings.Contains(joined, "unknown before its first recorded version") {
		t.Errorf("the qualifiers %q do not name the gap, want the store's own sentence about it", joined)
	}
	// A current read has no gap to report, so a surface cannot print a disclosure
	// that stopped being true.
	current := run(t, &fakeRetriever{set: setOf(candidate("m1", "proj", "fact", "database configuration m1", 1))}, baseRequest())
	if len(current.Qualifiers) != 0 {
		t.Errorf("a current read carries qualifiers %v, want none", current.Qualifiers)
	}
}

// TestAsOfQualifierNamesTheEvidenceCountsAsUnread: schema v18 put the evidence
// counts on Candidate, and a zero renders as "no recorded evidence" — a claim.
// The store deliberately does not fill them for a historical read (an observation
// is not versioned, so a count would describe the present), which means the trace
// says "no recorded evidence" for a memory that may well have observations. The
// disclosure is what keeps that a stated absence rather than a false one.
func TestAsOfQualifierNamesTheEvidenceCountsAsUnread(t *testing.T) {
	res := run(t, &fakeRetriever{set: setOf(candidate("m1", "proj", "fact", "database configuration m1", 1))}, historicalRequest(at(0)))
	joined := strings.Join(res.Qualifiers, " ")
	if !strings.Contains(joined, "evidence") {
		t.Errorf("the qualifier %q does not mention the evidence counts, so the trace's \"no recorded evidence\" for a historical row stands as a claim", joined)
	}
	if !strings.Contains(joined, "not read") {
		t.Errorf("the qualifier %q says nothing about the evidence counts being unread, want it to say the read did not happen", joined)
	}
}

// TestAsOfRefusesAZeroInstant: the zero time is not an instant, and a request
// that named it would be a historical read of the year one — silently, because
// every stored timestamp is after it and the read would simply find nothing.
func TestAsOfRefusesAZeroInstant(t *testing.T) {
	req := baseRequest()
	zero := time.Time{}
	req.AsOf = &zero
	if _, err := Run(context.Background(), &fakeRetriever{set: setOf()}, req); err == nil {
		t.Fatal("a zero as_of instant was accepted, want it refused")
	} else if !strings.Contains(err.Error(), "AsOf") {
		t.Errorf("the refusal is %q, want it to name the field that is wrong", err)
	}
}

// TestAsOfLeavesACurrentRequestUntouched: the binding is conditional. A request
// with no as_of must reach the store with no as_of, or every search in the tree
// would become a historical read of some arbitrary past.
func TestAsOfLeavesACurrentRequestUntouched(t *testing.T) {
	f := &fakeRetriever{set: setOf(candidate("m1", "proj", "fact", "database configuration m1", 1))}
	res := run(t, f, baseRequest())
	if f.req.AsOf != nil {
		t.Errorf("the retriever was asked with AsOf = %v, want nil: a request that named no instant is a current read", f.req.AsOf)
	}
	if res.Trace.AsOf != "" {
		t.Errorf("Trace.AsOf = %q, want empty for a current read", res.Trace.AsOf)
	}
	if len(res.Qualifiers) != 0 {
		t.Errorf("a current read carries qualifiers %v, want none", res.Qualifiers)
	}
}
