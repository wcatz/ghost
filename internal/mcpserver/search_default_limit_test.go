package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The default `limit` of ghost_memory_search is a policy about how much of the
// retrieval window reaches a caller's block, not a neutral cap: the offline
// context bench measures the block it produces, and the shipped default is the
// one the graded corpus's precision and token figures were chosen from. These
// tests pin the three values the handler must hold: the default it applies when
// the caller names none, and an explicit caller value honoured unchanged on both
// sides of it — including above the default, which is the half a naive
// "min(limit, default)" would silently break.
//
// The rows are injected through the CandidateSet seam rather than saved into the
// store, so the population the assembler is offered is exactly n: the real save
// and retrieval paths fold lexical near-duplicates and window the legs, which
// would make the count this test reads a property of those stages instead of the
// budget under test.

// limitProbeRows builds n rows that both retrieval legs found, so no row is
// exposed to the leg-consensus gate and the only thing that can cap the block is
// the item budget.
func limitProbeRows(n int) *memory.CandidateSet {
	rows := make([]memory.Candidate, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, memory.Candidate{
			Memory: memory.Memory{
				ID:         fmt.Sprintf("probe%02d", i),
				ProjectID:  "test-project",
				Category:   "fact",
				Content:    fmt.Sprintf("probe row number %02d for the default limit", i),
				CreatedAt:  "2026-09-01 12:00:00",
				Importance: 0.7,
			},
			FTSRank:    i,
			VectorRank: i,
			Base:       1.0 / float64(61+i),
			Decay:      1.0,
			Score:      1.0 / float64(61+i),
		})
	}
	return weakSet(rows...)
}

// rowsRendered counts the memory lines in a formatted search answer.
func rowsRendered(t *testing.T, out string) int {
	t.Helper()
	if !strings.Contains(out, "[ghost:outcome=") {
		t.Fatalf("the answer carries no verdict line, so it is not the formatted search this test counts:\n%s", out)
	}
	return strings.Count(out, "- [")
}

// TestSearchDefaultLimitIsFive: with no `limit` argument the handler applies its
// default, which caps the block at five rows even when the store offers more.
func TestSearchDefaultLimitIsFive(t *testing.T) {
	_, session := newSearchSession(t, limitProbeRows(8))

	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "probe",
	}))
	if got := rowsRendered(t, out); got != 5 {
		t.Errorf("a search with no limit rendered %d rows, want the default of 5:\n%s", got, out)
	}
}

// TestSearchExplicitLimitIsHonoured: a caller's own `limit` is the bound, on
// both sides of the default. Above it is the half that a default applied as a
// hard cap would break; below it proves the default is not a floor.
func TestSearchExplicitLimitIsHonoured(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"below the default", 3, 3},
		{"above the default", 8, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, session := newSearchSession(t, limitProbeRows(8))

			out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
				"project_id": "test-project",
				"query":      "probe",
				"limit":      tc.limit,
			}))
			if got := rowsRendered(t, out); got != tc.want {
				t.Errorf("a search with limit %d rendered %d rows, want %d:\n%s", tc.limit, got, tc.want, out)
			}
		})
	}
}
