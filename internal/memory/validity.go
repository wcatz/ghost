package memory

import (
	"time"
)

const (
	// ValidityUnset means the row states no validity window or verification.
	ValidityUnset = "unset"
	// ValidityValid means the window contains now and verification is recorded.
	ValidityValid = "valid"
	// ValidityFuture means the window has not opened yet.
	ValidityFuture = "future"
	// ValidityExpired means the window has closed.
	ValidityExpired = "expired"
	// ValidityUnverified means the window is readable but not verified.
	ValidityUnverified = "unverified"
)

// ValidityState reports a memory's validity state at now and any unreadable stamps.
func ValidityState(validFrom, validUntil, verifiedAt *string, now time.Time) (string, []string) {
	var unreadable []string
	read := func(s *string) (*time.Time, bool) {
		t, ok := parseStoredStamp(s)
		if !ok && s != nil {
			unreadable = append(unreadable, *s)
		}
		return t, ok
	}
	from, _ := read(validFrom)
	until, _ := read(validUntil)
	_, verifiedReadable := read(verifiedAt)

	switch {
	case until != nil && until.Before(now):
		return ValidityExpired, unreadable
	case from != nil && from.After(now):
		return ValidityFuture, unreadable
	case from == nil && until == nil && !verifiedReadable:
		return ValidityUnset, unreadable
	case !verifiedReadable:
		return ValidityUnverified, unreadable
	default:
		return ValidityValid, unreadable
	}
}

// parseStoredStamp parses a stored validity stamp.
func parseStoredStamp(s *string) (*time.Time, bool) {
	if s == nil {
		return nil, false
	}
	t, ok := ParseStamp(*s)
	if !ok {
		return nil, false
	}
	return &t, true
}

// ValidityWithheld reports whether state means a retrieval path must withhold the row.
func ValidityWithheld(state string) bool {
	return state == ValidityExpired || state == ValidityFuture
}

// ValidityAt reports the validity state and whether the row is withheld at t.
func ValidityAt(validFrom, validUntil, verifiedAt *string, t time.Time) (state string, withheld bool) {
	state, _ = ValidityState(validFrom, validUntil, verifiedAt, t)
	return state, ValidityWithheld(state)
}
