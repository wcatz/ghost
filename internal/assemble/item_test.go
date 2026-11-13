package assemble

import (
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// The renderer is the only place a caller learns what a row claims about itself.
// Stage 2 keeps a row because its window is open, and a row whose window nobody
// has ever checked is a different thing to act on than one checked last month —
// but if the line renders them identically, the column is being stored for
// nobody. These tests pin the field set the item line carries and the one
// surface that has not moved to it yet (ValidityLabel, called by
// internal/mcpserver's formatMemories).

func stamp(s string) *time.Time {
	at, err := time.Parse(memory.StoredStampLayout, s)
	if err != nil {
		panic("test fixture is not a stored stamp: " + s)
	}
	return &at
}

// A whole-day boundary renders as a date, because that is what a caller states and
// what a reader acts on; a stamp with a time of day keeps it, because then the
// time was the point.
func TestItemLineRendersValidityDatesAndTimes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		item  Item
		wants []string
	}{
		{
			name:  "open window",
			item:  Item{ValidityState: validityValid, ValidFrom: stamp("2026-01-15 00:00:00"), ValidUntil: stamp("2026-10-01 00:00:00")},
			wants: []string{"valid from 2026-01-15", "until 2026-10-01"},
		},
		{
			name:  "open window nobody checked",
			item:  Item{ValidityState: validityUnverified, ValidFrom: stamp("2026-01-15 00:00:00"), ValidUntil: stamp("2026-10-01 00:00:00")},
			wants: []string{"valid from 2026-01-15", "until 2026-10-01", "unverified"},
		},
		{
			name:  "checked at a moment of the day",
			item:  Item{ValidityState: validityValid, ValidFrom: stamp("2026-01-15 00:00:00"), VerifiedAt: stamp("2026-09-20 09:30:00")},
			wants: []string{"valid from 2026-01-15", "verified 2026-09-20 09:30:00"},
		},
		{
			// What a writer stores for a bare date as a window's end.
			name:  "an end of day",
			item:  Item{ValidityState: validityValid, ValidFrom: stamp("2026-01-15 00:00:00"), ValidUntil: stamp("2026-10-01 23:59:59")},
			wants: []string{"valid from 2026-01-15", "until 2026-10-01"},
		},
		{
			// The near miss a rule keyed only on the hour would take: 23:59:58 is
			// not a day boundary and must keep its second.
			name:  "a second before the end of the day",
			item:  Item{ValidityState: validityValid, ValidUntil: stamp("2026-10-01 23:59:58")},
			wants: []string{"until 2026-10-01 23:59:58"},
		},
		{
			// The end-of-day collapse belongs to the window's end alone.
			name:  "the end of a day as a start",
			item:  Item{ValidityState: validityFuture, ValidFrom: stamp("2026-10-01 23:59:59")},
			wants: []string{"valid from 2026-10-01 23:59:59", "not yet valid"},
		},
		{
			name:  "the end of a day as a verification",
			item:  Item{ValidityState: validityValid, VerifiedAt: stamp("2026-09-20 23:59:59")},
			wants: []string{"verified 2026-09-20 23:59:59"},
		},
		{
			// A midnight valid_until is reachable without any of Ghost's tools — an
			// artifact, a restored snapshot, a hand edit or a date() call can leave
			// one — and the date form does NOT stand for midnight on this boundary.
			name:  "midnight as the end of a window",
			item:  Item{ValidityState: validityExpired, ValidUntil: stamp("2026-10-01 00:00:00")},
			wants: []string{"until 2026-10-01 00:00:00", "expired"},
		},
		{
			name:  "verification with no window",
			item:  Item{ValidityState: validityValid, VerifiedAt: stamp("2026-09-20 00:00:00")},
			wants: []string{"verified 2026-09-20"},
		},
		{
			name:  "an open end only",
			item:  Item{ValidityState: validityUnverified, ValidFrom: stamp("2026-01-15 00:00:00")},
			wants: []string{"valid from 2026-01-15", "unverified"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.item.Line()
			for _, want := range tc.wants {
				if !strings.Contains(line, want) {
					t.Errorf("line is missing %q: %s", want, line)
				}
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A browsing surface has not run stage 2, so a closed window is about to be
// printed in full. Printing it unmarked is the worst available reading of a dated
// row: the date is right there and nothing says what it means.
func TestValidityStateOfMatchesWhatTheStagesWouldDecide(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		from, to, verified *string
		want               string
	}{
		{"closed window", ptr("2024-01-01 00:00:00"), ptr("2020-06-01 00:00:00"), nil, validityExpired},
		{"window not yet open", ptr("2099-01-01 00:00:00"), ptr("2199-12-31 00:00:00"), nil, validityFuture},
		{"open window nobody checked", ptr("2024-01-01 00:00:00"), ptr("2199-12-31 00:00:00"), nil, validityUnverified},
		{"checked current claim", ptr("2024-01-01 00:00:00"), nil, ptr("2026-01-01 00:00:00"), validityValid},
		{"no claim at all", nil, nil, nil, validityUnset},
		{"expiry is today, not yesterday", nil, ptr("2026-09-27 00:00:00"), nil, validityExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidityStateOf(tc.from, tc.to, tc.verified, now)
			if got != tc.want {
				t.Fatalf("ValidityStateOf = %q, want %q", got, tc.want)
			}
			// The rule ValidityStateOf's own comment promises to share: the state the
			// pipeline reaches for this row must be the state a browsing surface
			// renders.
			if staged := readValidity(memory.Candidate{Memory: memory.Memory{
				ValidFrom: tc.from, ValidUntil: tc.to, VerifiedAt: tc.verified,
			}}, now).state; staged != got {
				t.Errorf("the pipeline reads this row as %q and the renderer as %q", staged, got)
			}
			label := ValidityLabel(got, tc.from, tc.to, tc.verified)
			switch tc.want {
			case validityExpired:
				if !strings.Contains(label, "expired") {
					t.Errorf("a closed window renders unmarked: %q", label)
				}
			case validityFuture:
				if !strings.Contains(label, "not yet valid") {
					t.Errorf("a window that has not opened renders unmarked: %q", label)
				}
			case validityUnverified:
				if !strings.Contains(label, "unverified") {
					t.Errorf("an unchecked window renders unmarked: %q", label)
				}
			case validityUnset:
				if label != "" {
					t.Errorf("a row that claims nothing renders %q, want nothing", label)
				}
			}
		})
	}
}
