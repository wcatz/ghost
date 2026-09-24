package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSearchExplainReturnsDiagnosisThroughTheTool: an agent debugging a bad
// result must be able to ask for the breakdown through the tool it already
// calls, not reach into a package. This asserts the argument is accepted, the
// response is machine-readable, and the fields an agent needs to attribute
// blame are actually present.
func TestSearchExplainReturnsDiagnosisThroughTheTool(t *testing.T) {
	_, session := newCapSession(t)

	for _, c := range []string{
		"helmfile deploys through sops secrets",
		"sops age key lives in ci",
		"helmfile environments are global dev prod",
	} {
		callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project",
			"content":    c,
			"category":   "fact",
		})
	}

	res := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "helmfile sops",
		"limit":      3,
		"explain":    true,
	})
	if res.IsError {
		t.Fatalf("explain search errored: %s", resultText(res))
	}

	raw := resultText(res)
	var ex memory.SearchExplain
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		t.Fatalf("response is not a JSON explanation: %v\n%s", err, raw)
	}
	if ex.Query != "helmfile sops" {
		t.Errorf("Query = %q, want the query that was explained", ex.Query)
	}
	if len(ex.Rows) == 0 {
		t.Fatal("no rows in the explanation; nothing was diagnosed")
	}

	// The fields that let an agent attribute blame must be present and
	// meaningful, not silently zero.
	for _, r := range ex.Rows {
		if r.FTSRank < -1 {
			t.Errorf("row %s FTSRank = %d", r.ID, r.FTSRank)
		}
		// A row that matched at least one leg must carry a positive fused
		// score; zero would mean the RRF arithmetic never ran for it.
		if (r.FTSRank >= 0 || r.VectorRank >= 0) && r.RRFScore <= 0 {
			t.Errorf("row %s matched a leg (fts=%d vec=%d) but has fused score %v", r.ID, r.FTSRank, r.VectorRank, r.RRFScore)
		}
		if !r.Included && r.Reason == "" {
			t.Errorf("row %s excluded without a reason", r.ID)
		}
	}
	if ex.VectorAvailable && !strings.Contains(raw, "vector_rank") {
		t.Error("vector_rank missing from the payload")
	}

	// The non-explain path must still return the ordinary formatted list:
	// explain is an opt-in alternate rendering, not a mode change.
	plain := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "helmfile sops",
		"limit":      3,
	})
	plainText := resultText(plain)
	if strings.Contains(plainText, `"fts_rank"`) {
		t.Errorf("plain search returned an explanation:\n%s", plainText)
	}
	if !strings.Contains(plainText, "Memory") && !strings.Contains(plainText, "fact") {
		t.Errorf("plain search returned an unexpected rendering:\n%s", plainText)
	}
}
