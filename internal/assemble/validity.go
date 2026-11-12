package assemble

import (
	"fmt"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

var stampLayouts = memory.StampLayouts

func parseStamp(s string) time.Time {
	for _, layout := range stampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

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

// ScopeContradicts reports whether a row's scope conflicts with the requested scope.
func ScopeContradicts(scope, want map[string]string) bool {
	return !memory.ScopeMatches(scope, want)
}

// BucketUnexpected reports whether a row is in a bucket the request did not ask for.
func BucketUnexpected(bucket, projectID string) bool {
	if projectID == "" {
		return false
	}
	return bucket != projectID && bucket != "_global"
}

const (
	validityValid      = memory.ValidityValid
	validityFuture     = memory.ValidityFuture
	validityExpired    = memory.ValidityExpired
	validityUnverified = memory.ValidityUnverified
	validityUnset      = memory.ValidityUnset
)

type validityVerdict struct {
	state       string
	unparseable []string
}

func readValidity(c memory.Candidate, now time.Time) validityVerdict {
	state, unreadable := memory.ValidityState(c.ValidFrom, c.ValidUntil, c.VerifiedAt, now)
	return validityVerdict{state: state, unparseable: unreadable}
}

func formatNote(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

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

// ValidityStateOf returns the stage-2 validity verdict for a stored triple at now.
func ValidityStateOf(from, until, verified *string, now time.Time) string {
	state, _ := memory.ValidityState(from, until, verified, now)
	return state
}

// ValidityLabel renders a stored validity triple for a listing.
func ValidityLabel(state string, from, until, verified *string) string {
	f, u, v := parseStampPtr(from), parseStampPtr(until), parseStampPtr(verified)
	if f == nil && u == nil && v == nil && state != validityUnverified {
		return ""
	}
	return validityLabel(state, f, u, v)
}

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

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

const (
	defaultMaxNoteBytes  = 512
	defaultMaxNotesBytes = 2048
)

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

func truncateAtRune(s string, n int) string { return clampBytes(s, n) }
