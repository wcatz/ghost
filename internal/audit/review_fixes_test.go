package audit

import (
	"testing"
)

// TestAnUppercaseIDInANegLineSurvivesTheSidecarRoundTrip: an id is UPPER-case on
// the wire, and the one field shape that carries ids inside a segment — the `neg`
// line — used to REFUSE it.
//
// The refusal was invisible because it is total rather than intermittent: isHex
// accepted only 0-9a-f, and an id this package writes is hex(randomblob(16))
// upper-cased, so any id containing A-F (which is 15 of 16 letters, so
// effectively all of them) failed parseNegSegment, ReadSidecar refused the file,
// and the turn's whole audit was skipped for a reason that has nothing to do with
// the transcript. A tool that silently drops work on the majority of inputs is
// worse than one that fails loudly, so the case is pinned with a REAL id rather
// than a digit-only one that would have passed by luck.
func TestAnUppercaseIDInANegLineSurvivesTheSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()

	s := newTestSignals(t)
	s.AddProse("ignore 4F3A9C1E7B2D8A6F5C0E1234AB5678EF, that guidance is obsolete")

	path, err := WriteSidecar(dir, s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	got, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar on a sidecar naming an uppercase id: %v", err)
	}
	if !got.HasID("4F3A9C1E7B2D8A6F5C0E1234AB5678EF") {
		t.Errorf("the id this sidecar names was lost on the round trip")
	}
}

// TestAnIDWrittenInEitherCaseIsTheSameID: the same lowercase-only isHex that broke
// the `neg` line made memoryIDs MISS an upper-case id in a transcript — the
// mirror image of the same defect, and the more consequential one, because this is
// the path that decides a memory was identified at all.
//
// An agent quoting a stored id back is as likely to write it as Ghost rendered it
// (upper-case) as to write it lower-cased, so both spellings have to name the same
// memory and both have to be recorded in the same canonical case.
func TestAnIDWrittenInEitherCaseIsTheSameID(t *testing.T) {
	const upper = "4F3A9C1E7B2D8A6F5C0E1234AB5678EF"
	const lower = "4f3a9c1e7b2d8a6f5c0e1234ab5678ef"

	for _, spelling := range []string{upper, lower} {
		s := newTestSignals(t)
		s.AddProse("per " + spelling + " the port is 9090")
		if !s.HasID(upper) {
			t.Errorf("an id written %q did not name the memory", spelling)
		}
	}
}
