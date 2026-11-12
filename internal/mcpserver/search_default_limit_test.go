package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func limitProbeRows(n int) *memory.CandidateSet {
	rows := make([]memory.Candidate, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, memory.Candidate{
			Memory: memory.Memory{
				ID: fmt.Sprintf("probe%02d", i), ProjectID: "test-project", Category: "fact",
				Content: fmt.Sprintf("probe row number %02d for the default limit", i), CreatedAt: "2026-09-01 12:00:00", Importance: 0.7,
			},
			FTSRank: i, VectorRank: i, Base: 1.0 / float64(61+i), Decay: 1.0, Score: 1.0 / float64(61+i),
		})
	}
	return weakSet(rows...)
}

func rowsRendered(t *testing.T, out string) int {
	t.Helper()
	if !strings.Contains(out, "[ghost:outcome=") {
		t.Fatalf("answer carries no verdict line:\n%s", out)
	}
	return strings.Count(out, "- [")
}

func TestSearchDefaultAndExplicitLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want int
	}{
		{"default", map[string]any{"project_id": "test-project", "query": "probe"}, 5},
		{"below", map[string]any{"project_id": "test-project", "query": "probe", "limit": 3}, 3},
		{"above", map[string]any{"project_id": "test-project", "query": "probe", "limit": 8}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, session := newSearchSession(t, limitProbeRows(8))
			out := resultText(callTool(t, session, "ghost_memory_search", tc.args))
			if got := rowsRendered(t, out); got != tc.want {
				t.Errorf("rendered %d rows, want %d:\n%s", got, tc.want, out)
			}
		})
	}
}
