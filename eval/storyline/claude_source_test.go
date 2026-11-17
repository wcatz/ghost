package main

import (
	"slices"
	"strings"
	"testing"
)

// TestClaudeCodeSourceDrivesTheAnnotatedArcsWithATrace: the annotated-edge
// misses were observed on the claude-code source, and the per-pair trace is what
// says whether the classifier was even asked. A regression for both halves has
// to drive the same routing AND grade the captured trace, not just the edge.
func TestClaudeCodeSourceDrivesTheAnnotatedArcsWithATrace(t *testing.T) {
	for _, args := range [][]string{
		arcPhaseArgs("supersede", "proj", "claude-code"),
		arcPhaseArgs("resolve", "proj", "claude-code"),
	} {
		i := slices.Index(args, "--source")
		if i < 0 || args[i+1] != "claude-code" {
			t.Errorf("%v does not classify through claude-code", args)
		}
		if !slices.Contains(args, "--apply") {
			t.Errorf("%v would grade a dry run", args)
		}
	}

	for _, makeStory := range []func() Storyline{RenamedEndpoint, RecategorisedReplacement, RegressedFix} {
		s := makeStory()
		t.Run(s.Key, func(t *testing.T) {
			for _, pair := range reversals(s) {
				// Synthetic ids: the grade reads ids from the store, and the trace
				// parser reads them from the pass's own output. Both are strings here.
				newer, older := "aaaaaaaa0001", "bbbbbbbb0002"
				res := &Result{
					Opening: map[string]string{pair.newer.Key: newer, pair.older.Key: older},
					State: State{
						Links:  []Link{{Source: newer, Target: older, Relation: "supersedes"}},
						Stamps: map[string]Stamp{newer: {CreatedAt: "2026-07-01 00:00:00"}, older: {CreatedAt: "2026-01-01 00:00:00"}},
					},
					SupersedeTrace: []SupersedeTrace{{NewerID: newer, OlderID: older, Status: "proposed", Detail: "supersedes"}},
				}
				check := supersedeEdge(res, pair)
				if !check.Passed {
					t.Errorf("%s failed: %s", check.Name, check.Detail)
				}
				// Now drop the edge but keep the trace: the check fails and NAMES the
				// classifier as the stage that rejected the pair.
				res.State.Links = nil
				check = supersedeEdge(res, pair)
				if check.Passed || !strings.Contains(check.Detail, "classifier rejected") {
					t.Errorf("a missed edge with a proposed trace = %+v, want the classifier diagnosis", check)
				}
			}
		})
	}
}
