package assemble

import (
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// stampLayouts are the layouts SQLite's datetime() and date() produce, and they
// live with the column they read rather than here: both are accepted because the
// columns are unconstrained text, and a reader that accepted only the shape
// Ghost writes would silently turn a real claim into no claim.
var stampLayouts = memory.StampLayouts

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

// parseStampPtr reads a stored VALIDITY stamp into a pointer, and nil when the
// value is absent or unreadable. Callers distinguish the two through the trace,
// not through the pointer.
//
// It reads through memory.ParseStamp rather than the local parseStamp, because it
// has to agree with the rule that decided the state it renders. The two parsers
// differ on exactly one input: a stamp of 0001-01-01 00:00:00 parses successfully
// and lands on the zero time, so a zero-rejecting reader cannot tell it from a
// value that failed to parse. memory.ParseStamp reports it as READABLE —
// time.Parse accepts it, and the store's own TestImportVerification asserts that
// it should — and memory.ValidityState therefore calls such a row expired. A label
// renderer still using the zero-rejecting parse would hand the caller all-nil
// pointers, take the "nothing to render" early return, and print no marker on a
// row the search assembler had already dropped as retired.
//
// parseStamp above stays as it is, because it reads created_at and resolved_at,
// where a malformed value being treated as ancient is deliberate (see decayRank:
// an unparseable created_at must never spuriously win).
func parseStampPtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, ok := memory.ParseStamp(*s)
	if !ok {
		return nil
	}
	return &t
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

// The state names are memory.ValidityState's, aliased here so this file's
// pipeline reads the same vocabulary the store's explain mode reports. The rule
// itself lives in internal/memory (validity.go): the assembler's stage and
// explain mode both read it, and two copies would be two answers to "may this
// row be used".
const (
	validityValid      = memory.ValidityValid
	validityFuture     = memory.ValidityFuture
	validityExpired    = memory.ValidityExpired
	validityUnverified = memory.ValidityUnverified
	validityUnset      = memory.ValidityUnset
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
//
// The decision is memory.ValidityState's — the same call explain mode makes for a
// row the search returned — and only the collection of unreadable values is
// done here, because a note is this package's to emit and not the store's.
func readValidity(c memory.Candidate, now time.Time) validityVerdict {
	state, unreadable := memory.ValidityState(c.ValidFrom, c.ValidUntil, c.VerifiedAt, now)
	return validityVerdict{state: state, unparseable: unreadable}
}

// formatNote builds a bounded diagnostic note.
func formatNote(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// validityLabel renders a row's validity claim for a listing, or "" when it
// states none.
//
// This is the one place the claim is turned into words, and every surface reaches
// it — Item.Line below, and ValidityLabel for the surfaces that still render
// memory.Memory directly — so a reader comparing a searched answer with a
// listing sees the same fields in the same place, which is the convergence the
// architecture document asks for. A second implementation would let the two drift
// the moment either was edited, which is how a caller ends up believing a window
// means something in one place and nothing in the other.
//
// A state is rendered whenever the values do not already say it:
//
//   - unverified, because "true until then, checked by nobody" and "true until
//     then, checked on the 20th" are different things to act on and only the
//     first has nothing in the numbers to say so.
//   - expired and not-yet-valid, because a browsing surface — a list, a
//     cross-project search, a project context — has not run stage 2 and will
//     otherwise print a retired claim with nothing marking it retired. That is the
//     worst available reading of a dated row, and it is the reading a caller gets
//     if the label omits the one word that settles it.
//
// On the assembler's own surfaces the last two never appear: stage 2 drops those
// rows before stage 10 renders them, so Item.Line is only ever handed valid,
// unverified or unset. A surface that has not run the stages derives the state
// itself with ValidityStateOf, because it cannot claim a row is out of date
// without knowing what date it is.
func validityLabel(state string, from, until, verified *time.Time) string {
	var b strings.Builder
	if from != nil {
		b.WriteString(" valid from ")
		b.WriteString(stampText(from, false))
	}
	if until != nil {
		b.WriteString(" until ")
		b.WriteString(stampText(until, true))
	}
	// Rendered whenever it is recorded, window or not: with a window it is the
	// moment somebody last checked the claim, and without one it is the whole
	// claim. Printing it only in the second case would make "true until then,
	// checked on the 20th" and "true until then, checked last week" identical.
	if verified != nil {
		b.WriteString(" verified ")
		b.WriteString(stampText(verified, false))
	}
	switch state {
	case validityExpired:
		b.WriteString(" expired")
	case validityFuture:
		b.WriteString(" not yet valid")
	case validityUnverified:
		b.WriteString(" unverified")
	}
	return b.String()
}

// ValidityStateOf is the stage-2 verdict for a stored triple read against a
// clock, for the surfaces that have not run the pipeline and so cannot inherit
// one from a trace. It is the SAME call readValidity makes, so a browsing surface
// and a searched one cannot disagree about whether a row is retired.
//
// It was a second copy until #583, with its own parseStamp, and the two did not
// merely duplicate each other — they disagreed, which is worse. Both treated a
// value of 0001-01-01 00:00:00 as unreadable, because a stamp that parses to the
// zero time looked like a stamp that failed to parse. memory.ParseStamp reports
// it as READABLE (time.Parse accepts it, and the store's own
// TestImportVerification asserts that it should), so a row whose valid_until is
// that string is expired: its window closed at the zero instant. readValidity
// moved to the store's parser first and therefore started saying expired while
// this copy still said unset, so a searched row was dropped while the same row
// was still listed by every browsing surface. Delegating is what makes the
// surfaces agree, and the agreement is the point — not a tidier factoring.
//
// An unreadable value is not a state: it reads as unset here, and the trace is
// where the value itself is reported, since a string-only verdict has nowhere to
// put it.
func ValidityStateOf(from, until, verified *string, now time.Time) string {
	state, _ := memory.ValidityState(from, until, verified, now)
	return state
}

// ValidityLabel renders a stored validity triple for a listing, for the surfaces
// that render memory.Memory rather than an Item: ghost_memories_list and
// ghost_search_all. A surface that runs the pipeline does not need it — stage 2
// has already dropped the retired rows before Item.Line is handed one.
//
// It takes the stored strings because those are what memory.Memory carries, and
// it parses them here so the parsing has one implementation: the column is
// unconstrained text, so a row can hold a value this build does not read, and
// deciding what to show about a value nothing can interpret is the renderer's
// call, not each caller's. An unreadable value renders as nothing — the row
// carries no readable claim, which is what stage 2 called unset and what the
// trace reports as validity_unparseable.
//
// state is the row's verdict against the clock the caller is rendering for;
// ValidityStateOf is what a current listing passes, and memory.ValidityAt at the
// requested instant is what an as_of listing passes, so the verdict is the one
// ghost_memory_search reaches at that instant. An empty state renders the values
// and no verdict, and is right where the caller already knows the row survived
// stage 2.
func ValidityLabel(state string, from, until, verified *string) string {
	f, u, v := parseStampPtr(from), parseStampPtr(until), parseStampPtr(verified)
	if f == nil && u == nil && v == nil && state != validityUnverified {
		return ""
	}
	return validityLabel(state, f, u, v)
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
// first thing that happened is the thing that survives. Order is therefore load-
// bearing, not cosmetic: a caller who sets a total below its own first note gets an
// empty list rather than a truncated one, because a partial note is worse than
// none. Put the sentence that qualifies the result at the front for that reason.
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
