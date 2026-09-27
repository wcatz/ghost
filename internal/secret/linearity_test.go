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
	// One long line, so the cost is per-line rather than per-record, and enough
	// `$var=` assignments per line to make a per-match rescan expensive.
	shape := "the relay resolves $relay_addr and $relay_port, then $relay_tls and $relay_ca, "
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
		if elapsed > time.Second {
			t.Fatalf("Detect on a %d-byte line took %v, want well under 1s — a per-line rescan is quadratic", len(text), elapsed)
		}
		runs = append(runs, measurement{size: len(text), at: elapsed})
	}

	// Quadrupling the input must not more than roughly sextuple the time.
	// Quadratic would be 16x, which is the shape this test exists to catch, and
	// the margin absorbs timer noise and the allocator.
	for i := 1; i < len(runs); i++ {
		prev, cur := runs[i-1], runs[i]
		if cur.at > 6*prev.at {
			t.Errorf("Detect went from %v at %d bytes to %v at %d bytes, which is worse than linear",
				prev.at, prev.size, cur.at, cur.size)
		}
	}
}

// TestDetectHandlesOneVeryLongLine states the same property as a floor rather
// than a ratio, for the shape the ratio cannot see: a single line, which is where
// the per-match rescan was quadratic.
func TestDetectHandlesOneVeryLongLine(t *testing.T) {
	text := strings.Repeat("$a $b $c $relay_addr $relay_port $relay_tls 2222 ", 8000)
	if len(text) < 100_000 {
		t.Fatalf("fixture is only %d bytes", len(text))
	}
	Detect(strings.Repeat("warm up the regex machine 2222 ", 100))
	start := time.Now()
	Detect(text)
	elapsed := time.Since(start)
	t.Logf("%d bytes in %v", len(text), elapsed)
	if elapsed > time.Second {
		t.Errorf("Detect on a %d-byte line took %v, want well under 1s", len(text), elapsed)
	}
}

func BenchmarkDetect4KB(b *testing.B) {
	text := strings.Repeat("the relay resolves on 2222 and the tablet is redacted_something ", 80)[:4096]
	b.ReportAllocs()
	for b.Loop() {
		Detect(text)
	}
}
