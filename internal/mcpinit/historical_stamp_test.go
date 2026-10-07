package mcpinit

import (
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestHistoricalSessionMemoriesUnreadableStampIsNoBound covers the as_of read
// path's half of the stamp contract: the validity columns are unconstrained
// text, so a row may legally hold a whole-day stamp (a portable artifact, a
// date() call, a hand edit) or a value nothing in this build can read at all.
//
// Both used to be read with time.Parse(memory.StoredStampLayout, …) and the
// error discarded. That turned every unreadable value into a NON-NIL pointer to
// the zero time, and the renderer then printed a date Ghost never recorded —
// `valid from 0001-01-01 unverified` — for a row ghost_memories_list renders
// with no bound at all, because that surface reads through
// assemble.parseStampPtr → memory.ParseStamp. It also meant a legitimate
// whole-day stamp fabricated the same zero bound: "2026-02-01" does not parse
// against the stored layout, so the row's real claim was replaced by a false
// one.
//
// memory.ParseStamp is the repo's one reader for these columns: it tries every
// layout StampLayouts names, reports readability, and leaves the pointer nil
// when it cannot read the value — which is what assemble/validity.go does and
// what memory.ValidityState's own rule expects.
func TestHistoricalSessionMemoriesUnreadableStampIsNoBound(t *testing.T) {
	unreadable := "not-a-stamp" // legal in the column, readable by nothing
	wholeDay := "2026-02-01"    // the other layout the readers accept

	rows := []memory.AsOfRow{
		{Memory: memory.Memory{
			ID:         "hmem01",
			ProjectID:  "p1",
			Category:   "gotcha",
			Content:    "a row whose validity columns hold values this build cannot read",
			Source:     "manual",
			CreatedAt:  unreadable,
			ValidFrom:  &unreadable,
			ValidUntil: &unreadable,
			VerifiedAt: &unreadable,
			ResolvedAt: &unreadable,
		}},
		{Memory: memory.Memory{
			ID:        "hmem02",
			ProjectID: "p1",
			Category:  "fact",
			Content:   "a row recorded as a whole-day stamp",
			Source:    "manual",
			CreatedAt: wholeDay,
			ValidFrom: &wholeDay,
		}},
	}

	got := historicalSessionMemories(rows, "p1", nil, sessionMemoriesCap, time.Now())
	if len(got) != 2 {
		t.Fatalf("historicalSessionMemories returned %d rows, want 2", len(got))
	}

	// Unreadable means no bound, not a pointer to the zero time.
	unread := got[0]
	for name, stamp := range map[string]*time.Time{
		"valid_from":  unread.ValidFrom,
		"valid_until": unread.ValidUntil,
		"verified_at": unread.VerifiedAt,
		"resolved_at": unread.ResolvedAt,
	} {
		if stamp != nil {
			t.Errorf("%s = %v, want nil: an unreadable value is no bound, not %s",
				name, stamp, time.Time{}.Format(time.RFC3339))
		}
	}
	if !unread.CreatedAt.IsZero() {
		t.Errorf("created_at = %v, want the zero time for an unreadable value", unread.CreatedAt)
	}

	// And nothing fabricated may reach the line the agent reads.
	asOf := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	line := sessionMemoryToItem(unread, &asOf).Line()
	if strings.Contains(line, "0001-01-01") {
		t.Errorf("the block invented a validity date Ghost never recorded:\n%s", line)
	}
	if strings.Contains(line, "valid from") || strings.Contains(line, "until") ||
		strings.Contains(line, "verified") {
		t.Errorf("a row with no readable stamp must carry no validity claim:\n%s", line)
	}

	// The readable half: the whole-day layout is a real claim, so it must be
	// parsed rather than fabricated away.
	whole := got[1]
	if whole.ValidFrom == nil {
		t.Fatalf("valid_from of a whole-day stamp is nil; ParseStamp is not reading the other layout")
	}
	if y, m, d := whole.ValidFrom.Date(); y != 2026 || m != time.February || d != 1 {
		t.Errorf("valid_from = %v, want 2026-02-01", whole.ValidFrom)
	}
	if line := sessionMemoryToItem(whole, &asOf).Line(); !strings.Contains(line, "valid from 2026-02-01") {
		t.Errorf("the whole-day stamp did not reach the line as its recorded claim:\n%s", line)
	}
}
