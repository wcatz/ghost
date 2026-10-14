package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestATrimmedQueryResponseNamesHowManyRowsItCut is #1289's regression. The
// response-fit post-pass drops whole rows from the bottom of a query answer, and
// before this the only trace of the drop was the machine line's admitted= and
// candidates= fields — the answer itself said nothing, so an agent reading it was
// never told a matching row existed behind the cut. The answer now ends its body
// with the same cut notice the row-rendering listing tools render, naming the
// count, the budget and the lever that reaches the rest.
func TestATrimmedQueryResponseNamesHowManyRowsItCut(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 2
	// A cap the short row, the framing and the notice fit inside and the long
	// row does not: exactly one row is cut, which is the count the notice must
	// report. The number is small on purpose — a notice that is not counted
	// against the cap would push this answer back over it.
	req.Budget.MaxBytes = 700
	rows := []memory.Candidate{
		candidate("KEEP", "proj", "fact", "short note", 0.9),
		candidate("CUT", "proj", "fact", strings.Repeat("a long note ", 400), 0.8),
	}

	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)
	if len(res.Items) != 1 || res.Items[0].ID != "KEEP" {
		t.Fatalf("the fixture did not trim to one row: items = %v — the case under test never happened",
			itemIDs(res.Items))
	}

	want := CutNotice(1, req.Budget.MaxBytes, searchFitAdvice)
	if !strings.Contains(res.Response, want) {
		t.Errorf("a trimmed answer does not carry the cut notice %q:\n%s", want, res.Response)
	}
	if !strings.Contains(res.Response, "narrow the query to see the rest") {
		t.Errorf("the cut notice must say how to see the rest:\n%s", res.Response)
	}
	if len(res.Response) > req.Budget.MaxBytes {
		t.Errorf("the answer is %d bytes against a %d-byte cap: the notice was not counted against it",
			len(res.Response), req.Budget.MaxBytes)
	}
}

// withCutNotice raises an untrimmed render's byte size by the length of the cut
// notice a `cut`-row trim adds, so a test that derives its cap from that render
// still fits the NOTICED answer: the notice is part of the measured response, and
// a cap that ignores it forces the pass to cut a second row to pay for it. It is
// measured rather than pinned because the notice names the budget it was fitted
// to, so its length can depend on the cap's digit count; the loop settles that,
// and converges in one pass for every cap these tests use.
func withCutNotice(base, cut int) int {
	// The notice is the blank-line-separated sentence responseCutNotice writes, so
	// the answer grows by that framing plus the sentence, not by the sentence
	// alone.
	cap := base
	for range 4 {
		next := base + len("\n\n"+CutNotice(cut, cap, searchFitAdvice))
		if next == cap {
			return cap
		}
		cap = next
	}
	return cap
}

// TestAnEmptiedQueryAnswerCarriesNoCutNotice is the other half of the contract:
// when the cap cuts every row there is nothing left for a notice to qualify, and
// the abstention sentence already names the budget and the action. A notice over
// no rows would be a sentence about rows the reader cannot see.
func TestAnEmptiedQueryAnswerCarriesNoCutNotice(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 2
	// The one row is large enough that its render cannot fit, while the empty
	// envelope the pass falls back to can: the answer is EMPTIED by the cap
	// rather than returned as the impossible-budget error.
	req.Budget.MaxBytes = 600
	rows := []memory.Candidate{
		candidate("A1", "proj", "fact", strings.Repeat("a", 1000), 0.9),
	}

	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)
	if len(res.Items) != 0 {
		t.Fatalf("the fixture did not empty: items = %v — the case under test never happened", itemIDs(res.Items))
	}
	if res.Outcome != OutcomeEmpty || res.Reason != reasonAllOverBudget {
		t.Fatalf("outcome/reason = %q/%q, want empty/all_over_budget", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Response, "cut by the response budget") {
		t.Errorf("the emptied answer does not name the cap that emptied it:\n%s", res.Response)
	}
	if strings.Contains(res.Response, "cut from the end of this answer") {
		t.Errorf("an emptied answer carries the rows-cut notice, which describes rows it does not hold:\n%s", res.Response)
	}
}
