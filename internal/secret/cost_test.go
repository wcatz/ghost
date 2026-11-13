package secret

import (
	"strconv"
	"strings"
	"testing"
)

// TestDetectDoesNotRescanTheLinePerAssignment is the mutation check for the
// line index. Every repetition is a complete `$aN = <24 characters>` assignment,
// so the candidate list is long, and the keys are named so that NOTHING is
// flagged: the walk has to run to the end of the line, where the per-match cost
// is paid. A fixture with no candidates, or one that returns a finding early,
// never executes the code it bounds and cannot fail.
func TestDetectDoesNotRescanTheLinePerAssignment(t *testing.T) {
	const value = "Kq9Xm2pL7wRt4Zb1XyZaQ3" // 24 characters, over the 20 floor
	build := func(n int) string {
		var b strings.Builder
		b.Grow(n * 20)
		for i := 0; i < n; i++ {
			b.WriteString("$a")
			b.WriteString(strconv.Itoa(i))
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

	// 8x the candidates: a per-match rescan is quadratic (~64x), the line index
	// is linear (~8x), so the bar is 20x. Figures are CPU time, cheapest of five
	// rounds (#815); the small fixture is measured over eight calls per round so
	// both windows are tens of milliseconds. Fixtures are built outside the timed
	// closure. The base is 400 candidates because a regression is quadratic too:
	// at 400 the restored rescan fails the bar in under a minute.
	const base = 400
	const rounds = 5

	smallText, bigText := build(base), build(base*8)
	smallT := fastestOf(rounds, 8, func() { Detect(smallText) })
	bigT := fastestOf(rounds, 1, func() { Detect(bigText) })

	t.Logf("%d candidates in %.3f ms of CPU, %d candidates in %.3f ms (%.1fx for 8x the input)",
		base, smallT*1000, base*8, bigT*1000, ratio(bigT, smallT))
	if smallT == 0 {
		t.Fatal("the base measurement was zero, so the ratio is meaningless")
	}
	if bigT > 20*smallT {
		t.Errorf("8x the candidates cost %.1fx the time — the line is being rescanned "+
			"per match, so the scan is quadratic in line length", ratio(bigT, smallT))
	}
}

func ratio(big, small float64) float64 {
	if small == 0 {
		return 0
	}
	return big / small
}
