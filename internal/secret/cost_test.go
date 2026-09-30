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
	//
	// The figures are CPU time and the cheapest of five rounds, not wall clock
	// and a single shot, because that combination is what flaked (#815): on a
	// loaded runner the 8x sample read 36x the 1x sample for code that costs
	// 8.2x, and the test failed for a tree that had not changed. fastestOf and
	// cpuClock carry the measurement; the minimum is over rounds because
	// everything left can only make a call look slower.
	//
	// The small fixture is measured over eight calls per round and the large over
	// one, which puts both windows at tens of milliseconds of CPU. That is the
	// clock's resolution made irrelevant — macOS counts getrusage in 1 ms — and
	// it is why the two are not measured the same way despite the ratio being
	// per-call either way.
	//
	// 400 candidates rather than the 2,000 this used, for a reason about FAILURE
	// rather than speed. The bar is a ratio, so the base size does not move it —
	// measured here, 400 candidates cost 5.0 ms of CPU and 3,200 cost 40.8 ms,
	// which is 8.1x for 8x the input. What the base decides is how long a
	// REGRESSION takes to be caught, and that is quadratic too: with the per-match
	// rescan restored, 400 candidates cost 78 ms and 3,200 cost 4.76 s, a 61x
	// reading that fails the 20x bar in under a minute including the warm calls.
	// At 2,000 the same mutation puts the large fixture past minutes, so the
	// assertion would eventually fail and nobody would ever see it — the suite
	// would go over its own timeout first.
	const base = 400
	const rounds = 5

	smallT := fastestOf(rounds, 8, func() { Detect(build(base)) })
	bigT := fastestOf(rounds, 1, func() { Detect(build(base * 8)) })

	t.Logf("%d candidates in %.3f ms of CPU, %d candidates in %.3f ms (%.1fx for 8x the input)",
		base, msOf(smallT), base*8, msOf(bigT), ratio(bigT, smallT))
	if smallT == 0 {
		t.Fatal("the base measurement was zero, so the ratio is meaningless")
	}
	if bigT > 20*smallT {
		t.Errorf("8x the candidates cost %.1fx the time — the line is being rescanned "+
			"per match, so the scan is quadratic in line length", ratio(bigT, smallT))
	}
}

// TestDetectAllocatesBoundedBytes is the mutation check for the mnemonic
// splitter, and getting a trustworthy instrument for it took three attempts,
// which is why the helper has a long comment.
//
// strings.FieldsFunc returns one slice no matter how many words it finds, so
// collecting the candidates costs exactly one extra allocation over walking them
// — an allocation COUNT cannot see the regression at all. Nor can an absolute
// byte bound: race instrumentation alone moves the same code by two and a half
// orders of magnitude, so a bound tight enough to be meaningful locally fails on
// a loaded runner.
//
// What it costs is SIZE — the slice holds every word of the content, which is
// what put 67% of Detect's allocations and about 10 KB per 4 KB save into a pass
// that stops at the first non-word. The regression therefore scales with the WORD
// COUNT, and the fixed form's cost does not scale with anything. So the assertion
// is a RATIO: eight times the content must not cost more than 1.5x the bytes per
// call.
//
// The four measurements the bar comes from, 8x the content:
//
//	                     plain          -race
//	walking (current)   1.0x           0.3x
//	collecting          10.6x          2.7x
//
// 1.5x is roughly even between the two sides, and all four are written down so a
// future change to the number is visible rather than remembered.
//
// The plain-run figure is now exact rather than sampled, and the reason is in
// the instrument rather than in the bar. On the pinned instrument the small and
// large fixtures read 130 B/op and 130 B/op — identical to the byte — and did so
// on 30 consecutive windows. Unpinned, the same fixture read anywhere from 130 to
// 9,962 B/op between adjacent windows of one run, because TotalAlloc is a
// process-wide counter and Go's regexp match machines live in a per-P sync.Pool:
// a goroutine that moves between Ps allocates a fresh machine inside whatever
// window it happens to be in. Re-arming GC per window had the same effect by
// emptying the pool again. It was measurement noise before — the 4 KB figure read
// 569, 8975, 3582, 569, 8975 across six runs and the 32 KB figure was bimodal at
// 421 or 4,203 — so this assertion used to fail intermittently and I would have
// shipped a flaky gate describing it as verified (#815). See
// allocatedBytesPerCall and pinnedToOneThread for the two causes.
func TestDetectAllocatesBoundedBytes(t *testing.T) {
	// Words with spaces throughout, which is the shape that made FieldsFunc
	// build a slice per word. ~480 words at 4 KB, ~3,840 at 32 KB.
	build := func(n int) string {
		return strings.Repeat("the relay listens on 2222 and answers ping on 443 for every subnet ", n)
	}
	small, big := build(60), build(480)
	if len(small) < 4000 || len(big) < 32000 {
		t.Fatalf("fixtures are %d and %d bytes, want at least 4000 and 32000", len(small), len(big))
	}

	// The SAME call count for both fixtures, which is a correctness property of
	// the ratio and not tidiness. allocatedBytesPerCall reads TotalAlloc across a
	// window, so anything the runtime allocates inside that window lands in both
	// figures divided by the same count — and the error it introduces is
	// one-sided. With equal counts, a contaminant of F bytes pulls the measured
	// ratio (pb·n+F)/(ps·n+F) toward 1 and never past it, so the test can lose
	// sensitivity but can never fail on noise. With the 100-against-8 counts this
	// used, F landed on the small-signal side with all of its weight and the
	// assertion failed on an unchanged implementation roughly half the time
	// (#815). Twelve calls of the 32 KB fixture is ~150 ms of window, which is
	// plenty of signal for the 8x ratio and short enough to keep the test quick.
	const calls = 12
	smallB := allocatedBytesPerCall(calls, func() { Detect(small) })
	bigB := allocatedBytesPerCall(calls, func() { Detect(big) })
	t.Logf("%.0f B/op at %d bytes, %.0f B/op at %d bytes (%.1fx for 8x the input)",
		smallB.bytesPerOp, len(small), bigB.bytesPerOp, len(big),
		ratio(bigB.bytesPerOp, smallB.bytesPerOp))

	// A walk that stops at the first non-word must not cost more as the content
	// grows. Both figures are per call over the same number of calls.
	if bigB.bytesPerOp > 1.5*smallB.bytesPerOp {
		t.Errorf("8x the content cost %.1fx the allocated bytes — something is "+
			"materialising the content, and the mnemonic pass collecting every "+
			"word is the shape that does it", ratio(bigB.bytesPerOp, smallB.bytesPerOp))
	}
}

func ratio(big, small float64) float64 {
	if small == 0 {
		return 0
	}
	return big / small
}

// msOf renders a seconds figure the way the cost tests report it. The raw value
// is a float count of nanoseconds, which prints as 4.98775e+06 and reads like an
// exponent rather than a duration.
func msOf(seconds float64) float64 { return seconds * 1000 }
