package portable

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A memory deleted here leaves its history behind, and the store refuses an
// import into that id. The importer has to report the refusal in a dry run as
// well as in the apply, or the preview is clean and the run fails.
func TestADryRunNamesTheRejectionOfAnIDThatStillHasHistory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", ""); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	id, err := s.Create(ctx, "p1", memory.Memory{Category: "gotcha", Content: "deleted here", Source: "manual", Importance: 0.4})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	artifact := strings.Join([]string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": id, "project_id": "p1", "category": "gotcha", "content": "an unrelated fact", "source": "mcp"}}),
	}, "\n") + "\n"

	for _, apply := range []bool{false, true} {
		var seen []RecordResult
		report, err := Import(ctx, s, strings.NewReader(artifact), ImportOptions{Apply: apply}, func(r RecordResult) {
			seen = append(seen, r)
		})
		if err != nil {
			t.Fatalf("apply=%v: Import: %v", apply, err)
		}
		if len(seen) != 1 || seen[0].Action != ActionReject {
			t.Fatalf("apply=%v: results = %+v, want one rejection", apply, seen)
		}
		if seen[0].Error == nil || !strings.Contains(seen[0].Error.Error(), "ghost history purge") {
			t.Errorf("apply=%v: the rejection = %v, want it to name the purge", apply, seen[0].Error)
		}
		if report.Rejected != 1 || len(report.Errors) != 1 || report.Created["memory"] != 0 {
			t.Errorf("apply=%v: report = rejected %d, errors %d, created %v; want one rejection and no create",
				apply, report.Rejected, len(report.Errors), report.Created)
		}
	}
}
