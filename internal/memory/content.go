package memory

import (
	"fmt"
	"unicode/utf8"
)

// MaxContentLen is the byte cap on content written by any Ghost writer —
// the MCP save/update/global tools, consolidation (reflection) proposals,
// and file imports all share this one constant. It is deliberately a
// constant rather than configuration: FTS rows, embeddings, search output,
// and session injection all stay bounded by it, and a knob would let one
// deployment silently reintroduce the loss this cap exists to prevent.
//
// 8000 (raised from 2000) gives incident and delegation-discipline records
// room for the second half that the old cap silently dropped, while keeping
// worst-case storage, token, and search costs at 4x the previous ceiling —
// still bounded, never unbounded.
const MaxContentLen = 8000

// TruncationMarker returns the marker ClampContent appends to content it
// cut: it names the exact limit so the stored value itself shows how much
// was lost, even to a reader with no access to the original save response.
func TruncationMarker() string {
	return fmt.Sprintf(" …[truncated at %d chars]", MaxContentLen)
}

// ClampContent applies MaxContentLen to s. Content that fits is returned
// byte-identical with cut=false — no marker, no rewrite. Content over the
// cap is cut at a rune boundary within the cap and TruncationMarker is
// appended, so truncation is explicit in the stored text; cut=true tells
// the caller to warn the saving agent that the full text did not land.
func ClampContent(s string) (string, bool) {
	if len(s) <= MaxContentLen {
		return s, false
	}
	end := MaxContentLen
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + TruncationMarker(), true
}
