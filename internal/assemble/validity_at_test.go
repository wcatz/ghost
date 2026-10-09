package assemble

import (
	"fmt"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestAsOfStageTwoKeepsWhatTheListingWithholdsAtT is #910's half of the rule the
// as_of listings use. The rows go through Run itself, so the comparison is
// against the real stage and not against a second reading of its rule: every row
// survives a historical read, including the ones memory.ValidityAt withholds at
// the same instant, and the ones a browsing surface would label.
//
// The two verdicts are deliberately different, and this is the test that says so:
// #898/#899 held the listing's helper to stage 2's verdict on the grounds that a
// row search left out at an instant was a row the listing left out at that
// instant. That stopped being true when the borrowed window was recognised for
// what it is. The bounds an as_of row carries are the LIVE row's — memory_history
// records no validity — so a verdict drawn from them is a claim about today's
// bounds, not about the row at T. Stage 2 therefore draws none, and
// memory.ValidityAt stays the LISTING's rule (and ValidityStateOf the browsing
// surfaces'), each of which is still the one helper for its own question.
func TestAsOfStageTwoKeepsWhatTheListingWithholdsAtT(t *testing.T) {
	t0 := time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	stamp := func(d time.Duration) *string { s := t0.Add(d).Format(f); return &s }
	verified := stamp(-time.Hour)
	zero := "0001-01-01 00:00:00"

	type row struct {
		name             string
		from, until, ver *string
	}
	rows := []row{
		{"open ended", nil, nil, nil},
		{"valid across T", stamp(-time.Hour), stamp(time.Hour), verified},
		{"closed before T", stamp(-48 * time.Hour), stamp(-time.Second), verified},
		{"closed one second before T", nil, stamp(-time.Second), nil},
		{"ends exactly at T", stamp(-time.Hour), stamp(0), verified},
		{"opens one second after T", stamp(time.Second), nil, verified},
		{"starts exactly at T", stamp(0), stamp(time.Hour), verified},
		{"opens after T", stamp(time.Hour), stamp(2 * time.Hour), verified},
		{"expired and future", stamp(time.Hour), stamp(-time.Hour), verified},
		{"unverified window", stamp(-time.Hour), stamp(time.Hour), nil},
		{"unreadable until", nil, strptr("the ides of march"), nil},
		{"unreadable from, closed until", strptr("soon"), stamp(-time.Hour), nil},
		{"zero instant until", nil, &zero, nil},
		{"zero instant from", &zero, stamp(time.Hour), verified},
	}

	cands := make([]memory.Candidate, 0, len(rows))
	for i, r := range rows {
		c := candidate(fmt.Sprintf("m%02d", i), "proj", "fact", "database configuration "+r.name, 1)
		c.ValidFrom, c.ValidUntil, c.VerifiedAt = r.from, r.until, r.ver
		cands = append(cands, c)
	}
	req := historicalRequest(t0)
	req.Budget = Budget{MaxItems: len(rows) + 1}
	// The wall clock is deliberately somewhere else: it must not matter, and on
	// this path it is not even the clock the bounds were written against.
	req.Now = t0.Add(1000 * 24 * time.Hour)
	res := run(t, &fakeRetriever{set: setOf(cands...)}, req)

	kept := map[string]bool{}
	for _, it := range res.Items {
		kept[it.ID] = true
	}
	withheldAtT := 0
	for i, r := range rows {
		id := fmt.Sprintf("m%02d", i)
		state, withheld := memory.ValidityAt(r.from, r.until, r.ver, t0)
		if withheld {
			withheldAtT++
		}
		// The row is kept whatever the listing's rule says about it, and the
		// item carries no verdict for the stage to have contradicted.
		if !kept[id] {
			t.Errorf("%s: an as_of search dropped the row (listing state %q): the window is the live row's, "+
				"so no verdict read from it is a verdict about that instant", r.name, state)
			continue
		}
		for _, it := range res.Items {
			if it.ID == id && it.ValidityState != "" {
				t.Errorf("%s: stage 2 recorded state %q, want the empty state that renders the bounds with no "+
					"verdict: a clock's answer about today's window is not a claim about T", r.name, it.ValidityState)
			}
		}
		// The two rules that still hold a verdict are unchanged, and each is
		// still the one helper for its own surface: the listing withholds on
		// ValidityAt, a browsing surface labels with ValidityStateOf.
		if got := ValidityStateOf(r.from, r.until, r.ver, t0); got != state {
			t.Errorf("%s: ValidityStateOf at T = %q, memory.ValidityAt = %q", r.name, got, state)
		}
	}
	if withheldAtT == 0 {
		t.Fatal("no row in this fixture is out of window at T, so the assertions above would say nothing " +
			"about the divergence this change is about")
	}
	// The count of rows the LISTING would have withheld is the count the note
	// reports there; search reports none, because it withheld none.
	if got := memory.AsOfValidityNote(t0, 0); got == memory.AsOfValidityNote(t0, withheldAtT) {
		t.Error("AsOfValidityNote with and without a withheld count renders the same sentence, so the count " +
			"is not a claim any surface can be held to")
	}
}
