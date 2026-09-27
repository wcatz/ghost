package secret

import (
	"strings"
	"testing"
)

// TestDetectDoesNotMaterialiseTheText pins the allocation property, which is
// what the mnemonic rewrite was actually for.
//
// The profile of a save put 67% of Detect's allocations in strings.FieldsFunc
// inside the mnemonic pass: a []string of every word in the content, built
// before the first element was looked at, for a test that almost always stops at
// the first non-word. That was about 10 KB per 4 KB of content and it dwarfed
// everything else — the rules table allocated almost nothing.
//
// A count is a hard number rather than a timing, so this does not flake on a
// loaded runner, and it is the property that keeps a future refactor from
// reintroducing the slice.
func TestDetectDoesNotMaterialiseTheText(t *testing.T) {
	// Deliberately ordinary text with spaces: the shape that made FieldsFunc
	// allocate a slice per word.
	text := strings.Repeat("the relay listens on 2222 and answers ping on 443 for every subnet ", 60)
	if len(text) < 4000 {
		t.Fatalf("fixture is only %d bytes", len(text))
	}
	// One warm call, so the regex machines and the cached word set are not
	// counted.
	Detect(text)

	allocs := testing.AllocsPerRun(20, func() { Detect(text) })
	t.Logf("%d bytes, %.1f allocations per Detect", len(text), allocs)
	// The current implementation allocates once, for the match slices. Anything
	// that scales with the word count is the FieldsFunc regression back — with
	// roughly 480 words here, a collecting implementation lands in the hundreds.
	if allocs > 8 {
		t.Errorf("Detect on %d bytes allocated %.1f times per call, want at most 8 — "+
			"something is materialising the text again", len(text), allocs)
	}
}
