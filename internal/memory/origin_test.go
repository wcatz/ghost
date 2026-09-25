package memory

import "testing"

// TestOriginClassCoversEverySchemaSource keeps provenance interpretation in
// one place. The session banner and MCP listing both consume memories.source;
// a new legal source must not silently acquire a different trust rule in one
// surface than in the other.
func TestOriginClassCoversEverySchemaSource(t *testing.T) {
	cases := []struct {
		source    string
		wantOwn   bool
		wantLabel string
	}{
		{source: "manual", wantOwn: true, wantLabel: ""},
		{source: "reflection", wantOwn: false, wantLabel: "reflection"},
		{source: "chat", wantOwn: false, wantLabel: "chat"},
		{source: "tool", wantOwn: false, wantLabel: "tool"},
		{source: "mcp", wantOwn: false, wantLabel: "mcp"},
		{source: "onboarding", wantOwn: false, wantLabel: "onboarding"},
		{source: "decision_log", wantOwn: false, wantLabel: "decision_log"},
	}

	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			own, label := OriginClass(tc.source)
			if own != tc.wantOwn || label != tc.wantLabel {
				t.Errorf("OriginClass(%q) = (%v, %q), want (%v, %q)", tc.source, own, label, tc.wantOwn, tc.wantLabel)
			}
		})
	}
}
