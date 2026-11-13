package assemble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestATrimmedQueryResponseNamesHowManyRowsItCut is #1289's regression.
func TestATrimmedQueryResponseNamesHowManyRowsItCut(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 2
	// A cap the short row, the framing and the notice fit inside and the long row does
	// not: exactly one row is cut, which is the count the notice must report.
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

// TestAQueryAnswerKeepsItsOnlyRowWholeOverTheBudget is #1483's regression.
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

// TestAQueryAnswerWhoseFramingIsTheExcessDoesNotClaimTheRowIsTooLarge: the row
// fits the cap on its own and the framing around it — the verdict line, the
// qualifiers — is what pushes the answer over.
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
