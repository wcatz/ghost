package secret

import (
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
	// Each repetition is a complete `$relay_addr = <24 characters> -asPlainText`
	// assignment, so the candidate list is long AND every candidate reaches both
	// per-match lookups — the flag test, which needs a `-Flag` after the value, and
	// the quoted-argument scan.
	shape := "$relay_addr = Kq9Xm2pL7wRt4Zb1XyZaQ3 -asPlainText "
	build := func(n int) string {
		return "$host" + strings.Repeat(shape, n/len(shape)+1)
	}

	type measurement struct {
		size int
		at   time.Duration
	}
	var runs []measurement
	// 64 KB and 256 KB. A megabyte was measured at ~740 ms and is deliberately
	// NOT asserted: it is four times past anything Ghost stores, and a budget an
	// order of magnitude from the limit is worth more as a canary than as a
	// threshold that fails on a loaded CI runner.
	for _, size := range []int{1 << 16, 1 << 18} {
		text := build(size)[:size]
		// Warm the caches so the first size is not paying for the regex
		// machine's own one-time setup.
		Detect(build(1024))
		start := time.Now()
		Detect(text)
		elapsed := time.Since(start)
		t.Logf("%8d bytes in %v", len(text), elapsed)
		if elapsed > 20*time.Second {
			t.Fatalf("Detect on a %d-byte line took %v, want well under 20s — a per-line rescan is quadratic", len(text), elapsed)
		}
		runs = append(runs, measurement{size: len(text), at: elapsed})
	}

	// Quadrupling the input must not more than roughly sextuple the time.
	// Quadratic would be 16x, which is the shape this test exists to catch. The
	// ratio is the assertion that carries the property, because it is
	// machine-independent: this held at 4.0x on a workstation and 5.0x under
	// race instrumentation on CI, while the absolute numbers for the same code
	// differed by 20x. The budgets above are canaries, not limits.
	for i := 1; i < len(runs); i++ {
		prev, cur := runs[i-1], runs[i]
		if cur.at > 6*prev.at {
			t.Errorf("Detect went from %v at %d bytes to %v at %d bytes, which is worse than linear",
				prev.at, prev.size, cur.at, cur.size)
		}
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
