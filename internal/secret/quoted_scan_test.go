package secret

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDetectBoundsTheQuotedArgumentScan is the test that was missing, and the gap
// was real: neither cost test reached detectQuotedArgument, so nothing bounded
// its per-match cost. A comment claimed this file's sibling covered it, and it
// did not — the sibling's fixture has no flag after the value, so
// valueIsCommand is false, and detectQuotedArgument sits inside the same
// `if shellVar && valueIsCommand` as the flag test (secret.go:1421). Neither test
// reached it. This one does.
//
// Reaching it needs three things at once, which is why an ordinary fixture misses
// by default:
//
//   - a flag after the value, so valueIsCommand is true and the quoted scan runs;
//   - NO quoted argument that looks like a credential, so the scan returns false;
//   - a value that isCommandWord accepts, so the loop `continue`s instead of
//     falling through to a finding.
//
// The third is what makes the other two usable. `ConvertTo-SecureString` is a real
// cmdlet of 22 characters, which clears the 20-character value floor that
// assignmentRe requires — `Get-Credential` is 14 and matches nothing at all — and
// it is a command word, so the walk runs to the end of the line with every
// candidate reaching the expensive scan. A fixture that returned early here would
// measure nothing, which is the failure this file has now produced in three
// successive rounds.
func TestDetectBoundsTheQuotedArgumentScan(t *testing.T) {
	// The `-Label "..."` matters as much as the cmdlet does. detectQuotedArgument
	// starts with `if !li.lineQuoted(offset) { return }`, so a line with no quote
	// character anywhere never reaches the regex that is the actual cost — the
	// first version of this fixture had none, and the mutation that made the scan
	// read the whole line instead of the window PASSED at 7.85x, identical to the
	// honest reading, because the changed line was never executed. The quoted
	// literal is one character so it cannot be a credential in its own right; its
	// only job is to put a quote on the line.
	shape := "$cmd = ConvertTo-SecureString -Label \"pool\" -AsPlainText "
	build := func(n int) string { return strings.Repeat(shape, n) }

	// Confirm the shape, because a fixture that quietly stops being it keeps
	// passing while measuring something else. The candidate count and the
	// no-finding check are the two halves: the first says the walk has many
	// candidates, the second says it does not return before paying for them.
	probe := build(200)
	if got := len(assignmentRe.FindAllStringSubmatchIndex(probe, -1)); got != 200 {
		t.Fatalf("the fixture produced %d assignment candidates, want 200 — the test "+
			"is not exercising the path it is meant to bound", got)
	}
	if f, ok := Detect(probe); ok {
		t.Fatalf("the fixture is flagged by %q, so Detect returns before the walk "+
			"finishes and the per-match cost is never paid", f.Rule)
	}
	// And the property the fixture exists to establish: every candidate actually
	// reaches the quoted scan. Asserted by running the loop's own predicates, so
	// this cannot pass on a fixture that reaches the flag test and stops.
	li := newLineIndex(probe)
	matches := assignmentRe.FindAllStringSubmatchIndex(probe, -1)
	var reached int
	for i, m := range matches {
		windowEnd := len(probe)
		if i+1 < len(matches) {
			windowEnd = matches[i+1][0]
		}
		if probe[m[2*assignmentShellVar]:m[2*assignmentShellVar+1]] == "" {
			continue
		}
		if !valueIsCommand(li, m[2*assignmentValue+1], windowEnd) {
			continue
		}
		reached++
	}
	if reached != len(matches) {
		t.Fatalf("only %d of %d candidates reach the quoted scan, so the cost being "+
			"bounded is not the one this test claims", reached, len(matches))
	}

	// 8x the candidates, same estimator as the linearity test above: median of
	// per-round ratios over interleaved rounds, because a single shot is exposed to
	// whatever the runner is doing during the shorter measurement.
	//
	// The bar is 20x, and the arithmetic is worth showing because I got it wrong
	// first: with 8x the input the LINEAR prediction is 8x, so a bar of 8x sits
	// exactly on an honest measurement — this one read 7.85x and would have failed
	// on a quiet machine. Quadratic here is 64x, so the geometric midpoint of the
	// two is sqrt(8*64) = 22.6, and 20x is that with a little rounding down.
	// Measured honest reading: 6.6-8.0x.
	const rounds = 7
	Detect(build(50)) // one-time regex machine setup, outside every measurement
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
	median := ratios[len(ratios)/2]
	t.Logf("per-round ratios %.2f..%.2f, median %.2fx for 8x the candidates",
		ratios[0], ratios[len(ratios)-1], median)
	if median > 20 {
		t.Errorf("8x the candidates cost %.2fx the time (median of %d rounds, bar 20x) "+
			"— the quoted-argument scan is rescanning the rest of the line per match, "+
			"which would be 64x", median, rounds)
	}
}
