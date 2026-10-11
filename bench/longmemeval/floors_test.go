package main

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The hybrid CI floors are one fact recorded in two places: the OVERALL row of
// the run they were derived from (docs/benchmarks.md), and the -floors spec the
// hybrid job gates on (.github/workflows/longmemeval.yml), which also names the
// run. #771 is what happens when the two are allowed to disagree. The shipped
// floors came from a warm local run made BEFORE the harness adopted the
// search_document:/search_query: task prefixes, so every dispatch since has
// measured a different vector space from the one the numbers came from: the job
// floor-CHECKED a run instead of validating it, and its exit status was not
// evidence of a regression.
//
// Keeping them in step is a rule, not a coincidence, so it is asserted here
// rather than left to whoever edits one of them next:
//
//   - the workflow's floors and the doc's recorded floors are the same numbers;
//   - both name the same run, so a floor can be traced to the measurement it
//     came from — which is the only thing that distinguishes a re-derived floor
//     from one lifted out of a superseded vector space;
//   - each floor is that run's observed value minus the documented margin, so a
//     floor no recorded derivation produces fails, including one from a run in a
//     different vector space;
//   - the workflow header no longer carries the notice that said the gate was
//     not a regression signal, because the floors are real again.
func TestTheHybridFloorsComeFromTheRecordedPrefixedRun(t *testing.T) {
	wf := mustReadFile(t, "../../.github/workflows/longmemeval.yml")
	doc := mustReadFile(t, "../../docs/benchmarks.md")

	hybrid := yamlSection(wf, "hybrid")
	if hybrid == "" {
		t.Fatal("no `hybrid:` job in .github/workflows/longmemeval.yml; the floors have nothing to be derived for")
	}

	// The fts job gates on floors too, so the spec has to be read out of the
	// hybrid job's own section rather than out of the whole file.
	spec := floorsSpec.FindStringSubmatch(hybrid)
	if spec == nil {
		t.Fatalf("the hybrid job has no `-floors \"r5=…,ndcg10=…\"` line:\n%s", hybrid)
	}
	wfR5, _ := strconv.ParseFloat(spec[1], 64)
	wfNDCG, _ := strconv.ParseFloat(spec[2], 64)

	// The doc is where the run, the observed values and the margin live, so it
	// is read first and the workflow is checked against it.
	line := recordedFloorLine(doc)
	if line == "" {
		t.Fatal("docs/benchmarks.md records no hybrid floor derivation; without it the workflow's floors cannot be traced to a run")
	}
	rec := docFloorRE.FindStringSubmatch(line)
	if rec == nil {
		t.Fatalf("the recorded hybrid floor derivation is unreadable; it must name the run, the observed OVERALL metrics, the floors and the margin\n%s", line)
	}
	docRun := rec[1]
	docR5, _ := strconv.ParseFloat(rec[2], 64)
	docNDCG, _ := strconv.ParseFloat(rec[3], 64)
	docFloorR5, _ := strconv.ParseFloat(rec[4], 64)
	docFloorNDCG, _ := strconv.ParseFloat(rec[5], 64)
	docMargin, _ := strconv.ParseFloat(rec[6], 64)

	if wfR5 != docFloorR5 || wfNDCG != docFloorNDCG {
		t.Errorf("the workflow gates on r5=%.2f,ndcg10=%.2f but docs/benchmarks.md records r5=%.2f,ndcg10=%.2f\n"+
			"these are one fact in two places, and #771 is what their drift costs", wfR5, wfNDCG, docFloorR5, docFloorNDCG)
	}

	wfRun := derivedRunID(hybrid)
	switch {
	case wfRun == "":
		t.Errorf("the hybrid job names no run on any line that mentions its floors:\n%s", hybrid)
	case wfRun != docRun:
		t.Errorf("the workflow derives its floors from run %s but docs/benchmarks.md records run %s", wfRun, docRun)
	}

	// The margin is the fts floor's: enough headroom for RRF tie jitter, tight
	// enough to catch a regression.
	if docMargin < 0.005 || docMargin > 0.03 {
		t.Errorf("the documented margin %.3f is outside the 0.005-0.03 band the fts floor uses; a floor this far below its run is not a regression signal", docMargin)
	}

	// Each floor must be the recorded observed value less the recorded margin,
	// rounded to the two decimals a floor is quoted at. One step of slack is
	// allowed either way, for two reasons that both bite in practice: a
	// half-way difference rounds to 0.88 or 0.89 depending on the rounding
	// rule, and 0.905 - 0.020 is not representable in binary floating point at
	// all. Without the slack the test rejects the very derivation it is meant
	// to accept; with it a floor from another vector space still fails, because
	// that floor is a step or more away from this run's numbers.
	//
	// This is the assertion a floor from another vector space fails.
	cases := []struct {
		name            string
		observed, floor float64
	}{
		{"r5", docR5, wfR5},
		{"ndcg10", docNDCG, wfNDCG},
	}
	const step = 0.01
	for _, c := range cases {
		want := math.Round((c.observed-docMargin)*100) / 100
		if d := math.Abs(c.floor - want); d > step {
			t.Errorf("%s: recorded OVERALL %.3f at a %.3f margin derives a floor of %.2f, but the workflow gates on %.2f\n"+
				"one of the two comes from a different run", c.name, c.observed, docMargin, want, c.floor)
		}
	}

	// The notice that said the gate was not a regression signal lives in the
	// workflow's header, which is outside the job section everything else here
	// reads, so it is checked across the whole file.
	for _, stale := range []string{"RE-BASELINE", "predates the harness"} {
		if strings.Contains(wf, stale) {
			t.Errorf("the workflow still says %q; the floors are derived from a recorded prefixed-space run, so the notice is false", stale)
		}
	}
}

// floorsSpec is a job's gate. Both jobs carry it, which is why every lookup
// here is scoped to one job's section of the file.
var floorsSpec = regexp.MustCompile(`-floors\s+"r5=([0-9.]+),ndcg10=([0-9.]+)"`)

// runIDLine marks the one line in docs/benchmarks.md that records the hybrid
// floor derivation.
const runIDLine = "Hybrid CI floors, prefixed space:"

// docFloorRE reads that line: run id, the observed OVERALL R@5 and NDCG@10, the
// floors they produce, and the margin between them. Everything the test needs
// is on that line, so a re-baseline is an edit in one place.
var docFloorRE = regexp.MustCompile(`run ` + "`?" + `(\d{6,})` + "`?" +
	`.*?R@5 ([0-9.]+).*?NDCG@10 ([0-9.]+).*?r5=([0-9.]+).*?ndcg10=([0-9.]+).*?([0-9.]+) margin`)

var runIDRE = regexp.MustCompile(`run (\d{6,})`)

// derivedRunID returns the run a job names as the source of its floors, or "".
// The search is scoped to the lines that mention floors, because a hybrid job's
// comments also cite runs for other reasons (why the timeout is what it is) —
// a bare search of the job would read one of those as the derivation.
func derivedRunID(job string) string {
	var id string
	for _, line := range strings.Split(job, "\n") {
		if !strings.Contains(strings.ToLower(line), "floor") {
			continue
		}
		for _, m := range runIDRE.FindAllStringSubmatch(line, -1) {
			if id != "" && id != m[1] {
				return "" // two runs on floor lines: no single derivation
			}
			id = m[1]
		}
	}
	return id
}

// recordedFloorLine returns the line of doc that carries the hybrid floor
// derivation, or "" if the doc records none.
func recordedFloorLine(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, runIDLine) {
			return line
		}
	}
	return ""
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// yamlSection returns the body of a job in a workflow's `jobs:` map, from its
// own key to the next sibling key at the same indent. The hybrid and fts jobs
// both gate on floors, so reading the whole file would conflate them.
func yamlSection(wf, job string) string {
	key := "\n  " + job + ":"
	start := strings.Index(wf, key)
	if start < 0 {
		return ""
	}
	rest := wf[start+len(key):]
	if end := siblingKeyIndex(rest); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// siblingKeyIndex returns the byte offset of the next job key at two-space
// indent, or -1. Only keys count: a deeper-indented line inside the job belongs
// to it, and a comment or a blank line ends nothing.
func siblingKeyIndex(body string) int {
	offset := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		if isSiblingJobKey(trimmed) {
			return offset
		}
		offset += len(line)
	}
	return -1
}

// isSiblingJobKey reports whether a line is a jobs: key at two-space indent
// rather than something nested inside this job (a deeper-indented mapping, a
// sequence item, a comment or a blank line).
func isSiblingJobKey(line string) bool {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return false
	}
	name := strings.TrimSpace(line)
	if name == "" || strings.HasPrefix(name, "#") || strings.HasPrefix(name, "-") {
		return false
	}
	return strings.HasSuffix(name, ":")
}
