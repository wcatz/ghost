package portable

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// #1096. A dry run and the apply it previews must agree about an artifact that
// repeats an id, because docs/cli.md promises the preview runs the same
// validation as the run it describes rather than a second code path that can
// disagree with it.
//
// The store's create-or-skip answer comes from its presence check, and that
// check reads the database. An apply run has committed the first record by the
// time the second is checked, so the second is a skip; a dry run has written
// nothing, so the second reads as absent and the preview reports a create the
// apply will not make. The plan has to keep the other half of that answer
// itself: an id an earlier step created is present for every step after it.
func TestADryRunAndItsApplyAgreeWhenAnArtifactRepeatsAnID(t *testing.T) {
	// Every kind repeats an id, and the repeats are not interchangeable:
	//   dup1   — both records valid, so the first creates and the second skips
	//   dup2   — the first is refused, so nothing was written under the id and
	//            the second is judged on its own record rather than skipped
	//   dup3   — the second names a project neither the artifact nor the store
	//            has; the importer's project check runs ahead of the store in
	//            both modes, so the second is refused in both
	//   dup4   — the second is invalid on its own; the store's presence check
	//            runs ahead of its record checks, so the apply run skips it and
	//            the dry run must too rather than refuse it
	//   dupP   — a project id, repeated like any other
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
	dryReport, err := Import(ctx, dry, strings.NewReader(artifact), ImportOptions{}, func(r RecordResult) {
		drySeen = append(drySeen, r)
	})
	if err != nil {
		t.Fatalf("Import (dry run): %v", err)
	}

	applyDB, apply := newTestStoreWithDB(t)
	var applySeen []RecordResult
	if _, err := Import(ctx, apply, strings.NewReader(artifact), ImportOptions{Apply: true}, func(r RecordResult) {
		applySeen = append(applySeen, r)
	}); err != nil {
		t.Fatalf("Import (apply): %v", err)
	}

	// The preview and the run it previews must be the same list of verdicts,
	// record for record. That is the property the command exists to provide, and
	// it is asserted as a whole so a future disagreement anywhere in the plan
	// fails here rather than in whichever report line a reader happens to check.
	if len(drySeen) != len(applySeen) {
		t.Fatalf("the dry run reported %d records and the apply run %d", len(drySeen), len(applySeen))
	}
	for i := range drySeen {
		d, a := drySeen[i], applySeen[i]
		if d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action ||
			!slices.Equal(d.Clamped, a.Clamped) || d.Downgraded != a.Downgraded || d.DroppedRef != a.DroppedRef ||
			d.DropReason != a.DropReason {
			t.Errorf("record %d disagrees: dry %s %s line %d = %s (clamped %v, downgraded %v, dropped %q, reason %d), apply %s %s line %d = %s (clamped %v, downgraded %v, dropped %q, reason %d)",
				i, d.Type, d.ID, d.Line, d.Action, d.Clamped, d.Downgraded, d.DroppedRef, d.DropReason,
				a.Type, a.ID, a.Line, a.Action, a.Clamped, a.Downgraded, a.DroppedRef, a.DropReason)
		}
	}

	// And the shape of the answer, stated per repeated id rather than only as a
	// whole.
	want := map[string][]Action{
		"dupP": {ActionCreate, ActionSkip},
		"dup1": {ActionCreate, ActionSkip},
		"dup2": {ActionReject, ActionCreate},
		"dup3": {ActionCreate, ActionReject},
		"dup4": {ActionCreate, ActionSkip},
		"dupT": {ActionCreate, ActionSkip},
		"dupD": {ActionCreate, ActionSkip},
	}
	for id, wantActions := range want {
		var got []Action
		for _, r := range drySeen {
			if r.ID == id {
				got = append(got, r.Action)
			}
		}
		if len(got) != len(wantActions) {
			t.Fatalf("%s: the dry run reported %d records, want %d", id, len(got), len(wantActions))
		}
		for i := range got {
			if got[i] != wantActions[i] {
				t.Errorf("%s: the dry run's record %d is %s, want %s (all: %v)", id, i, got[i], wantActions[i], got)
			}
		}
	}

	// The apply run wrote exactly one row per repeated id, which is what the
	// skip it reported means on a store.
	for kind, query := range map[string]string{
		"memory":   "SELECT id, count(*) FROM memories WHERE id IN ('dup1','dup2','dup3','dup4') GROUP BY id",
		"task":     "SELECT id, count(*) FROM tasks WHERE id IN ('dupT') GROUP BY id",
		"decision": "SELECT id, count(*) FROM decisions WHERE id IN ('dupD') GROUP BY id",
	} {
		rows, err := applyDB.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("count %s: %v", kind, err)
		}
		defer rows.Close() //nolint:errcheck
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatalf("scan %s: %v", kind, err)
			}
			if n != 1 {
				t.Errorf("the store holds %d %s rows under id %s, want 1", n, kind, id)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %s: %v", kind, err)
		}
	}

	// The report's own counts, which is what the summary line is built from.
	if dryReport.Created["memory"] != 4 || dryReport.Skipped["memory"] != 2 || dryReport.Rejected != 2 {
		t.Errorf("the dry run's counts = created %v, skipped %v, rejected %d; want 4 created, 2 skipped, 2 rejected",
			dryReport.Created, dryReport.Skipped, dryReport.Rejected)
	}
	// Three project records: p1, dupP, dupP — so two create and one skips.
	if dryReport.Created["project"] != 2 || dryReport.Skipped["project"] != 1 {
		t.Errorf("the dry run's project counts = created %d, skipped %d; want 2 and 1", dryReport.Created["project"], dryReport.Skipped["project"])
	}
	if dryReport.Created["task"] != 1 || dryReport.Skipped["task"] != 1 {
		t.Errorf("the dry run's task counts = created %d, skipped %d; want 1 and 1", dryReport.Created["task"], dryReport.Skipped["task"])
	}
	if dryReport.Created["decision"] != 1 || dryReport.Skipped["decision"] != 1 {
		t.Errorf("the dry run's decision counts = created %d, skipped %d; want 1 and 1", dryReport.Created["decision"], dryReport.Skipped["decision"])
	}
}

// jsonLine marshals one artifact record, for building a fixture by hand.
func jsonLine(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	return string(raw)
}
