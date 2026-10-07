package memory

import (
	"time"
)

// This is the ONE rule that names a memory's validity state. It lives here, in
// the package that owns the columns, because every consumer of that answer now
// reads it rather than re-deriving it:
//
//   - internal/assemble, at THREE call sites, which between them cover every
//     surface a reader can see a validity state on. readValidity is stage 2, the
//     one that DROPS a row outside its window; ValidityStateOf is what the
//     browsing surfaces use, having run no stages; ValidityLabel renders the
//     state onto a listing. All three are in internal/assemble/validity.go and all
//     three call here.
//   - the explain projection, which reports the state of every candidate it
//     describes (internal/assemble/explain.go).
//
// A second copy would be a second answer to "may this row be used", free to
// drift from the first the moment either is edited: a search that explains a row
// as "valid" while the assembler drops it as "expired" is exactly the #571 class
// of bug, one layer up. The two copies this had did not merely duplicate each
// other — they disagreed, because the assembler's treated a stamp of
// 0001-01-01 00:00:00 as unreadable (it lands on the zero time, which a failed
// parse also produces) while this one reads it as a real boundary, and it expires
// the row. TestValidityStateOfAgreesWithTheOneRule in internal/assemble holds the
// three call sites to this answer, including on that value.
//
// ONE reader deliberately does not agree, and it is not this rule: readableStampPtr
// in internal/memory/store.go treats the zero instant as no lower bound stated,
// because its callers compose a SUCCESSOR's window out of a source's and inheriting
// a year-1 start would date the successor before it existed. That is a different
// question — "which bounds does the replacement inherit?" rather than "is this row
// retired?" — and it is documented on that function and pinned by
// TestReplaceNonManualDoesNotTreatTheZeroInstantAsAStatedWindow.
//
// The states are exported because the assembler's stage records them into its
// trace and renders them into an answer, and those spellings are part of that
// surface's contract.
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
