package portable

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// pointerRun is one import's outcome, collected for comparing a dry run with the
// apply it previews.
type pointerRun struct {
	actions map[string]Action
	dropped map[string]string
	report  ImportReport
	store   *memory.Store
}

func runPointerImport(t *testing.T, artifact string, apply bool) pointerRun {
	t.Helper()
	s := newTestStore(t)
	run := pointerRun{actions: map[string]Action{}, dropped: map[string]string{}, store: s}
	report, err := Import(context.Background(), s, strings.NewReader(artifact), ImportOptions{Apply: apply}, func(r RecordResult) {
		key := fmt.Sprintf("%s/%s", r.Type, r.ID)
		run.actions[key] = r.Action
		if r.DroppedRef != "" {
			run.dropped[key] = r.DroppedRef
		}
	})
	if err != nil {
		t.Fatalf("Import(apply=%v): %v", apply, err)
	}
	run.report = report
	return run
}

// assertSameOutcome holds a dry run to the apply it previews: the same action
// for every record, the same drops, and the same counts.
func assertSameOutcome(t *testing.T, dry, applied pointerRun) {
	t.Helper()
	if fmt.Sprint(dry.actions) != fmt.Sprint(applied.actions) {
		t.Errorf("actions differ:\n dry run %v\n apply   %v", dry.actions, applied.actions)
	}
	if fmt.Sprint(dry.dropped) != fmt.Sprint(applied.dropped) {
		t.Errorf("dropped pointers differ:\n dry run %v\n apply   %v", dry.dropped, applied.dropped)
	}
	if fmt.Sprint(dry.report.Created) != fmt.Sprint(applied.report.Created) ||
		fmt.Sprint(dry.report.Skipped) != fmt.Sprint(applied.report.Skipped) ||
		dry.report.Rejected != applied.report.Rejected {
		t.Errorf("counts differ:\n dry run created %v skipped %v rejected %d\n apply   created %v skipped %v rejected %d",
			dry.report.Created, dry.report.Skipped, dry.report.Rejected,
			applied.report.Created, applied.report.Skipped, applied.report.Rejected)
	}
}

// TestATaskWhoseBlockerIsRejectedStillImports: a blocked_by pointer naming a
// task the artifact contains but rejects is a pointer that can never resolve,
// exactly like one naming a task the artifact does not contain. It is dropped
// and the dependent task imports — in the dry run and the apply alike, so the
// preview describes the run that follows.
//
// The blocker is rejected two ways (an invalid priority, and a project neither
// the store nor the artifact holds, which is refused before the store method is
// reached), and a third task keeps a valid blocker, so a fix that dropped every
// pointer would fail too.
func TestATaskWhoseBlockerIsRejectedStillImports(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"bad priority","status":"pending","priority":9}}` + "\n" +
		`{"type":"task","task":{"id":"tb2","project_id":"pb","title":"blocked by a rejected task","status":"pending","priority":2,"blocked_by":"tb1"}}` + "\n" +
		`{"type":"task","task":{"id":"tb3","project_id":"nowhere","title":"orphan","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb4","project_id":"pb","title":"blocked by an orphan","status":"pending","priority":2,"blocked_by":"tb3"}}` + "\n" +
		`{"type":"task","task":{"id":"tb5","project_id":"pb","title":"good blocker","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb6","project_id":"pb","title":"blocked by a good task","status":"pending","priority":2,"blocked_by":"tb5"}}` + "\n"

	dry := runPointerImport(t, artifact, false)
	applied := runPointerImport(t, artifact, true)
	assertSameOutcome(t, dry, applied)

	for key, want := range map[string]Action{
		"task/tb1": ActionReject,
		"task/tb2": ActionCreate,
		"task/tb3": ActionReject,
		"task/tb4": ActionCreate,
		"task/tb5": ActionCreate,
		"task/tb6": ActionCreate,
	} {
		for mode, run := range map[string]pointerRun{"dry run": dry, "apply": applied} {
			if got := run.actions[key]; got != want {
				t.Errorf("%s: %s was %q, want %q", mode, key, got, want)
			}
		}
	}
	for key, want := range map[string]string{"task/tb2": "tb1", "task/tb4": "tb3"} {
		if got := applied.dropped[key]; got != want {
			t.Errorf("%s reported dropped pointer %q, want %q", key, got, want)
		}
	}
	if got, ok := applied.dropped["task/tb6"]; ok {
		t.Errorf("task/tb6 reported dropped pointer %q, but its blocker imported", got)
	}

	tasks, err := selectTasks(context.Background(), applied.store, []string{"pb"})
	if err != nil {
		t.Fatalf("selectTasks: %v", err)
	}
	blockedBy := map[string]string{}
	for _, task := range tasks {
		blockedBy[task.ID] = task.BlockedBy
	}
	for id, want := range map[string]string{"tb2": "", "tb4": "", "tb6": "tb5"} {
		got, ok := blockedBy[id]
		if !ok {
			t.Errorf("task %s is not in the store after the apply", id)
			continue
		}
		if got != want {
			t.Errorf("task %s blocked_by = %q, want %q", id, got, want)
		}
	}
}

// TestADecisionWhoseSupersederIsRejectedStillImports is the superseded_by half
// of the same rule.
func TestADecisionWhoseSupersederIsRejectedStillImports(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc1","project_id":"pb","title":"kept","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc2"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc2","project_id":"pb","title":"no rationale","decision":"d","rationale":"","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc3","project_id":"pb","title":"kept too","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc4"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc4","project_id":"pb","title":"good superseder","decision":"d","rationale":"r","status":"active"}}` + "\n"

	dry := runPointerImport(t, artifact, false)
	applied := runPointerImport(t, artifact, true)
	assertSameOutcome(t, dry, applied)

	for key, want := range map[string]Action{
		"decision/dc1": ActionCreate,
		"decision/dc2": ActionReject,
		"decision/dc3": ActionCreate,
		"decision/dc4": ActionCreate,
	} {
		for mode, run := range map[string]pointerRun{"dry run": dry, "apply": applied} {
			if got := run.actions[key]; got != want {
				t.Errorf("%s: %s was %q, want %q", mode, key, got, want)
			}
		}
	}
	if got := applied.dropped["decision/dc1"]; got != "dc2" {
		t.Errorf("decision/dc1 reported dropped pointer %q, want %q", got, "dc2")
	}
	if got, ok := applied.dropped["decision/dc3"]; ok {
		t.Errorf("decision/dc3 reported dropped pointer %q, but its superseder imported", got)
	}

	decisions, err := selectDecisions(context.Background(), applied.store, []string{"pb"})
	if err != nil {
		t.Fatalf("selectDecisions: %v", err)
	}
	supersededBy := map[string]string{}
	for _, d := range decisions {
		supersededBy[d.ID] = d.SupersededBy
	}
	for id, want := range map[string]string{"dc1": "", "dc3": "dc4"} {
		got, ok := supersededBy[id]
		if !ok {
			t.Errorf("decision %s is not in the store after the apply", id)
			continue
		}
		if got != want {
			t.Errorf("decision %s superseded_by = %q, want %q", id, got, want)
		}
	}
}
