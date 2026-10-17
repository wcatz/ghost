package assemble

import (
	"fmt"
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

// TestAQueryAnswerKeepsItsOnlyRowWholeOverTheBudget is #1483's regression. The
// one row is larger than the cap on its own, and the pass used to drop it and
// answer empty/all_over_budget: a memory at the store's content cap whose line
// breaks fold to `⏎` renders past the search budget, so a legal memory could not
// be returned by search at all. The first row is never cut, which is the rule
// the listing tools already apply: it is shown whole, over the cap, with a
// notice saying why, and the notice names no advice because no argument makes
// one row smaller. This replaces the test that pinned the emptied answer.
func TestAQueryAnswerKeepsItsOnlyRowWholeOverTheBudget(t *testing.T) {
	content := "zebra" + strings.Repeat("\nx", 300)
	for _, limit := range []int{1, 2} {
		req := baseRequest()
		req.Budget.MaxItems = limit
		req.Budget.MaxBytes = 600
		rows := []memory.Candidate{candidate("A1", "proj", "fact", content, 0.9)}

		res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)
		if len(res.Items) != 1 || res.Items[0].ID != "A1" {
			t.Fatalf("limit %d: items = %v, want the only row kept", limit, itemIDs(res.Items))
		}
		if res.Outcome == OutcomeEmpty || res.Reason == reasonAllOverBudget {
			t.Errorf("limit %d: outcome/reason = %q/%q, want the row's own verdict, not an abstention",
				limit, res.Outcome, res.Reason)
		}
		if !strings.Contains(res.Response, Data(content)) {
			t.Errorf("limit %d: the row is not in the answer whole:\n%s", limit, res.Response)
		}
		want := "(The first row alone is larger than the 600-byte response budget, so it is shown in full " +
			"rather than dropped; no search argument makes one row smaller.)"
		if !strings.Contains(res.Response, want) {
			t.Errorf("limit %d: the answer does not carry the first-row notice %q:\n%s", limit, want, res.Response)
		}
		if strings.Contains(res.Response, searchFitAdvice) {
			t.Errorf("limit %d: the notice advises fewer results, which cannot shrink one row:\n%s",
				limit, res.Response)
		}
		if res.Bytes <= req.Budget.MaxBytes {
			t.Fatalf("limit %d: the answer is %d bytes, inside the %d-byte cap: the fixture never put "+
				"the row over it", limit, res.Bytes, req.Budget.MaxBytes)
		}
		if strings.Contains(res.Response, "[ghost:outcome=empty") {
			t.Errorf("limit %d: the machine line reports an empty answer:\n%s", limit, res.Response)
		}
	}
}

// TestAQueryAnswerCutsLowerRowsBeforeKeepingTheFirstOverBudget is the control for
// the rule above: a lower-ranked row is still cut while more than one row is
// left, and only the LAST row is kept whole. Two rows each larger than the cap
// come back as the first one, with the notice counting the one that went.
func TestAQueryAnswerCutsLowerRowsBeforeKeepingTheFirstOverBudget(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 2
	req.Budget.MaxBytes = 600
	rows := []memory.Candidate{
		candidate("KEEP", "proj", "fact", strings.Repeat("first ", 200), 0.9),
		candidate("CUT", "proj", "fact", strings.Repeat("second ", 200), 0.8),
	}

	res := run(t, &fakeRetriever{set: ftsOnlySet(rows...)}, req)
	if !eq(itemIDs(res.Items), []string{"KEEP"}) {
		t.Fatalf("items = %v, want the lower row cut and the first kept", itemIDs(res.Items))
	}
	want := "(1 row was cut from the end of this answer, and the first row alone is larger than the 600-byte " +
		"response budget, so it is shown in full rather than dropped; no search argument makes one row smaller.)"
	if !strings.Contains(res.Response, want) {
		t.Errorf("the answer does not carry %q:\n%s", want, res.Response)
	}
	if !eq(stageTrace(t, res, "response_fit").DroppedIDs, []string{"CUT"}) {
		t.Errorf("the trace does not record the lower row as cut: %+v", stageTrace(t, res, "response_fit"))
	}
}

// TestAQueryAnswerWhoseFramingIsTheExcessDoesNotClaimTheRowIsTooLarge: the row
// fits the cap on its own and the framing around it — the verdict line, the
// qualifiers — is what pushes the answer over. The row is still kept whole,
// because the first row is never cut, but the notice may not say the row alone
// is larger than the budget: that is false, and a caller reading it would
// believe the stored memory is what cannot fit. Both the one-row answer and the
// one a lower row was cut from are checked.
func TestAQueryAnswerWhoseFramingIsTheExcessDoesNotClaimTheRowIsTooLarge(t *testing.T) {
	keep := candidate("KEEP", "proj", "fact", strings.Repeat("a", 500), 0.9)
	for _, tc := range []struct {
		name string
		rows []memory.Candidate
		lead string
	}{
		{"one row", []memory.Candidate{keep}, "(The first row fits the "},
		{"a lower row cut", []memory.Candidate{keep, candidate("CUT", "proj", "fact", strings.Repeat("b", 500), 0.8)},
			"(1 row was cut from the end of this answer, and the first row fits the "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			req.Budget.MaxItems = 2
			alone := run(t, &fakeRetriever{set: ftsOnlySet(keep)}, req)
			req.Budget.MaxBytes = alone.Bytes - 1

			res := run(t, &fakeRetriever{set: ftsOnlySet(tc.rows...)}, req)
			if !eq(itemIDs(res.Items), []string{"KEEP"}) {
				t.Fatalf("items = %v, want the first row kept whole", itemIDs(res.Items))
			}
			if line := len(res.Items[0].Line()); line > req.Budget.MaxBytes {
				t.Fatalf("precondition: the row's line is %d bytes, over the %d-byte cap, so the framing "+
					"is not what this fixture puts over it", line, req.Budget.MaxBytes)
			}
			want := fmt.Sprintf("%s%d-byte response budget, but not with the rest of this answer around it, "+
				"so it is shown in full over the budget rather than dropped.)", tc.lead, req.Budget.MaxBytes)
			if !strings.Contains(res.Response, want) {
				t.Errorf("the answer does not carry %q:\n%s", want, res.Response)
			}
			if strings.Contains(res.Response, "alone is larger") {
				t.Errorf("the notice claims a row that fits the cap is larger than it:\n%s", res.Response)
			}
		})
	}
}
