package secret

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDetectBoundsTheQuotedArgumentScan bounds the per-match cost of
// detectQuotedArgument, which no other cost test reaches. Reaching it needs a
// flag after the value (so valueIsCommand is true), no quoted argument that
// looks like a credential (so the scan returns false), a value isCommandWord
// accepts (so the loop continues to the next candidate), and a quote somewhere
// on the line (lineQuoted gates the regex that is the real cost).
// ConvertTo-SecureString is a 22-character cmdlet, which clears the 20-character
// value floor assignmentRe requires.
func TestDetectBoundsTheQuotedArgumentScan(t *testing.T) {
	shape := "$cmd = ConvertTo-SecureString -Label \"pool\" -AsPlainText "
	build := func(n int) string { return strings.Repeat(shape, n) }

	// Confirm the shape: a fixture that quietly stops being it keeps passing
	// while measuring something else.
	probe := build(200)
	matches := assignmentRe.FindAllStringSubmatchIndex(probe, -1)
	if len(matches) != 200 {
		t.Fatalf("the fixture produced %d assignment candidates, want 200", len(matches))
	}
	if f, ok := Detect(probe); ok {
		t.Fatalf("the fixture is flagged by %q, so the per-match cost is never paid", f.Rule)
	}
	li := newLineIndex(probe)
	for i, m := range matches {
		windowEnd := len(probe)
		if i+1 < len(matches) {
			windowEnd = matches[i+1][0]
		}
		if probe[m[2*assignmentShellVar]:m[2*assignmentShellVar+1]] == "" || !valueIsCommand(li, m[2*assignmentValue+1], windowEnd) {
			t.Fatalf("candidate %d does not reach the quoted scan", i)
		}
	}

	// 8x the candidates: linear predicts 8x, quadratic 64x, so the bar is 20x
	// (honest readings 6.6-8.0x). Median of per-round ratios over interleaved
	// rounds, because a single shot is exposed to runner noise.
	const rounds = 7
	Detect(build(50)) // one-time regex machine setup
	small, big := build(200), build(1600)
	ratios := make([]float64, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		Detect(small)
		s := time.Since(start)
		start = time.Now()
		Detect(big)
		b := time.Since(start)
		if s <= 0 || b <= 0 {
			t.Fatal("a measurement was zero, so the ratio is meaningless")
		}
		ratios = append(ratios, ratio(float64(b.Nanoseconds()), float64(s.Nanoseconds())))
	}
	sort.Float64s(ratios)
	if median := ratios[len(ratios)/2]; median > 20 {
		t.Errorf("8x the candidates cost %.2fx the time (median of %d rounds, bar 20x) "+
			"— the quoted-argument scan is rescanning the rest of the line per match", median, rounds)
	}
}
