package memory

import (
	"fmt"
	"unicode/utf8"
)

// MaxContentLen is the byte cap on content written by any production Ghost
// writer — the MCP save/update/global tools, consolidation (reflection)
// proposals (memories and learned context), decision companion memories,
// and file imports all share this one constant. Bench/eval seeders and
// snapshot restore write synthetic or byte-exact data into throwaway or
// restored stores and are deliberately outside this contract.
//
// It is deliberately a constant rather than configuration: FTS rows,
// embeddings, search output, and session injection all stay bounded by
// it, and a knob would let one deployment silently reintroduce the loss
// this cap exists to prevent.
//
// 8000 (raised from 2000) gives incident and delegation-discipline records
// room for the second half that the old cap silently dropped, while keeping
// worst-case storage, token, and search costs at 4x the previous ceiling —
// still bounded, never unbounded.
const MaxContentLen = 8000

// TruncationMarker returns the marker ClampContent appends to content it
// cut: it names the exact limit so the stored value itself shows how much
// was lost, even to a reader with no access to the original save response.
// The limit is byte-based (MaxContentLen is a byte cap), so the marker says
// bytes, not chars.
func TruncationMarker() string {
	return fmt.Sprintf(" …[truncated at %d bytes]", MaxContentLen)
}

// TruncateUTF8 returns the longest prefix of s that is at most maxBytes bytes
// and ends on a UTF-8 rune boundary. It does not append a marker; callers that
// need to explain a lossy display should add their own display marker.
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// ClampContent applies MaxContentLen to s. Content that fits is returned
// byte-identical with cut=false — no marker, no rewrite. Content over the
// cap is cut at a rune boundary within the cap and TruncationMarker is
// appended, so truncation is explicit in the stored text; cut=true tells the
// caller to warn the saving agent that the full text did not land.
func ClampContent(s string) (string, bool) {
	if len(s) <= MaxContentLen {
		return s, false
	}
	return TruncateUTF8(s, MaxContentLen) + TruncationMarker(), true
}
