package memory

import (
	"slices"
	"testing"
	"time"
)

// TestValidityStateNamesAContract covers the boundaries a careless edit breaks
// silently: the order of the tests and the treatment of an unreadable value.
// internal/assemble's validity stage and explain mode both read this rule.
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
	day, hour := 24*time.Hour, time.Hour

	for _, tc := range []struct {
		name                  string
		from, until, verified *string
		want                  string
		wantUnreadable        string
	}{
		{name: "valid window", from: stamp(now.Add(-day)), until: stamp(now.Add(day)),
			verified: stamp(now.Add(-hour)), want: ValidityValid},
		{name: "closed window is expired", from: stamp(now.Add(-2 * day)), until: stamp(now.Add(-hour)),
			verified: stamp(now.Add(-hour)), want: ValidityExpired},
		{name: "expired beats future", from: stamp(now.Add(day)), until: stamp(now.Add(-hour)),
			verified: stamp(now.Add(-hour)), want: ValidityExpired},
		{name: "unopened window is future", from: stamp(now.Add(day)), verified: stamp(now.Add(-hour)),
			want: ValidityFuture},
		{name: "no verification is unverified", from: stamp(now.Add(-hour)), until: stamp(now.Add(hour)),
			want: ValidityUnverified},
		{name: "no claim is unset, never valid", want: ValidityUnset},
		{name: "date-only window is read", from: date(now.Add(-day)), until: date(now.Add(day)),
			verified: date(now.Add(-hour)), want: ValidityValid},
		{name: "unreadable boundary is reported", from: raw("not a timestamp"), until: stamp(now.Add(-hour)),
			want: ValidityExpired, wantUnreadable: "not a timestamp"},
		{name: "unreadable boundary alone is unset", from: raw("yesterday"),
			want: ValidityUnset, wantUnreadable: "yesterday"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, unreadable := ValidityState(tc.from, tc.until, tc.verified, now)
			if state != tc.want {
				t.Errorf("ValidityState = %q, want %q", state, tc.want)
			}
			if tc.wantUnreadable == "" && len(unreadable) != 0 {
				t.Errorf("unreadable = %v, want none", unreadable)
			}
			if tc.wantUnreadable != "" && !slices.Contains(unreadable, tc.wantUnreadable) {
				t.Errorf("unreadable = %v, want it to carry %q", unreadable, tc.wantUnreadable)
			}
		})
	}
}
