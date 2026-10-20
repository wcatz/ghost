package reflection

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/adversarial"
	"github.com/wcatz/ghost/internal/memory"
)

// TestSharedTokenShapedStoredIDsAreWithheldByTheGroundingRefusal runs the shared
// token-shaped set through the reflection grounding WARN, whose ids are stored
// ids and whose line lands in lifecycle.log on the detached path (#1641).
func TestSharedTokenShapedStoredIDsAreWithheldByTheGroundingRefusal(t *testing.T) {
	for _, p := range adversarial.TokenShaped() {
		t.Run(p.Name, func(t *testing.T) {
			in := ReflectionInput{
				ProjectName: "ghost",
				ExistingMemories: []memory.Memory{
					opMem(p.Text, "fact", "The mesh is served by 2.BeXIAhbj.js", 0.5),
					opMem(opID2, "fact", "Production runs in region fsn1 behind Cloudflare", 0.5),
				},
			}
			var buf bytes.Buffer
			llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":["merge ` + p.Text + `,` + opID2 + ` -> the bastion on port 2222 fronts the mesh bundle 2.BeXIAhbq.js"]}`})
			llm.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
			if _, err := llm.Consolidate(context.Background(), in); err != nil {
				t.Fatalf("Consolidate: %v", err)
			}
			logged := buf.String()
			if !strings.Contains(logged, "introduced identifiers absent from its sources") {
				t.Fatalf("the grounding refusal was not logged:\n%s", logged)
			}
			if strings.Contains(logged, p.Text) {
				t.Errorf("the grounding WARN echoed the stored id:\n%s", logged)
			}
			if !strings.Contains(logged, "withheld") {
				t.Errorf("the grounding WARN did not name the format the guard found:\n%s", logged)
			}
		})
	}
}
