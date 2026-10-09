package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// armTally is one arm's numbers over the runs of one storyline: the counts the
// issue's success numbers are quoted in, and each session's block size.
type armTally struct {
	arm    string
	runs   int // runs that completed
	errors int // runs that ended in an error, excluded from every count below
	// carried counts runs in which EVERY answer-carries line passed.
	carried int
	// avoidFailed counts runs in which any answer-avoids line failed: the stale,
	// expired or mistaken claim was used unmarked. For correction-replay that is
	// "the mistake was repeated".
	avoidFailed int
	hasAvoids   bool
	// judged and judgeYes are the advisory paraphrase column.
	judged, judgeYes int
	// delivered counts runs in which every delivery line passed (Ghost arm).
	delivered int
	// blocks[run][session] is the size in bytes of the block the session was handed.
	blocks [][]int
}

// tally reads one arm's completed runs. It is a pure function of the results.
func tally(arm string, results []*Result, errored int) armTally {
	t := armTally{arm: arm, runs: len(results), errors: errored}
	for _, res := range results {
		var carries, carriesOK, avoidFail, deliveryFail int
		for _, c := range res.Checks {
			switch {
			case strings.HasPrefix(c.Name, "answer-carries:"):
				carries++
				if c.Passed {
					carriesOK++
				}
			case strings.HasPrefix(c.Name, "answer-avoids:"):
				t.hasAvoids = true
				if !c.Passed {
					avoidFail++
				}
			case isDeliveryCheck(c.Name):
				if !c.Passed {
					deliveryFail++
				}
			}
		}
		if carries > 0 && carriesOK == carries {
			t.carried++
		}
		if avoidFail > 0 {
			t.avoidFailed++
		}
		if deliveryFail == 0 {
			t.delivered++
		}
		if res.Judged {
			t.judged++
			for _, c := range res.Checks {
				if c.Advisory && c.Passed {
					t.judgeYes++
				}
			}
		}
		sizes := make([]int, len(res.Sessions))
		for i, s := range res.Sessions {
			sizes[i] = len(s.Block)
		}
		t.blocks = append(t.blocks, sizes)
	}
	return t
}

// isDeliveryCheck is the block-reading lines: the ones whose verdict is about
// what Ghost handed the session rather than what the agent said.
func isDeliveryCheck(name string) bool {
	for _, p := range []string{"injection-present:", "carry-forward:", "stale-original:", "expired-withheld:"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// blockSizes is each session's block size across the runs: one number when every
// run agreed, a min-max range when they did not.
func (t armTally) blockSizes() string {
	if len(t.blocks) == 0 {
		return "-"
	}
	var parts []string
	for i := range t.blocks[0] {
		lo, hi := t.blocks[0][i], t.blocks[0][i]
		for _, run := range t.blocks {
			if i < len(run) {
				lo, hi = min(lo, run[i]), max(hi, run[i])
			}
		}
		if lo == hi {
			parts = append(parts, fmt.Sprintf("%dB", lo))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%dB", lo, hi))
		}
	}
	return strings.Join(parts, ", ")
}

func (t armTally) ratio(n int) string { return fmt.Sprintf("%d/%d", n, t.runs) }

// formatSummary prints both arms side by side per storyline, with the block size
// of each session. The figures are counts of runs, quoted against the targets in
// docs/benchmarks.md and gated by nothing.
func formatSummary(cells []cell, runs int) string {
	var b strings.Builder
	seen := map[string]bool{}
	var order []Storyline
	for _, c := range cells {
		if !seen[c.story.Key] {
			seen[c.story.Key] = true
			order = append(order, c.story)
		}
	}
	for _, story := range order {
		fmt.Fprintf(&b, "## %s — %s (n=%d per arm)\n\n", story.Key, story.Title, runs)
		b.WriteString("| arm | runs | answer carries | stale/mistake used | judge yes (advisory) | block delivered | block bytes per session |\n")
		b.WriteString("|-----|------|----------------|--------------------|----------------------|-----------------|-------------------------|\n")
		for _, arm := range []string{armWithGhost, armWithoutGhost} {
			var results []*Result
			errored := 0
			for _, c := range cells {
				if c.story.Key != story.Key || c.arm != arm {
					continue
				}
				if c.err != nil || c.res == nil {
					errored++
					continue
				}
				results = append(results, c.res)
			}
			if len(results) == 0 && errored == 0 {
				continue
			}
			t := tally(arm, results, errored)
			avoid, judge, delivered := "n/a", "n/a", t.ratio(t.delivered)
			if t.hasAvoids {
				avoid = t.ratio(t.avoidFailed)
			}
			if t.judged > 0 {
				judge = fmt.Sprintf("%d/%d", t.judgeYes, t.judged)
			}
			if arm == armWithoutGhost {
				delivered = "n/a (empty block)"
			}
			runsCol := fmt.Sprintf("%d", t.runs)
			if t.errors > 0 {
				runsCol += fmt.Sprintf(" (+%d errored)", t.errors)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				arm, runsCol, t.ratio(t.carried), avoid, judge, delivered, t.blockSizes())
		}
		b.WriteString("\n")
	}
	return b.String()
}

// writeSummary writes the side-by-side tables, naming the storylines and the
// model so a pasted table carries what produced it.
func writeSummary(dir string, stories []Storyline, model, summary string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}
	var keys []string
	for _, s := range stories {
		keys = append(keys, s.Key)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Storyline summary\n\n- storylines: %s\n- model: %s\n\n%s", strings.Join(keys, ", "), model, summary)
	path := filepath.Join(dir, "summary-"+strings.Join(keys, "+")+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write summary: %w", err)
	}
	return path, nil
}
