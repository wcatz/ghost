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
	reasons map[string]DropReason
	report  ImportReport
	store   *memory.Store
}

func runPointerImport(t *testing.T, artifact string, apply bool) pointerRun {
	t.Helper()
	return runPointerImportOnto(t, "", artifact, apply)
}

// runPointerImportOnto is runPointerImport onto a store that already holds what
// seed, an artifact applied first, puts there.
func runPointerImportOnto(t *testing.T, seed, artifact string, apply bool) pointerRun {
	t.Helper()
	s := newTestStore(t)
	if seed != "" {
		if _, err := Import(context.Background(), s, strings.NewReader(seed), ImportOptions{Apply: true}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	run := pointerRun{actions: map[string]Action{}, dropped: map[string]string{}, reasons: map[string]DropReason{}, store: s}
	report, err := Import(context.Background(), s, strings.NewReader(artifact), ImportOptions{Apply: apply}, func(r RecordResult) {
		key := fmt.Sprintf("%s/%s", r.Type, r.ID)
		run.actions[key] = r.Action
		if r.DroppedRef != "" {
			run.dropped[key] = r.DroppedRef
			run.reasons[key] = r.DropReason
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
	if fmt.Sprint(dry.reasons) != fmt.Sprint(applied.reasons) {
		t.Errorf("drop reasons differ:\n dry run %v\n apply   %v", dry.reasons, applied.reasons)
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

// TestAPointerToARecordAlreadyInTheStoreIsKept: a target the store already
// holds is skipped by the import, and a skip has landed — the row is there to
// point at. The dependent record is created with its pointer intact in both
// modes, whether the artifact lists it before its target or after.
func TestAPointerToARecordAlreadyInTheStoreIsKept(t *testing.T) {
	const header = `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n"
	const target = `{"type":"task","task":{"id":"tt1","project_id":"pb","title":"target","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"dt1","project_id":"pb","title":"superseder","decision":"d","rationale":"r","status":"active"}}` + "\n"
	const taskDependent = `{"type":"task","task":{"id":"tt2","project_id":"pb","title":"dependent","status":"pending","priority":2,"blocked_by":"tt1"}}` + "\n"
	const decisionDependent = `{"type":"decision","decision":{"id":"dt2","project_id":"pb","title":"superseded","decision":"d","rationale":"r","status":"superseded","superseded_by":"dt1"}}` + "\n"

	for name, artifact := range map[string]string{
		"dependent listed after its target":  header + target + taskDependent + decisionDependent,
		"dependent listed before its target": header + taskDependent + decisionDependent + target,
	} {
		t.Run(name, func(t *testing.T) {
			runs := map[string]pointerRun{}
			for mode, apply := range map[string]bool{"dry run": false, "apply": true} {
				s := newTestStore(t)
				if _, err := Import(context.Background(), s, strings.NewReader(header+target), ImportOptions{Apply: true}, nil); err != nil {
					t.Fatalf("seed: %v", err)
				}
				run := pointerRun{actions: map[string]Action{}, dropped: map[string]string{}, store: s}
				report, err := Import(context.Background(), s, strings.NewReader(artifact), ImportOptions{Apply: apply}, func(r RecordResult) {
					key := fmt.Sprintf("%s/%s", r.Type, r.ID)
					run.actions[key] = r.Action
					if r.DroppedRef != "" {
						run.dropped[key] = r.DroppedRef
					}
				})
				if err != nil {
					t.Fatalf("Import(%s): %v", mode, err)
				}
				run.report = report
				runs[mode] = run
			}
			assertSameOutcome(t, runs["dry run"], runs["apply"])

			for mode, run := range runs {
				for key, want := range map[string]Action{
					"task/tt1":     ActionSkip,
					"task/tt2":     ActionCreate,
					"decision/dt1": ActionSkip,
					"decision/dt2": ActionCreate,
				} {
					if got := run.actions[key]; got != want {
						t.Errorf("%s: %s was %q, want %q", mode, key, got, want)
					}
				}
				if len(run.dropped) != 0 {
					t.Errorf("%s: dropped pointers %v, want none — every target is already in the store", mode, run.dropped)
				}
			}

			s := runs["apply"].store
			tasks, err := selectTasks(context.Background(), s, []string{"pb"})
			if err != nil {
				t.Fatalf("selectTasks: %v", err)
			}
			found := false
			for _, task := range tasks {
				if task.ID == "tt2" {
					found = true
					if task.BlockedBy != "tt1" {
						t.Errorf("task tt2 blocked_by = %q, want %q", task.BlockedBy, "tt1")
					}
				}
			}
			if !found {
				t.Errorf("task tt2 is not in the store after the apply")
			}
			decisions, err := selectDecisions(context.Background(), s, []string{"pb"})
			if err != nil {
				t.Fatalf("selectDecisions: %v", err)
			}
			found = false
			for _, d := range decisions {
				if d.ID == "dt2" {
					found = true
					if d.SupersededBy != "dt1" {
						t.Errorf("decision dt2 superseded_by = %q, want %q", d.SupersededBy, "dt1")
					}
				}
			}
			if !found {
				t.Errorf("decision dt2 is not in the store after the apply")
			}
		})
	}
}

// pointersAfter reads back, from the store an apply ran against, the blocked_by
// of every task and the superseded_by of every decision in project pb.
func pointersAfter(t *testing.T, s *memory.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	tasks, err := selectTasks(context.Background(), s, []string{"pb"})
	if err != nil {
		t.Fatalf("selectTasks: %v", err)
	}
	for _, task := range tasks {
		out["task/"+task.ID] = task.BlockedBy
	}
	decisions, err := selectDecisions(context.Background(), s, []string{"pb"})
	if err != nil {
		t.Fatalf("selectDecisions: %v", err)
	}
	for _, d := range decisions {
		out["decision/"+d.ID] = d.SupersededBy
	}
	return out
}

// assertPointerOutcome checks, in both modes, every record's action and the
// drop each one reports (a record absent from wantDrops must report none), and
// then the pointers the apply left in the store.
func assertPointerOutcome(t *testing.T, dry, applied pointerRun, wantActions map[string]Action,
	wantDrops map[string]string, wantReasons map[string]DropReason, wantStored map[string]string) {
	t.Helper()
	assertSameOutcome(t, dry, applied)
	for mode, run := range map[string]pointerRun{"dry run": dry, "apply": applied} {
		for key, want := range wantActions {
			if got := run.actions[key]; got != want {
				t.Errorf("%s: %s was %q, want %q", mode, key, got, want)
			}
			if got, want := run.dropped[key], wantDrops[key]; got != want {
				t.Errorf("%s: %s reported dropped pointer %q, want %q", mode, key, got, want)
			}
			if want, ok := wantReasons[key]; ok {
				if got := run.reasons[key]; got != want {
					t.Errorf("%s: %s drop reason = %d, want %d", mode, key, got, want)
				}
			}
		}
	}
	stored := pointersAfter(t, applied.store)
	for key, want := range wantStored {
		got, ok := stored[key]
		if !ok {
			t.Errorf("%s is not in the store after the apply", key)
			continue
		}
		if got != want {
			t.Errorf("%s pointer = %q in the store, want %q", key, got, want)
		}
	}
}

// TestAPointerToARecordNowhereIsReported: a blocked_by or superseded_by naming
// a record that is neither in the artifact nor in the destination store is
// dropped — it can never resolve — and the dependent record's report line names
// the drop and says why, in the dry run and the apply alike (#1153). Before, the
// pointer was cleared while ordering and nothing in the report said a link from
// the source machine was lost.
func TestAPointerToARecordNowhereIsReported(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pb","title":"blocked by nothing here","status":"pending","priority":2,"blocked_by":"tx9"}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pb","title":"superseded by nothing here","decision":"d","rationale":"r","status":"superseded","superseded_by":"dx9"}}` + "\n"

	assertPointerOutcome(t, runPointerImport(t, artifact, false), runPointerImport(t, artifact, true),
		map[string]Action{"task/ta1": ActionCreate, "decision/da1": ActionCreate},
		map[string]string{"task/ta1": "tx9", "decision/da1": "dx9"},
		map[string]DropReason{"task/ta1": DropAbsent, "decision/da1": DropAbsent},
		map[string]string{"task/ta1": "", "decision/da1": ""})
}

// TestAPointerInACycleIsReported: pointers that form a cycle have no order of
// writes that satisfies them, so each is dropped and its record's report line
// says so (#1153). A record that only points INTO a cycle is not part of it: its
// target is written, so its pointer is kept and nothing is reported for it.
func TestAPointerInACycleIsReported(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tc3","project_id":"pb","title":"points into the cycle","status":"pending","priority":2,"blocked_by":"tc1"}}` + "\n" +
		`{"type":"task","task":{"id":"tc1","project_id":"pb","title":"cycle one","status":"pending","priority":2,"blocked_by":"tc2"}}` + "\n" +
		`{"type":"task","task":{"id":"tc2","project_id":"pb","title":"cycle two","status":"pending","priority":2,"blocked_by":"tc1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc1","project_id":"pb","title":"cycle one","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc2"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc2","project_id":"pb","title":"cycle two","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc1"}}` + "\n"

	assertPointerOutcome(t, runPointerImport(t, artifact, false), runPointerImport(t, artifact, true),
		map[string]Action{
			"task/tc1": ActionCreate, "task/tc2": ActionCreate, "task/tc3": ActionCreate,
			"decision/dc1": ActionCreate, "decision/dc2": ActionCreate,
		},
		map[string]string{"task/tc1": "tc2", "task/tc2": "tc1", "decision/dc1": "dc2", "decision/dc2": "dc1"},
		map[string]DropReason{"task/tc1": DropCycle, "task/tc2": DropCycle, "decision/dc1": DropCycle, "decision/dc2": DropCycle},
		map[string]string{"task/tc1": "", "task/tc2": "", "task/tc3": "tc1", "decision/dc1": "", "decision/dc2": ""})
}

// TestAPointerToARecordOnlyInTheStoreIsKept: a pointer naming a record the
// artifact does not carry but the destination store already holds resolves, so
// it is kept and nothing is reported — in a dry run as in the apply (#1153). It
// is the case that separates "not in this artifact" from "can never resolve".
//
// The same holds inside a cycle whose other member the store already has: that
// member is skipped, the store's row is there to point at, and the new record
// keeps its pointer.
func TestAPointerToARecordOnlyInTheStoreIsKept(t *testing.T) {
	const project = `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n"
	seed := project +
		`{"type":"task","task":{"id":"ts1","project_id":"pb","title":"target","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"ds1","project_id":"pb","title":"superseder","decision":"d","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"task","task":{"id":"tk1","project_id":"pb","title":"cycle member already here","status":"pending","priority":2}}` + "\n"
	artifact := project +
		`{"type":"task","task":{"id":"tk1","project_id":"pb","title":"cycle member already here","status":"pending","priority":2,"blocked_by":"tk2"}}` + "\n" +
		`{"type":"task","task":{"id":"tk2","project_id":"pb","title":"new cycle member","status":"pending","priority":2,"blocked_by":"tk1"}}` + "\n" +
		`{"type":"task","task":{"id":"ts2","project_id":"pb","title":"dependent","status":"pending","priority":2,"blocked_by":"ts1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"ds2","project_id":"pb","title":"superseded","decision":"d","rationale":"r","status":"superseded","superseded_by":"ds1"}}` + "\n"

	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), runPointerImportOnto(t, seed, artifact, true),
		map[string]Action{"task/ts2": ActionCreate, "decision/ds2": ActionCreate, "task/tk1": ActionSkip, "task/tk2": ActionCreate},
		map[string]string{},
		nil,
		map[string]string{"task/ts2": "ts1", "decision/ds2": "ds1", "task/tk1": "", "task/tk2": "tk1"})
}
