package secret

import (
	"strings"
	"testing"
)

// TestDetectDoesNotMaterialiseTheText pins the allocation property the mnemonic
// rewrite was for: a []string of every word in the content, built before the
// first element was looked at, was 67% of Detect's allocations. A count is a
// hard number that does not flake on a loaded runner.
func TestDetectDoesNotMaterialiseTheText(t *testing.T) {
	text := strings.Repeat("the relay listens on 2222 and answers ping on 443 for every subnet ", 60)
	if len(text) < 4000 {
		t.Fatalf("fixture is only %d bytes", len(text))
	}
	Detect(text) // warm: regex machines and the cached word set are not counted

	// Loose on purpose: 1.0 on a workstation, 40.0 under race instrumentation.
	// The text holds ~480 words, so the collecting implementation lands in the
	// hundreds and nothing else in Detect scales with the word count.
	allocs := testing.AllocsPerRun(20, func() { Detect(text) })
	if allocs > 150 {
		t.Errorf("Detect on %d bytes allocated %.1f times per call, want at most 150 — "+
			"something is materialising the text again", len(text), allocs)
	}
}
