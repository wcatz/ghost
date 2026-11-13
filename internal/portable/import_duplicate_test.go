package portable

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// #1096. A dry run and the apply it previews must agree about an artifact that
// repeats an id.
func TestADryRunAndItsApplyAgreeWhenAnArtifactRepeatsAnID(t *testing.T) {
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "p1", "path": "/src/p1", "name": "one"}}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "dupP", "path": "/src/dupP", "name": "dup"}}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "dupP", "path": "/src/dupP", "name": "dup again"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup1", "project_id": "p1", "category": "gotcha", "content": "first", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup1", "project_id": "p1", "category": "gotcha", "content": "second", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup2", "project_id": "p1", "category": "bogus", "content": "refused", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup2", "project_id": "p1", "category": "gotcha", "content": "accepted", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup3", "project_id": "p1", "category": "gotcha", "content": "first", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup3", "project_id": "nope", "category": "gotcha", "content": "second", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup4", "project_id": "p1", "category": "gotcha", "content": "first", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "dup4", "project_id": "p1", "category": "bogus", "content": "second", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{"id": "dupT", "project_id": "p1", "title": "first", "status": "pending", "priority": 1}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{"id": "dupT", "project_id": "p1", "title": "second", "status": "pending", "priority": 1}}),
		jsonLine(t, map[string]any{"type": "decision", "decision": map[string]any{"id": "dupD", "project_id": "p1", "title": "first", "decision": "do it", "rationale": "why", "status": "active"}}),
		jsonLine(t, map[string]any{"type": "decision", "decision": map[string]any{"id": "dupD", "project_id": "p1", "title": "second", "decision": "do it", "rationale": "why", "status": "active"}}),
	}
	artifact := strings.Join(lines, "\n") + "\n"
	ctx := context.Background()
	dry := newTestStore(t)
	var drySeen []RecordResult
	dryReport, err := Import(ctx, dry, strings.NewReader(artifact), ImportOptions{}, func(r RecordResult) { drySeen = append(drySeen, r) })
	if err != nil {
		t.Fatalf("dry: %v", err)
	}
	applyDB, apply := newTestStoreWithDB(t)
	var applySeen []RecordResult
	if _, err := Import(ctx, apply, strings.NewReader(artifact), ImportOptions{Apply: true}, func(r RecordResult) { applySeen = append(applySeen, r) }); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(drySeen) != len(applySeen) {
		t.Fatalf("len dry=%d apply=%d", len(drySeen), len(applySeen))
	}
	for i := range drySeen {
		d, a := drySeen[i], applySeen[i]
		if d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action || !slices.Equal(d.Clamped, a.Clamped) || d.Downgraded != a.Downgraded || d.DroppedRef != a.DroppedRef || d.DropReason != a.DropReason {
			t.Fatalf("i=%d dry=%+v apply=%+v", i, d, a)
		}
	}
	want := map[string][]Action{
		"dupP": {ActionCreate, ActionSkip}, "dup1": {ActionCreate, ActionSkip}, "dup2": {ActionReject, ActionCreate},
		"dup3": {ActionCreate, ActionReject}, "dup4": {ActionCreate, ActionSkip}, "dupT": {ActionCreate, ActionSkip}, "dupD": {ActionCreate, ActionSkip},
	}
	for id, wantActions := range want {
		var got []Action
		for _, r := range drySeen {
			if r.ID == id {
				got = append(got, r.Action)
			}
		}
		if len(got) != len(wantActions) || !slices.Equal(got, wantActions) {
			t.Fatalf("%s got=%v want=%v", id, got, wantActions)
		}
	}
	for kind, query := range map[string]string{
		"memory":   "SELECT id, count(*) FROM memories WHERE id IN ('dup1','dup2','dup3','dup4') GROUP BY id",
		"task":     "SELECT id, count(*) FROM tasks WHERE id IN ('dupT') GROUP BY id",
		"decision": "SELECT id, count(*) FROM decisions WHERE id IN ('dupD') GROUP BY id",
	} {
		rows, err := applyDB.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for rows.Next() {
			var id string
			var n int
			_ = rows.Scan(&id, &n)
			if n != 1 {
				t.Fatalf("%s %s count=%d", kind, id, n)
			}
		}
		_ = rows.Err()
		_ = rows.Close()
	}
	if dryReport.Created["memory"] != 4 || dryReport.Skipped["memory"] != 2 || dryReport.Rejected != 2 ||
		dryReport.Created["project"] != 2 || dryReport.Skipped["project"] != 1 ||
		dryReport.Created["task"] != 1 || dryReport.Skipped["task"] != 1 ||
		dryReport.Created["decision"] != 1 || dryReport.Skipped["decision"] != 1 {
		t.Fatalf("report=%+v", dryReport)
	}
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
