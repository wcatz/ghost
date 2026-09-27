package secret

import (
	"strings"
	"testing"
)

// TestDetectDoesNotRescanTheLinePerAssignment is the mutation check for the
// line index, and it exists because the first version of the performance work
// did NOT have one.
//
// The linearity test I wrote alongside the line index passed with the quadratic
// version restored, and the reason is a fixture mistake worth recording: its line
// held many `$relay_addr` style tokens but no `=` and no `:`, so
// assignmentRe found no candidates at all, the loop body never ran, and the
// quadratic code was never on the path. A performance test that never executes
// the code it is bounding is a test that cannot fail.
//
// This fixture is built the other way round. Every repetition is a complete
// `$aN = <20 characters>` assignment, so the candidate list is long, and the keys
// are named so that NOTHING is flagged — the loop has to run to the end of the
// line, which is where the per-match cost is paid. A finding would return early
// and hide the shape again.
func TestDetectDoesNotRescanTheLinePerAssignment(t *testing.T) {
	// 20 characters that clear the length floor and are not a word segment, so
	// the key/value test passes them and the walk continues.
	const value = "Kq9Xm2pL7wRt4Zb1XyZaQ3" // 24 characters, over the 20 floor
	build := func(n int) string {
		var b strings.Builder
		b.Grow(n * 20)
		for i := 0; i < n; i++ {
			b.WriteString("$a")
			b.WriteString(itoa(i))
			b.WriteString(" = ")
			b.WriteString(value)
			b.WriteByte(' ')
		}
		return b.String()
	}

	// Confirm the fixture is the shape it claims to be: candidates, no finding.
	small := build(200)
	if got := len(assignmentRe.FindAllStringSubmatchIndex(small, -1)); got < 150 {
		t.Fatalf("the fixture produced %d assignment candidates, want ~200 — "+
			"the test is not exercising the path it is meant to bound", got)
	}
	if f, ok := Detect(small); ok {
		t.Fatalf("the fixture is flagged by %q, so Detect returns before the walk finishes "+
			"and the cost is never paid", f.Rule)
	}

	// Grow the candidate count 8x. A per-match rescan of the remainder is
	// quadratic, so the time grows ~64x; the line index makes it linear, so it
	// grows ~8x. The bar is 20x, which separates the two without depending on
	// how fast the machine is — the ratio is the whole assertion.
	const base = 2_000
	warm := build(64)
	Detect(warm) // one warm call: the regex machines and the word set are one-time

	measure := func(n int) (int64, float64) {
		text := build(n)
		start := now()
		Detect(text)
		return int64(len(text)), secondsSince(start)
	}
	_, smallT := measure(base)
	bigLen, bigT := measure(base * 8)

	t.Logf("%d candidates in %v, %d candidates in %v (%.1fx for 8x the input)",
		base, smallT, base*8, bigT, ratio(bigT, smallT))
	if smallT == 0 {
		t.Fatal("the base measurement was zero, so the ratio is meaningless")
	}
	if bigT > 20*smallT {
		t.Errorf("8x the candidates cost %.1fx the time — the line is being rescanned "+
			"per match, so the scan is quadratic in line length", ratio(bigT, smallT))
	}
	_ = bigLen
}

// TestDetectAllocatesBoundedBytes is the mutation check for the mnemonic
// splitter, and it is a BYTES assertion rather than an allocation COUNT because
// a count cannot see this regression at all.
//
// strings.FieldsFunc returns one slice no matter how many words it finds, so
// collecting the candidates costs exactly one extra allocation over walking them
// — AllocsPerRun cannot tell the two apart. What it costs is SIZE: the slice
// holds every word of the content, which is what put 67% of Detect's allocations
// and about 10 KB per 4 KB save into a pass that stops at the first non-word.
//
// The same shape in the same content measures 1,128 B/op now and 54,897 B/op
// with the collecting splitter, so the bound below sits three orders of magnitude
// above the current figure and an order of magnitude below the regression.
func TestDetectAllocatesBoundedBytes(t *testing.T) {
	// Words with spaces throughout, which is the shape that made FieldsFunc
	// build a slice per word. ~480 words.
	text := strings.Repeat("the relay listens on 2222 and answers ping on 443 for every subnet ", 60)
	if len(text) < 4000 {
		t.Fatalf("fixture is only %d bytes", len(text))
	}
	Detect(text) // warm: one-time setup is not what is being measured

	result := benchmarkDetect(text)
	t.Logf("%.0f B/op over %d bytes of content", result.bytesPerOp, len(text))
	if result.bytesPerOp > 4096 {
		t.Errorf("Detect allocates %.0f bytes per call on %d bytes of content, want at most 4096 — "+
			"something is materialising the content; the mnemonic pass collecting "+
			"every word is the shape that measures 54,897 B/op here",
			result.bytesPerOp, len(text))
	}
}

func ratio(big, small float64) float64 {
	if small == 0 {
		return 0
	}
	return big / small
}
