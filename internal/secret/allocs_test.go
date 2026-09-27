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
	// The bound is loose on purpose. This measures 1.0 on a workstation and
	// 40.0 under race instrumentation, because the instrumentation itself
	// allocates; a threshold tight enough to be exact is a threshold that fails
	// on someone else's machine.
	//
	// What it still separates is the thing worth separating. The text holds
	// roughly 480 words, so the collecting implementation this replaced lands in
	// the hundreds, and nothing else in Detect scales with the word count. A
	// bound of 150 sits an order of magnitude below the regression and an order
	// of magnitude above the instrumented baseline.
	if allocs > 150 {
		t.Errorf("Detect on %d bytes allocated %.1f times per call, want at most 150 — "+
			"something is materialising the text again", len(text), allocs)
	}
}
