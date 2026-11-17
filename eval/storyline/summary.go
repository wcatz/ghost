package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultMinQualified = 0.8

type armTally struct {
	arm              string
	runs             int // runs that qualified and count toward the scores below
	completed        int // runs that finished, qualified or not
	errors           int // runs that ended in an error, excluded from every count below
	noReport         int
	carried          int
	avoidFailed      int
	hasAvoids        bool
	hasCarries       bool
	judged, judgeYes int
	judgeUnread      int
	delivered        int
	blocks           [][]int
}

func tally(arm string, results []*Result, errored, noReport int) armTally {
	t := armTally{arm: arm, runs: len(results), errors: errored, noReport: noReport}
	for _, res := range results {
		var carries, carriesOK, avoidFail, deliveryFail int
		for _, c := range res.Checks {
			switch {
			case strings.HasPrefix(c.Name, "answer-carries:"):
				t.hasCarries = true
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
		for _, c := range res.Checks {
			switch {
			case c.Name == "judge:unreadable" || c.Name == "judge:error":
				t.judgeUnread++
			case c.Advisory && strings.HasPrefix(c.Name, "judge:"):
				t.judged++
				if c.Passed {
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

func isDeliveryCheck(name string) bool {
	for _, p := range []string{"injection-present:", "carry-forward:", "stale-original:", "expired-withheld:"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

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

func (t armTally) qualifiedRatio() string { return fmt.Sprintf("%d/%d", t.runs, t.completed) }

// armInconclusive is the same test the summary applies when it marks an arm
// inconclusive, so the exit decision cannot disagree with the table the reader
// saw. A completed run is one that reached a result, qualified or not; an errored
// run is reported as a failure in its own right and does not dilute the fraction.
func armInconclusive(cells []cell, storyKey, arm string, min float64) bool {
	qualified, completed := 0, 0
	for _, c := range cells {
		if c.story.Key != storyKey || c.arm != arm || c.err != nil || c.res == nil {
			continue
		}
		completed++
		if c.res.Disqualified == "" {
			qualified++
		}
	}
	return completed > 0 && float64(qualified)/float64(completed) < min
}

func formatSummary(cells []cell, runs int, minQualified float64) string {
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
		b.WriteString("| arm | runs | qualified | answer carries | stale/mistake used | judge yes (advisory) | block delivered | block bytes per session |\n")
		b.WriteString("|-----|------|-----------|----------------|--------------------|----------------------|-----------------|-------------------------|\n")
		for _, arm := range []string{armWithGhost, armFlatNotes, armWithoutGhost} {
			var results []*Result
			var disqualified []string
			errored, noReport := 0, 0
			for _, c := range cells {
				if c.story.Key != story.Key || c.arm != arm {
					continue
				}
				if c.err != nil || c.res == nil {
					errored++
					continue
				}
				if c.reportErr != nil {
					noReport++
				}
				if c.res.Disqualified != "" {
					disqualified = append(disqualified, fmt.Sprintf("run %d: %s", c.run, c.res.Disqualified))
					continue
				}
				results = append(results, c.res)
			}
			completed := len(results) + len(disqualified)
			if completed == 0 && errored == 0 {
				continue
			}
			t := tally(arm, results, errored, noReport)
			t.completed = completed
			carried, avoid, judge, delivered := "n/a", "n/a", "n/a", t.ratio(t.delivered)
			if t.hasCarries {
				carried = t.ratio(t.carried)
			}
			if t.hasAvoids {
				avoid = t.ratio(t.avoidFailed)
			}
			if t.judged > 0 {
				judge = fmt.Sprintf("%d/%d", t.judgeYes, t.judged)
			}
			if t.judgeUnread > 0 {
				if judge == "n/a" {
					judge = ""
				}
				judge += fmt.Sprintf(" (+%d unread)", t.judgeUnread)
				judge = strings.TrimSpace(judge)
			}
			if arm == armWithoutGhost {
				delivered = "n/a (empty block)"
			}
			if arm == armFlatNotes {
				delivered = "n/a (every note)"
			}
			runsCol := fmt.Sprintf("%d", t.completed)
			if t.errors > 0 {
				runsCol += fmt.Sprintf(" (+%d errored)", t.errors)
			}
			if t.noReport > 0 {
				runsCol += fmt.Sprintf(" (%d report not written)", t.noReport)
			}
			qualifiedCol := t.qualifiedRatio()
			if armInconclusive(cells, story.Key, arm, minQualified) {
				qualifiedCol += " (inconclusive)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
				arm, runsCol, qualifiedCol, carried, avoid, judge, delivered, t.blockSizes())
		}
		b.WriteString("\n")
	}
	var dropped []string
	for _, c := range cells {
		if c.res != nil && c.res.Disqualified != "" {
			dropped = append(dropped, fmt.Sprintf("- %s %s run %d: %s", c.story.Key, c.arm, c.run, c.res.Disqualified))
		}
	}
	if len(dropped) > 0 {
		b.WriteString("## disqualified runs\n\n")
		b.WriteString(strings.Join(dropped, "\n") + "\n\n")
	}
	return b.String()
}

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
	name := strings.Join(keys, "+")
	if len(name) > 180 {
		name = "all"
	}
	path := filepath.Join(dir, "summary-"+name+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write summary: %w", err)
	}
	return path, nil
}
