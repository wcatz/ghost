package assemble

import (
	"fmt"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// stampLayouts are the layouts SQLite's datetime() and date() produce. Both are
// accepted because the columns are unconstrained text: a row written with a
// date is as readable as one written with a timestamp, and rejecting the
// shorter form would silently turn a real claim into no claim.
var stampLayouts = []string{"2006-01-02 15:04:05", "2006-01-02"}

// parseStamp reads a stored timestamp, or the zero time when it cannot be read.
// A malformed created_at is treated as ancient rather than fresh, so it can
// never win a ranking by being unparseable.
func parseStamp(s string) time.Time {
	for _, layout := range stampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseStampPtr reads a stored timestamp into a pointer, and nil when the value
// is absent or unreadable. Callers distinguish the two through the trace, not
// through the pointer.
func parseStampPtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t := parseStamp(*s)
	if t.IsZero() {
		return nil
	}
	return &t
}

// ExpiredAt reports whether a row's validity window has closed. A nil boundary
// is unset, not open-ended: a row that states no expiry has made no claim about
// its own currency, which is the common case and must not be read as "expired".
func ExpiredAt(until *time.Time, now time.Time) bool {
	return until != nil && until.Before(now)
}

// NotYetValidAt reports whether a row's validity window has not opened yet.
func NotYetValidAt(from *time.Time, now time.Time) bool {
	return from != nil && from.After(now)
}

// ScopeContradicts reports whether a row's scope asserts a different place than
// the request asked for. Silence is not disagreement: a row that does not mention
// a key contradicts nothing, which is the rule that keeps unscoped knowledge
// eligible for every request.
//
// It is a named negation of memory.ScopeMatches rather than a second
// implementation of the same rule. The two questions are asked by two different
// consumers — this one by stage 3's predicate, the other by the linker, the
// fusion scope narrowing and every save-time check — and the only way they can
// be guaranteed to agree is for there to be one rule. A copy here would be a
// second answer to "may this row be used", free to drift from the first the
// moment either is edited: a linker that keeps a contradicting pair apart while
// a block shows both, or a leak metric that scores a contradicting row clean.
// ScopeContradicts and ScopeMatches are not independent decisions, so they are
// not independent code.
func ScopeContradicts(scope, want map[string]string) bool {
	return !memory.ScopeMatches(scope, want)
}

// BucketUnexpected reports whether a row is in a bucket the request did not ask
// for. It is a measurement predicate, not a membership rule: project membership
// is enforced in SQL, and a cross-project request makes no bucket unexpected by
// definition. Using it as a drop condition would be a second, silent filter on
// top of the one the legs already applied.
func BucketUnexpected(bucket, projectID string) bool {
	if projectID == "" {
		return false
	}
	return bucket != projectID && bucket != "_global"
}

// validityState names a row's validity against the request clock. The order
// matters: a row whose window has closed is expired even if it also carries an
// open start, because a closed window is the stronger statement.
const (
	validityValid      = "valid"
	validityFuture     = "future"
	validityExpired    = "expired"
	validityUnverified = "unverified"
	validityUnset      = "unset"
)

// validityVerdict is stage 2's decision for one row.
type validityVerdict struct {
	state string
	// unparseable names a stored value that is not a readable claim. It is
	// reported rather than treated as either valid or invalid: SQLite holds
	// these columns as unconstrained text, and reading a value nobody can
	// interpret as "valid" would assert something the store never said.
	unparseable []string
}

// readValidity interprets a row's validity columns against the request clock.
// A missing or unreadable boundary is unset, so the row keeps its place unless a
// readable boundary excludes it.
func readValidity(c memory.Candidate, now time.Time) validityVerdict {
	v := validityVerdict{state: validityUnset}
	read := func(s *string) (*time.Time, bool) {
		if s == nil {
			return nil, false
		}
		t := parseStamp(*s)
		if t.IsZero() {
			v.unparseable = append(v.unparseable, *s)
			return nil, false
		}
		return &t, true
	}
	from, _ := read(c.ValidFrom)
	until, _ := read(c.ValidUntil)
	_, verifiedReadable := read(c.VerifiedAt)

	switch {
	case ExpiredAt(until, now):
		v.state = validityExpired
	case NotYetValidAt(from, now):
		v.state = validityFuture
	case from == nil && until == nil && !verifiedReadable:
		// No claim at all: the row says nothing about its own currency, which
		// is the state of every memory written before the columns existed.
		v.state = validityUnset
	case !verifiedReadable:
		// A window with no readable verification is unverified: the row claims
		// a period and nothing vouches for it. verified_at is a flag in v1, so
		// this state is recorded and the row is kept.
		v.state = validityUnverified
	default:
		v.state = validityValid
	}
	return v
}

// formatNote builds a bounded diagnostic note.
func formatNote(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// clampBytes truncates to at most n bytes on a rune boundary, so a multi-byte
// character is never cut in half and the result is always valid UTF-8.
func clampBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b begins a UTF-8 sequence. The byte at the cut point
// starts a character exactly when it is not a continuation byte (0b10xxxxxx).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// noteBudget is the documented default bound on one diagnostic note and on all
// of them together. A pathological corpus (thousands of unreadable validity
// values) must not be able to inflate a response, and a note nobody reads in
// full is still a note that counted against the budget.
const (
	defaultMaxNoteBytes  = 512
	defaultMaxNotesBytes = 2048
)

// boundNotes keeps notes within the per-note and total bounds, in order, so the
// first thing that happened is the thing that survives.
func boundNotes(notes []string, perNote, total int) []string {
	if perNote <= 0 {
		perNote = defaultMaxNoteBytes
	}
	if total <= 0 {
		total = defaultMaxNotesBytes
	}
	kept := make([]string, 0, len(notes))
	used := 0
	for _, n := range notes {
		if len(n) > perNote {
			n = truncateAtRune(n, perNote)
		}
		if used+len(n)+1 > total {
			break
		}
		kept = append(kept, n)
		used += len(n) + 1
	}
	return kept
}

// truncateAtRune cuts a string to at most n bytes without splitting a character.
func truncateAtRune(s string, n int) string { return clampBytes(s, n) }
