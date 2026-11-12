package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type armTally struct {
	arm              string
	runs             int // runs that completed
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
		for _, arm := range []string{armWithGhost, armFlatNotes, armWithoutGhost} {
			var results []*Result
			errored, noReport := 0, 0
			for _, c := range cells {
				if c.story.Key != story.Key || c.arm != arm {
					continue
				}
				if c.err != nil || c.res == nil {
					errored++
					continue
				}
				results = append(results, c.res)
				if c.reportErr != nil {
					noReport++
				}
			}
			if len(results) == 0 && errored == 0 {
				continue
			}
			t := tally(arm, results, errored, noReport)
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
			runsCol := fmt.Sprintf("%d", t.runs)
			if t.errors > 0 {
				runsCol += fmt.Sprintf(" (+%d errored)", t.errors)
			}
			if t.noReport > 0 {
				runsCol += fmt.Sprintf(" (%d report not written)", t.noReport)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				arm, runsCol, carried, avoid, judge, delivered, t.blockSizes())
		}
		b.WriteString("\n")
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
