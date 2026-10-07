package assemble

import (
	"fmt"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestValidityAtAgreesWithStageTwoAtTheRequestedInstant holds the one helper the
// as_of listings use (memory.ValidityAt) to the verdict stage 2 reaches when
// ghost_memory_search binds the assembler's clock to the same instant (#899).
// The rows go through Run itself, so the comparison is against the real stage and
// not against a second reading of its rule: same inputs, same verdict, at the
// instants either side of a bound, at the bound, and over unreadable bounds.
func TestValidityAtAgreesWithStageTwoAtTheRequestedInstant(t *testing.T) {
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
	// The wall clock is deliberately somewhere else: it must not matter.
	req.Now = t0.Add(1000 * 24 * time.Hour)
	res := run(t, &fakeRetriever{set: setOf(cands...)}, req)

	kept := map[string]bool{}
	for _, it := range res.Items {
		kept[it.ID] = true
	}
	for i, r := range rows {
		id := fmt.Sprintf("m%02d", i)
		state, withheld := memory.ValidityAt(r.from, r.until, r.ver, t0)
		if kept[id] == withheld {
			t.Errorf("%s: search kept=%v but memory.ValidityAt withheld=%v (state %q): the listing and search disagree",
				r.name, kept[id], withheld, state)
		}
		if kept[id] {
			for _, it := range res.Items {
				if it.ID == id && it.ValidityState != state {
					t.Errorf("%s: stage 2 recorded state %q, memory.ValidityAt says %q", r.name, it.ValidityState, state)
				}
			}
		}
		if got := ValidityStateOf(r.from, r.until, r.ver, t0); got != state {
			t.Errorf("%s: ValidityStateOf at T = %q, memory.ValidityAt = %q", r.name, got, state)
		}
	}
}
