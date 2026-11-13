package reflection

import (
	"context"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestRewriteKeepsTheSourceWhenAFilterRemovesTheReplacement drives the whole
// tier. The rewrite passes the grounding check and the operations are resolved —
// and then dropForeignProjectMemories runs and removes the rewritten text, because
// it names a project the input corpus never mentioned. The source row is kept:
// there is nothing left in the corpus that explains where its knowledge went, and
// an unattended reflect never deletes a memory on the model's say-so alone
// (#549). Demoting it later is resolve and supersede's job.
func TestRewriteKeepsTheSourceWhenAFilterRemovesTheReplacement(t *testing.T) {
	const (
		source = "the metrics endpoint is served by the ingest service"
		reword = "the metrics endpoint is served by the dingo sidecar"
	)
	in := ReflectionInput{
		ProjectName:       "ghost",
		OtherProjectNames: []string{"dingo"},
		ExistingMemories: []memory.Memory{
			{ID: seqID(1), Category: "fact", Content: source},
			{ID: seqID(2), Category: "fact", Content: "region fsn1 fronts every ingest worker"},
		},
	}
	llm := NewLlmConsolidator(&fakeReflector{reply: fmt.Sprintf(
		`{"ops":["rewrite %s -> %s","keep %s"]}`, seqID(1), reword, seqID(2))})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if _, ok := findMemory(result, reword); ok {
		t.Fatalf("the contaminating rewrite survived the filter: %+v", result.Memories)
	}
	if len(result.Memories) != 1 || result.Memories[0].Content != in.ExistingMemories[1].Content {
		t.Fatalf("want only the passed-through row left: %+v", result.Memories)
	}

	// The source has to come back, and it has to come back through the guard, since
	// the tier no longer emits anything for an id it resolved.
	drops := AuditGuardedDrops(in, result)
	if len(drops) != 1 || drops[0].Content != source {
		t.Fatalf("a rewrite outlived its replacement and the source row went with it: %+v", drops)
	}
}

func auditContains(drops []DroppedGuarded, content string) bool {
	for _, d := range drops {
		if d.Content == content {
			return true
		}
	}
	return false
}
