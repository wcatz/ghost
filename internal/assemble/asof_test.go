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

// TestAsOfBindsNowToTheInstant: the binding is the whole contract. A row's age,
// its validity window and the trace that describes the block must all be decided
// against T, and the only way to guarantee that is to move the request's clock
// before anything reads it. The test asserts it where a reader can see it —
// through the retriever request, which is the same Now every stage uses.
func TestAsOfBindsNowToTheInstant(t *testing.T) {
	f := &fakeRetriever{set: setOf(candidate("m1", "proj", "fact", "database configuration m1", 1))}
	req := historicalRequest(at(0))
	req.Now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) // the wall clock's answer

	res := run(t, f, req)
	if !f.req.Now.Equal(at(0)) {
		t.Errorf("the retriever was asked with Now = %v, want the as_of instant %v", f.req.Now, at(0))
	}
	if f.req.AsOf == nil || !f.req.AsOf.Equal(at(0)) {
		t.Errorf("the retriever was asked with AsOf = %v, want %v: the store has to know this is a historical read", f.req.AsOf, at(0))
	}
	// The age the trace reports is measured to T, not to the wall clock: this row
	// is a month old at T and negative-age if the binding had not happened.
	sig := res.Trace.Signals["m1"]
	if sig.AgeDays < 0 {
		t.Errorf("AgeDays = %v, want the age measured to the as_of instant, never a negative one", sig.AgeDays)
	}
	if res.Trace.AsOf == "" {
		t.Error("the trace carries no AsOf, want the instant a reader of the trace needs to tell a past block from a present one")
	}
	if !strings.HasPrefix(res.Trace.AsOf, "2027-02-01T12:00:00") {
		t.Errorf("Trace.AsOf = %q, want the RFC 3339 rendering of the requested instant", res.Trace.AsOf)
	}
}

// TestAsOfQualifiesEveryAnswer: a block that is silently a reading of the past is
// read as a reading of the present. The qualifier is therefore a field the
// surface renders on both an empty and a non-empty answer — which is why it is
// not a note, since a note list is bounded from the end and shown for the empty
// case only.
func TestAsOfQualifiesEveryAnswer(t *testing.T) {
	row := candidate("m1", "proj", "fact", "database configuration m1", 1)

	answered := run(t, &fakeRetriever{set: setOf(row)}, historicalRequest(at(0)))
	if len(answered.Items) == 0 {
		t.Fatal("the block is empty, so the assertion below would prove nothing about a non-empty answer")
	}
	if len(answered.Qualifiers) == 0 {
		t.Error("a historical block with rows carries no qualifier, want one: the rows do not say they are a past reading")
	}
	if joined := strings.Join(answered.Qualifiers, " "); !strings.Contains(joined, "keyword-only") {
		t.Errorf("the qualifier %q does not say the retrieval was keyword-only, want it to: an agent would otherwise assume a vector leg ran", joined)
	}
	if joined := strings.Join(answered.Qualifiers, " "); !strings.Contains(joined, "2027-02-01T12:00:00Z") {
		t.Errorf("the qualifier %q does not name the instant, want it to", joined)
	}

	// An empty historical answer is the one that most needs it: "no matching
	// memories" is a claim about the present unless the reader knows it is not.
	empty := run(t, &fakeRetriever{set: setOf()}, historicalRequest(at(0)))
	if len(empty.Qualifiers) == 0 {
		t.Error("an empty historical block carries no qualifier, want one: an absence from a past set reads as an absence from the store")
	}
	// The notes are the diagnostics list and must not repeat them: a surface
	// renders both, and the same sentence twice is noise that trains a reader to
	// skip it.
	for _, n := range empty.Notes {
		if strings.Contains(n, "historical read") {
			t.Errorf("the notes repeat the qualifier (%q), want it only in Result.Qualifiers", n)
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

// TestAsOfRefusesVectorOnly: an embedding records the text a memory holds now,
// so a vector leg over a past content set has nothing to compare. Refusing is the
// store's job (it is the one that knows), and this test pins that the assembler
// passes the request through rather than quietly downgrading the condition —
// because a caller that asked for the vector leg cannot tell from the rows which
// leg produced them.
func TestAsOfRefusesVectorOnly(t *testing.T) {
	req := historicalRequest(at(0))
	req.Condition = CondVectorOnly
	req.QueryVec = []float32{0.1, 0.2, 0.3}
	f := &fakeRetriever{set: setOf()}

	// The fake cannot refuse the way the store does, so the assertion is on the
	// request the assembler built: the condition reaches the store intact, which
	// is what lets the store refuse it in the words that name the leg.
	res, err := Run(context.Background(), f, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.req.Condition != CondVectorOnly {
		t.Errorf("the retriever was asked with condition %q, want %q passed through: the store refuses it, not the assembler", f.req.Condition, CondVectorOnly)
	}
	if res.Qualifiers == nil && len(res.Qualifiers) != 0 {
		t.Errorf("Qualifiers = %v, want an empty slice rather than nil for a request with rows", res.Qualifiers)
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
