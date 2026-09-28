package memory

import (
	"time"
)

// This is the ONE rule that names a memory's validity state. It lives here, in
// the package that owns the columns, because two consumers now read it:
//
//   - internal/assemble's validity stage, which DROPS a row outside its window
//     (internal/assemble/validity.go delegates here rather than keeping a copy),
//     and
//   - explain mode, which reports the state of a row the search returned
//     (internal/memory/explain.go).
//
// A second copy would be a second answer to "may this row be used", free to
// drift from the first the moment either is edited: a search that explains a row
// as "valid" while the assembler drops it as "expired" is exactly the #571 class
// of bug, one layer up. The states are exported because the assembler's stage
// records them into its trace and renders them into an answer, and those
// spellings are part of that surface's contract.
const (
	// ValidityUnset is the state of a row that states nothing about its own
	// currency: no window and no verification. It is the state of every memory
	// written before the columns existed, and it is NOT "valid" — nothing vouched
	// for the row, and nothing denied it either.
	ValidityUnset = "unset"
	// ValidityValid is a readable window that contains now, and a verification
	// someone recorded.
	ValidityValid = "valid"
	// ValidityFuture is a window that has not opened yet.
	ValidityFuture = "future"
	// ValidityExpired is a window that has closed. It is the stronger statement
	// of the two non-valid states, so a row that is both expired and future is
	// reported expired.
	ValidityExpired = "expired"
	// ValidityUnverified is a readable window with no recorded verification: the
	// row claims a period and nothing vouches for it. verified_at is a flag
	// rather than a predicate in the assembler, so this state is reported and
	// the row is kept.
	ValidityUnverified = "unverified"
)

// The stamp PARSER is the store's own — ParseStamp over StampLayouts, from
// #677's validity writers — rather than a second one here. A writer judging a
// value it did not store and a reader interpreting one have to reach the same
// verdict on what that value means, which is the same reason this file owns the
// STATE rule and internal/assemble delegates to it rather than keeping a copy.

// ValidityState names a memory's validity against a clock, and reports any
// stored value no layout could read.
//
// The order of the tests is load-bearing: a row whose window has closed is
// expired even if it also carries an open start, because a closed window is the
// stronger statement. A missing or unreadable boundary is unset, so the row
// keeps its place unless a readable boundary excludes it — a row that states no
// validity has made no claim about its own currency, which is the common case
// and must not be read as expired.
//
// The returned strings are the exported constants above, so a caller that
// compares them compares against the same values this function returns.
func ValidityState(validFrom, validUntil, verifiedAt *string, now time.Time) (string, []string) {
	var unreadable []string
	read := func(s *string) (*time.Time, bool) {
		if s == nil {
			return nil, false
		}
		t, ok := ParseStamp(*s)
		if !ok {
			unreadable = append(unreadable, *s)
			return nil, false
		}
		return &t, true
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
