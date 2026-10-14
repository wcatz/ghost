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

// MaxTitleLen is the byte cap on a TITLE — a task's, a decision's — and on one
// entry of a decision's alternatives list. It is the smaller, display-oriented
// budget a title has rather than the prose budget a body has: a title is a
// label in a listing line, so the cap exists to keep the LINE bounded.
//
// It lives here beside MaxContentLen rather than in the MCP package that
// declared it, because it is not one package's rule: `ghost_task_create`,
// `ghost_decision_record` and the two portable importers all cut a title at it,
// and a second copy of the number in the importer is how an imported row stops
// matching what the writer produces for the same text (#1226). Same reasoning,
// same consequence: a title is CUT, never refused, because a shortened label is
// a different label about the same thing and a refusal would put the task or
// decision outside every `ghost export` — the exporter applies the importer's
// own predicates — on the day the operator needs the backup.
const MaxTitleLen = 300

// MaxAlternatives is the number of entries a decision's alternatives list is
// kept to, `ghost_decision_record`'s cap. `ghost_decisions_list` renders the
// whole list back on one line, so an unbounded list is a row that grows with
// whatever the artifact carried rather than with the decision (#1226).
//
// The list is TRUNCATED to the first MaxAlternatives entries, in artifact
// order, for the reason the tags above do not apply to: an entry is a piece of
// the record's own reasoning and the order it was recorded in is part of what
// it says, so keeping the first N keeps a prefix of what was said instead of
// an arbitrary half of it.
const MaxAlternatives = 20

// BoundAlternatives is the alternatives list a decision STORES: at most
// MaxAlternatives entries, in the order they were recorded, each cut at
// MaxTitleLen. `ghost_decision_record` and the portable import apply exactly
// these three bounds, through this one function, so the two cannot drift (#1226).
//
// The caller's slice is never written through. A list that is already within
// every bound is returned as it is — byte for byte, with no copy and no rewrite,
// because an ordinary decision must round-trip unchanged — and anything that
// needs bounding is answered with a fresh slice, so the artifact the caller
// parsed still holds what it held.
func BoundAlternatives(alternatives []string) []string {
	over := len(alternatives) > MaxAlternatives
	if !over {
		for _, alt := range alternatives {
			if len(alt) > MaxTitleLen {
				over = true
				break
			}
		}
	}
	if !over {
		return alternatives
	}
	bounded := make([]string, 0, MaxAlternatives)
	for i, alt := range alternatives {
		if i == MaxAlternatives {
			break
		}
		bounded = append(bounded, TruncateUTF8(alt, MaxTitleLen))
	}
	return bounded
}

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
