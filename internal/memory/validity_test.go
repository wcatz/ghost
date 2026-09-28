package memory

import (
	"testing"
	"time"
)

// TestValidityStateNamesAContract is the guard for the rule this file owns.
//
// internal/assemble's validity stage and explain mode both read it, so a change
// here changes what two surfaces say about the same row. The cases below are the
// ones where the ORDER of the tests and the treatment of an unreadable value
// matter — they are not exhaustive illustrations, they are the boundaries a
// careless edit breaks silently.
func TestValidityStateNamesAContract(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Time) *string {
		s := d.Format("2006-01-02 15:04:05")
		return &s
	}
	date := func(d time.Time) *string {
		s := d.Format("2006-01-02")
		return &s
	}
	raw := func(s string) *string { return &s }

	for _, tc := range []struct {
		name                   string
		from, until, verified  *string
		want                   string
		wantUnreadableContains string
	}{
		{
			name: "a window that contains now, with a verification, is valid",
			from: stamp(now.Add(-24 * time.Hour)), until: stamp(now.Add(24 * time.Hour)),
			verified: stamp(now.Add(-time.Hour)), want: ValidityValid,
		},
		{
			name: "a window that has closed is expired",
			from: stamp(now.Add(-48 * time.Hour)), until: stamp(now.Add(-time.Hour)),
			verified: stamp(now.Add(-time.Hour)), want: ValidityExpired,
		},
		{
			// The ordering is load-bearing: a closed window is the stronger
			// statement, so a row that is both expired and future is expired. The
			// other order would report a claim as pending when it has already
			// lapsed.
			name: "expired beats future when a row claims both",
			from: stamp(now.Add(24 * time.Hour)), until: stamp(now.Add(-time.Hour)),
			verified: stamp(now.Add(-time.Hour)), want: ValidityExpired,
		},
		{
			name: "a window that has not opened is future",
			from: stamp(now.Add(24 * time.Hour)), verified: stamp(now.Add(-time.Hour)),
			want: ValidityFuture,
		},
		{
			// verified_at is a flag, not a predicate, in the assembler: a live row
			// nobody has re-verified is still true as far as the store knows.
			name: "a window with no verification is unverified, and is not expired",
			from: stamp(now.Add(-time.Hour)), until: stamp(now.Add(time.Hour)),
			want: ValidityUnverified,
		},
		{
			// The state of every memory written before the columns existed. It is
			// NOT "valid": nothing vouched for the row and nothing denied it.
			name: "no claim at all is unset, never valid",
			want: ValidityUnset,
		},
		{
			// A date-only value is as readable as a timestamp — the columns are
			// unconstrained text — and rejecting the shorter layout would turn a
			// real claim into no claim.
			name: "a date-only window is read, not discarded",
			from: date(now.Add(-24 * time.Hour)), until: date(now.Add(24 * time.Hour)),
			verified: date(now.Add(-time.Hour)), want: ValidityValid,
		},
		{
			// An unreadable value is neither valid nor invalid: it is REPORTED and
			// treated as unset, because reading a value nobody can interpret as
			// "valid" would assert something the store never said.
			name: "an unreadable boundary is unset and is reported",
			from: raw("not a timestamp"), until: stamp(now.Add(-time.Hour)),
			want:                   ValidityExpired,
			wantUnreadableContains: "not a timestamp",
		},
		{
			name: "an unreadable boundary with no readable claim is unset, and is reported",
			from: raw("yesterday"), want: ValidityUnset, wantUnreadableContains: "yesterday",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, unreadable := ValidityState(tc.from, tc.until, tc.verified, now)
			if state != tc.want {
				t.Errorf("ValidityState = %q, want %q", state, tc.want)
			}
			if tc.wantUnreadableContains == "" {
				if len(unreadable) != 0 {
					t.Errorf("unreadable = %v, want none", unreadable)
				}
				return
			}
			found := false
			for _, u := range unreadable {
				if u == tc.wantUnreadableContains {
					found = true
				}
			}
			if !found {
				t.Errorf("unreadable = %v, want it to carry %q: a value the store cannot read must be "+
					"reported, because the state says nothing about what was written",
					unreadable, tc.wantUnreadableContains)
			}
		})
	}
}

// ParseStamp itself is NOT tested here. It belongs to #677's validity writers and
// is exercised by their own tests; this file's subject is the STATE rule, and the
// one thing worth asserting about the parser from here is that the state rule
// reads a value through it rather than through a parser of its own — which is
// what the "date-only" and "unreadable" cases above already prove, since a
// second parser with a different layout set would fail them.
