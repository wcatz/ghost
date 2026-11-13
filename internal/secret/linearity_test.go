package secret

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDetectIsLinearInLineLength pins the cost model: the shell-variable test
// and the quoted-argument scan need the rest of the LINE after a value, and
// recomputing it per match made a line of n `$var=` assignments cost n scans of
// the whole remainder. The 8,000-byte cap on a memory says nothing about a task
// note or anything reaching ImportMemory, where the detector runs before the
// clamp. The budgets are far above anything Ghost stores, so they are canaries.
func TestDetectIsLinearInLineLength(t *testing.T) {
	// One long line of REAL assignments (`$relay_addr = <24 characters>`), so
	// every candidate reaches valueIsCommand, where the quadratic cost was. There
	// is deliberately NO `-Flag` after the value: a flag makes valueIsCommand
	// return true, the walk returns on the first candidate, and the per-match
	// cost is never paid. The quoted scan has its own test,
	// TestDetectBoundsTheQuotedArgumentScan.
	shape := "$relay_addr = Kq9Xm2pL7wRt4Zb1XyZaQ3 "
	build := func(n int) string {
		return "$host" + strings.Repeat(shape, n/len(shape)+1)
	}

	// Confirm the fixture: a long candidate list and NO finding, or the walk
	// returns early and the cost is never paid.
	probe := build(1 << 16)[:1<<16]
	candidates := len(assignmentRe.FindAllStringSubmatchIndex(probe, -1))
	if want := 3 * (len(probe) / len(shape)) / 4; candidates < want {
		t.Fatalf("the fixture produced %d assignment candidates in %d bytes, want at least %d",
			candidates, len(probe), want)
	}
	if f, ok := Detect(probe); ok {
		t.Fatalf("the fixture is flagged by %q, so the per-match cost is never paid", f.Rule)
	}

	// Median of the per-round RATIOS over interleaved rounds: both sizes are
	// timed under the same conditions within a round, so a GC pause or stolen
	// timeslice inflates both and cancels. A single shot read 7.3x on a loaded
	// CI runner, and min-of-N raises the ratio.
	const rounds = 7
	Detect(build(1024)) // one-time regex machine setup
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

	// Canary on the large size alone: 6.9 s was measured on a CI runner, so 20 s
	// is a real margin.
	start := time.Now()
	Detect(bigText)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Detect on a %d-byte line took %v, want well under 20s — a per-line rescan is quadratic",
			len(bigText), elapsed)
	}

	// 4x the input must not cost more than 8x the time: the geometric midpoint
	// of linear (4x) and quadratic (16x). Honest readings are 3.2-4.3x, and 7.3x
	// on a loaded CI runner, so do not tighten it because a machine is quiet.
	if median > 8 {
		t.Errorf("4x the input cost %.2fx the time (median of %d rounds) — worse than linear", median, rounds)
	}
}

func BenchmarkDetect4KB(b *testing.B) {
	text := strings.Repeat("the relay resolves on 2222 and the tablet is redacted_something ", 80)[:4096]
	b.ReportAllocs()
	for b.Loop() {
		Detect(text)
	}
}
