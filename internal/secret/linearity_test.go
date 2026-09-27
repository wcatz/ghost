package secret

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDetectIsLinearInLineLength pins the cost model, because the shape of the
// scan is a property of the control rather than an implementation detail.
//
// Every regex in the rules table is a linear scan, and the post-table passes
// walk their matches once. But two of those passes — the shell-variable test and
// the quoted-argument scan — need the rest of the LINE after a value, and they
// were recomputing it per match, so a line mentioning n `$var=` assignments cost
// n scans of the whole remainder. At 4 KB that is invisible; at 64 KB it is tens
// of milliseconds. And the 8,000-byte cap on a memory says nothing about a task
// note, a task title, a decision alternative, or anything reaching ImportMemory,
// where the detector runs before the clamp.
//
// The budgets are chosen to be far above anything Ghost stores, so they are
// canaries rather than limits. A quadratic scan that is invisible at 4 KB is
// tens of seconds at 256 KB, and this fails in a second.
func TestDetectIsLinearInLineLength(t *testing.T) {
	// One long line, so the cost is per-line rather than per-record — and built
	// from REAL ASSIGNMENTS, which is the correction. This fixture used to repeat
	// a sentence of `$relay_addr`-style *references* with no `=` or `:` anywhere
	// in it, so assignmentRe matched nothing, the two passes this paragraph is
	// about were never called, and the ratio below was timing the rules table and
	// the mnemonic walk instead. The same mistake made the cost test's
	// predecessor pass with the quadratic code in place. A performance test that
	// does not execute what it bounds cannot fail.
	//
	// Each repetition is a complete `$relay_addr = <24 characters>` assignment, so
	// the candidate list is long and every candidate reaches both per-match
	// lookups.
	//
	// There is deliberately NO `-Flag` after the value, and that is the second
	// round of a lesson this test has now taught twice. The flag was there to
	// "make each candidate reach the flag test" — and it did, and the test went on
	// passing with the quadratic code restored, at 4.02x against a 6x bar. A flag
	// makes valueIsCommand return TRUE, the walk returns on the FIRST candidate,
	// and the per-match cost is never paid at all. Reaching the expensive line is
	// not what exercises it; scanning the rest of the line to decide is, and that
	// happens whether the answer is yes or no. So the flag comes out.
	shape := "$relay_addr = Kq9Xm2pL7wRt4Zb1XyZaQ3 "
	build := func(n int) string {
		return "$host" + strings.Repeat(shape, n/len(shape)+1)
	}

	// Median of the per-round RATIOS, over interleaved rounds, rather than the
	// ratio of two single measurements.
	//
	// Two earlier versions of this failed, in opposite directions, and both were
	// the estimator rather than the property. A single shot per size passed
	// locally and read 7.3x on CI — 944 ms then 6.9 s — which is a loaded runner,
	// not a quadratic scan, and a 6x bar made that a red check. Taking the
	// minimum of five runs each was worse: the small measurement benefits more
	// from noise removal than the large one does (514 ms against 2.28 s here),
	// so min-of-N RAISES the ratio and hides nothing.
	//
	// The ratio per round is the robust form, because both sizes are timed under
	// the same conditions within a round, so a GC pause or a stolen timeslice
	// inflates both and cancels. Seven rounds, median, measured 4.26x with a
	// 3.62-4.89x spread under -race on this machine.
	const rounds = 7
	Detect(build(1024)) // one-time regex machine setup, outside every measurement
	smallText, bigText := build(1 << 16)[:1<<16], build(1 << 18)[:1<<18]
	ratios := make([]float64, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		Detect(smallText)
		small := time.Since(start)
		start = time.Now()
		Detect(bigText)
		big := time.Since(start)
		if small <= 0 || big <= 0 {
			t.Fatal("a measurement was zero, so the ratio is meaningless")
		}
		ratios = append(ratios, float64(big)/float64(small))
	}
	sort.Float64s(ratios)
	median := ratios[len(ratios)/2]
	t.Logf("per-round ratios %.2f..%.2f, median %.2fx for 4x the input",
		ratios[0], ratios[len(ratios)-1], median)

	// The canary, on the large size alone, since a quadratic scan is what would
	// blow it: measured 6.9 s on the CI runner that just read 7.3x, so 20 s is a
	// real margin rather than a formality.
	start := time.Now()
	Detect(bigText)
	bigElapsed := time.Since(start)
	t.Logf("%8d bytes in %v", len(bigText), bigElapsed)
	if bigElapsed > 20*time.Second {
		t.Errorf("Detect on a %d-byte line took %v, want well under 20s — a per-line rescan is quadratic",
			len(bigText), bigElapsed)
	}

	// Quadrupling the input must not more than 8x the time, and 8x is not a
	// round number: it is the geometric midpoint between the linear prediction
	// (4x) and the quadratic one (16x), which is the tightest bar that treats the
	// two failure directions symmetrically.
	//
	// What the bar is really resting on is the mutation, not the arithmetic:
	// restoring the per-match rescan of the rest of the line makes THIS test fail,
	// and TestDetectDoesNotRescanTheLinePerAssignment — which uses eight times
	// the candidates on one line — measures 66.3x for 8x the input. So the real
	// failure mode is nowhere near 8x and the bar's job is only to catch it. The measured honest readings are 3.2-4.1x for a single
	// shot here, 4.26x median under -race, and 7.3x on a loaded CI runner — all
	// four values written down so a future change to the number is visible rather
	// than remembered, and so nobody tightens it to 5x because their machine is
	// quiet.
	if median > 8 {
		t.Errorf("4x the input cost %.2fx the time (median of %d rounds) — worse than linear",
			median, rounds)
	}
}

// TestDetectHandlesOneVeryLongLine states the same property as a floor rather
// than a ratio, for the shape the ratio cannot see: a single line. Its fixture is
// assignments too, for the reason above — a line of bare words would not reach
// either pass.
func TestDetectHandlesOneVeryLongLine(t *testing.T) {
	text := strings.Repeat("$relay_addr = Kq9Xm2pL7wRt4Zb1XyZaQ3 -asPlainText ", 8000)
	if len(text) < 100_000 {
		t.Fatalf("fixture is only %d bytes", len(text))
	}
	Detect(strings.Repeat("$relay_addr = Kq9Xm2pL7wRt4Zb1XyZaQ3 -asPlainText ", 50))
	start := time.Now()
	Detect(text)
	elapsed := time.Since(start)
	t.Logf("%d bytes in %v", len(text), elapsed)
	if elapsed > 20*time.Second {
		t.Errorf("Detect on a %d-byte line took %v, want well under 20s", len(text), elapsed)
	}
}

func BenchmarkDetect4KB(b *testing.B) {
	text := strings.Repeat("the relay resolves on 2222 and the tablet is redacted_something ", 80)[:4096]
	b.ReportAllocs()
	for b.Loop() {
		Detect(text)
	}
}
