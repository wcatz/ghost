package portable

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

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
			run.dropped[key], run.reasons[key] = r.DroppedRef, r.DropReason
		}
	})
	if err != nil {
		t.Fatalf("apply=%v: %v", apply, err)
	}
	run.report = report
	return run
}

func assertSameOutcome(t *testing.T, dry, applied pointerRun) {
	t.Helper()
	if fmt.Sprint(dry.actions) != fmt.Sprint(applied.actions) || fmt.Sprint(dry.dropped) != fmt.Sprint(applied.dropped) || fmt.Sprint(dry.reasons) != fmt.Sprint(applied.reasons) ||
		fmt.Sprint(dry.report.Created) != fmt.Sprint(applied.report.Created) || fmt.Sprint(dry.report.Skipped) != fmt.Sprint(applied.report.Skipped) || dry.report.Rejected != applied.report.Rejected {
		t.Fatalf("dry=%+v applied=%+v", dry, applied)
	}
}

func pointersAfter(t *testing.T, s *memory.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	tasks, _ := selectTasks(context.Background(), s, []string{"pb"})
	for _, task := range tasks {
		out["task/"+task.ID] = task.BlockedBy
	}
	decisions, _ := selectDecisions(context.Background(), s, []string{"pb"})
	for _, d := range decisions {
		out["decision/"+d.ID] = d.SupersededBy
	}
	return out
}

func assertPointerOutcome(t *testing.T, dry, applied pointerRun, wantActions map[string]Action, wantDrops map[string]string, wantReasons map[string]DropReason, wantStored map[string]string) {
	t.Helper()
	assertSameOutcome(t, dry, applied)
	for _, run := range []pointerRun{dry, applied} {
		for key, want := range wantActions {
			if run.actions[key] != want || run.dropped[key] != wantDrops[key] {
				t.Fatalf("key=%s run=%+v", key, run)
			}
			if want, ok := wantReasons[key]; ok && run.reasons[key] != want {
				t.Fatalf("key=%s run=%+v", key, run)
			}
		}
	}
	stored := pointersAfter(t, applied.store)
	for key, want := range wantStored {
		if stored[key] != want {
			t.Fatalf("key=%s stored=%v", key, stored)
		}
	}
}

func projectRows(t *testing.T, s *memory.Store, projectID string) string {
	t.Helper()
	tasks, _ := selectTasks(context.Background(), s, []string{projectID})
	decisions, _ := selectDecisions(context.Background(), s, []string{projectID})
	return fmt.Sprintf("tasks=%d %+v decisions=%d %+v", len(tasks), tasks, len(decisions), decisions)
}

func assertDeleteLeavesProject(t *testing.T, s *memory.Store, gone, kept string) {
	t.Helper()
	before := projectRows(t, s, kept)
	if _, err := s.DeleteProject(context.Background(), gone, true); err != nil {
		t.Fatalf("DeleteProject(%s): %v", gone, err)
	}
	if after := projectRows(t, s, kept); after != before {
		t.Fatalf("before=%s after=%s", before, after)
	}
}

func TestATaskWhoseBlockerIsRejectedStillImports(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"bad priority","status":"pending","priority":9}}` + "\n" +
		`{"type":"task","task":{"id":"tb2","project_id":"pb","title":"blocked by a rejected task","status":"pending","priority":2,"blocked_by":"tb1"}}` + "\n" +
		`{"type":"task","task":{"id":"tb3","project_id":"nowhere","title":"orphan","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb4","project_id":"pb","title":"blocked by an orphan","status":"pending","priority":2,"blocked_by":"tb3"}}` + "\n" +
		`{"type":"task","task":{"id":"tb5","project_id":"pb","title":"good blocker","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb6","project_id":"pb","title":"blocked by a good task","status":"pending","priority":2,"blocked_by":"tb5"}}` + "\n"
	dry, applied := runPointerImport(t, artifact, false), runPointerImport(t, artifact, true)
	assertSameOutcome(t, dry, applied)
	assertPointerOutcome(t, dry, applied,
		map[string]Action{"task/tb1": ActionReject, "task/tb2": ActionCreate, "task/tb3": ActionReject, "task/tb4": ActionCreate, "task/tb5": ActionCreate, "task/tb6": ActionCreate},
		map[string]string{"task/tb2": "tb1", "task/tb4": "tb3"}, nil,
		map[string]string{"task/tb2": "", "task/tb4": "", "task/tb6": "tb5"})
}

func TestADecisionWhoseSupersederIsRejectedStillImports(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc1","project_id":"pb","title":"kept","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc2"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc2","project_id":"pb","title":"no rationale","decision":"d","rationale":"","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc3","project_id":"pb","title":"kept too","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc4"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc4","project_id":"pb","title":"good superseder","decision":"d","rationale":"r","status":"active"}}` + "\n"
	dry, applied := runPointerImport(t, artifact, false), runPointerImport(t, artifact, true)
	assertPointerOutcome(t, dry, applied,
		map[string]Action{"decision/dc1": ActionCreate, "decision/dc2": ActionReject, "decision/dc3": ActionCreate, "decision/dc4": ActionCreate},
		map[string]string{"decision/dc1": "dc2"}, nil,
		map[string]string{"decision/dc1": "", "decision/dc3": "dc4"})
}

func TestAPointerToARecordAlreadyInTheStoreIsKept(t *testing.T) {
	const header = `{"type":"header","schema_version":2}` + "\n" + `{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n"
	const target = `{"type":"task","task":{"id":"tt1","project_id":"pb","title":"target","status":"pending","priority":2}}` + "\n" + `{"type":"decision","decision":{"id":"dt1","project_id":"pb","title":"superseder","decision":"d","rationale":"r","status":"active"}}` + "\n"
	const deps = `{"type":"task","task":{"id":"tt2","project_id":"pb","title":"dependent","status":"pending","priority":2,"blocked_by":"tt1"}}` + "\n" + `{"type":"decision","decision":{"id":"dt2","project_id":"pb","title":"superseded","decision":"d","rationale":"r","status":"superseded","superseded_by":"dt1"}}` + "\n"
	for name, artifact := range map[string]string{"after": header + target + deps, "before": header + deps + target} {
		t.Run(name, func(t *testing.T) {
			dry, applied := runPointerImportOnto(t, header+target, artifact, false), runPointerImportOnto(t, header+target, artifact, true)
			assertPointerOutcome(t, dry, applied,
				map[string]Action{"task/tt1": ActionSkip, "task/tt2": ActionCreate, "decision/dt1": ActionSkip, "decision/dt2": ActionCreate},
				map[string]string{}, nil,
				map[string]string{"task/tt2": "tt1", "decision/dt2": "dt1"})
		})
	}
}

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

func TestAPointerInACycleIsReported(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tc3","project_id":"pb","title":"points into the cycle","status":"pending","priority":2,"blocked_by":"tc1"}}` + "\n" +
		`{"type":"task","task":{"id":"tc1","project_id":"pb","title":"cycle one","status":"pending","priority":2,"blocked_by":"tc2"}}` + "\n" +
		`{"type":"task","task":{"id":"tc2","project_id":"pb","title":"cycle two","status":"pending","priority":2,"blocked_by":"tc1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc1","project_id":"pb","title":"cycle one","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc2"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dc2","project_id":"pb","title":"cycle two","decision":"d","rationale":"r","status":"superseded","superseded_by":"dc1"}}` + "\n"
	assertPointerOutcome(t, runPointerImport(t, artifact, false), runPointerImport(t, artifact, true),
		map[string]Action{"task/tc1": ActionCreate, "task/tc2": ActionCreate, "task/tc3": ActionCreate, "decision/dc1": ActionCreate, "decision/dc2": ActionCreate},
		map[string]string{"task/tc1": "tc2", "task/tc2": "tc1", "decision/dc1": "dc2", "decision/dc2": "dc1"},
		map[string]DropReason{"task/tc1": DropCycle, "task/tc2": DropCycle, "decision/dc1": DropCycle, "decision/dc2": DropCycle},
		map[string]string{"task/tc1": "", "task/tc2": "", "task/tc3": "tc1", "decision/dc1": "", "decision/dc2": ""})
}

func TestAPointerToARecordOnlyInTheStoreIsKept(t *testing.T) {
	const project = `{"type":"header","schema_version":2}` + "\n" + `{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n"
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
		map[string]string{}, nil,
		map[string]string{"task/ts2": "ts1", "decision/ds2": "ds1", "task/tk1": "", "task/tk2": "tk1"})
}

func chainIntoCycle(n int) []struct{ id, blockedBy string } {
	rows := make([]struct{ id, blockedBy string }, n)
	for i := range rows {
		next := i + 1
		if i == n-1 {
			next = n - 2
		}
		rows[i].id, rows[i].blockedBy = fmt.Sprintf("c%d", i), fmt.Sprintf("c%d", next)
	}
	return rows
}

func TestCycleMembersIsLinear(t *testing.T) {
	const n = 1000
	recs := taskRecords(chainIntoCycle(n))
	calls := 0
	idOf := func(r parsedRecord) string { calls++; return r.rec.Task.ID }
	refOf := func(r parsedRecord) string { calls++; return r.rec.Task.BlockedBy }
	members := cycleMembers(recs, idOf, refOf)
	for i, on := range members {
		if want := i >= n-2; on != want {
			t.Fatalf("c%d on=%v want=%v", i, on, want)
		}
	}
	if calls > 4*n {
		t.Fatalf("calls=%d", calls)
	}
}

func TestTopoOrderOfAChainIntoACycleIsBounded(t *testing.T) {
	const n = 1000
	calls := 0
	idOf := func(r parsedRecord) string { calls++; return r.rec.Task.ID }
	refOf := func(r parsedRecord) string { calls++; return r.rec.Task.BlockedBy }
	clearRef := func(r parsedRecord) parsedRecord {
		task := *r.rec.Task
		task.BlockedBy = ""
		r.rec.Task = &task
		return r
	}
	got := topoOrder(taskRecords(chainIntoCycle(n)), idOf, refOf, clearRef)
	if len(got) != n {
		t.Fatalf("len=%d", len(got))
	}
	pos := map[string]int{}
	for i, r := range got {
		pos[r.rec.Task.ID] = i
	}
	for _, r := range got {
		task := r.rec.Task
		cycle := task.ID == fmt.Sprintf("c%d", n-1) || task.ID == fmt.Sprintf("c%d", n-2)
		if cycle != (task.BlockedBy == "") {
			t.Fatalf("task=%+v", task)
		}
		if task.BlockedBy != "" && pos[task.BlockedBy] > pos[task.ID] {
			t.Fatalf("task=%+v pos=%d ref=%d", task, pos[task.ID], pos[task.BlockedBy])
		}
	}
	if limit := 4 * n * n; calls > limit {
		t.Fatalf("calls=%d", calls)
	}
}

func TestCycleMembersMarksOnlyCycles(t *testing.T) {
	rows := []struct{ id, blockedBy string }{
		{id: "a", blockedBy: "b"}, {id: "b", blockedBy: "a"}, {id: "c", blockedBy: "a"}, {id: "d", blockedBy: "c"}, {id: "e", blockedBy: "d"},
		{id: "s", blockedBy: "s"}, {id: "x", blockedBy: "outside"}, {id: "p", blockedBy: "q"}, {id: "q", blockedBy: "r"}, {id: "r", blockedBy: "p"}, {id: "f", blockedBy: "q"}, {id: "g", blockedBy: ""},
	}
	want := map[string]bool{"a": true, "b": true, "s": true, "p": true, "q": true, "r": true}
	recs := taskRecords(rows)
	members := cycleMembers(recs, func(r parsedRecord) string { return r.rec.Task.ID }, func(r parsedRecord) string { return r.rec.Task.BlockedBy })
	for i, r := range rows {
		if members[i] != want[r.id] {
			t.Fatalf("id=%s got=%v want=%v", r.id, members[i], want[r.id])
		}
	}
}

func TestAPointerIntoAnotherProjectIsDropped(t *testing.T) {
	seed := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pa","path":"/src/pa","name":"pa"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pa","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pa","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n"
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"B task","status":"blocked","priority":2,"blocked_by":"ta1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"db1","project_id":"pb","title":"B decision","decision":"use y","rationale":"r","status":"superseded","superseded_by":"da1"}}` + "\n"
	applied := runPointerImportOnto(t, seed, artifact, true)
	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), applied,
		map[string]Action{"task/tb1": ActionCreate, "decision/db1": ActionCreate},
		map[string]string{"task/tb1": "ta1", "decision/db1": "da1"},
		map[string]DropReason{"task/tb1": DropAbsent, "decision/db1": DropAbsent},
		map[string]string{"task/tb1": "", "decision/db1": ""})
	assertDeleteLeavesProject(t, applied.store, "pa", "pb")
}

func TestAPointerIntoTheMappedProjectIsKept(t *testing.T) {
	seed := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"tx1","project_id":"pb","title":"target","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"dx1","project_id":"pb","title":"successor","decision":"use x","rationale":"r","status":"active"}}` + "\n"
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pq","path":"/src/pb","name":"pq"}}` + "\n" +
		`{"type":"task","task":{"id":"tq1","project_id":"pq","title":"dependent","status":"blocked","priority":2,"blocked_by":"tx1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dq1","project_id":"pq","title":"superseded","decision":"use y","rationale":"r","status":"superseded","superseded_by":"dx1"}}` + "\n"
	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), runPointerImportOnto(t, seed, artifact, true),
		map[string]Action{"project/pq": ActionSkip, "task/tq1": ActionCreate, "decision/dq1": ActionCreate},
		map[string]string{}, nil,
		map[string]string{"task/tq1": "tx1", "decision/dq1": "dx1"})
}

func TestAnInArtifactPointerIntoAnotherProjectIsDropped(t *testing.T) {
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pa","path":"/src/pa","name":"pa"}}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pa","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"B task","status":"blocked","priority":2,"blocked_by":"ta1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pa","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"db1","project_id":"pb","title":"B decision","decision":"use y","rationale":"r","status":"superseded","superseded_by":"da1"}}` + "\n"
	applied := runPointerImport(t, artifact, true)
	assertPointerOutcome(t, runPointerImport(t, artifact, false), applied,
		map[string]Action{"task/ta1": ActionCreate, "task/tb1": ActionCreate, "decision/da1": ActionCreate, "decision/db1": ActionCreate},
		map[string]string{"task/tb1": "ta1", "decision/db1": "da1"},
		map[string]DropReason{"task/tb1": DropAbsent, "decision/db1": DropAbsent},
		map[string]string{"task/tb1": "", "decision/db1": ""})
	assertDeleteLeavesProject(t, applied.store, "pa", "pb")
}

func TestAnInArtifactPointerToARecordStoredInAnotherProjectIsDropped(t *testing.T) {
	seed := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pa","path":"/src/pa","name":"pa"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pa","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pa","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n"
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pb","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"B task","status":"blocked","priority":2,"blocked_by":"ta1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pb","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"db1","project_id":"pb","title":"B decision","decision":"use y","rationale":"r","status":"superseded","superseded_by":"da1"}}` + "\n"
	applied := runPointerImportOnto(t, seed, artifact, true)
	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), applied,
		map[string]Action{"task/ta1": ActionSkip, "task/tb1": ActionCreate, "decision/da1": ActionSkip, "decision/db1": ActionCreate},
		map[string]string{"task/tb1": "ta1", "decision/db1": "da1"},
		map[string]DropReason{"task/tb1": DropAbsent, "decision/db1": DropAbsent},
		map[string]string{"task/tb1": "", "decision/db1": ""})
	assertDeleteLeavesProject(t, applied.store, "pa", "pb")
}

func TestAnInArtifactPointerIntoTheMappedProjectIsKept(t *testing.T) {
	seed := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ts1","project_id":"pb","title":"stored target","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"ds1","project_id":"pb","title":"stored successor","decision":"use x","rationale":"r","status":"active"}}` + "\n"
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pq","path":"/src/pb","name":"pq"}}` + "\n" +
		`{"type":"task","task":{"id":"tn1","project_id":"pq","title":"new target","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tn2","project_id":"pq","title":"blocked by the new target","status":"blocked","priority":2,"blocked_by":"tn1"}}` + "\n" +
		`{"type":"task","task":{"id":"ts1","project_id":"pq","title":"stored target","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"ts2","project_id":"pq","title":"blocked by the stored target","status":"blocked","priority":2,"blocked_by":"ts1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dn1","project_id":"pq","title":"new successor","decision":"use x","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"dn2","project_id":"pq","title":"superseded by the new one","decision":"use y","rationale":"r","status":"superseded","superseded_by":"dn1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"ds1","project_id":"pq","title":"stored successor","decision":"use x","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"ds2","project_id":"pq","title":"superseded by the stored one","decision":"use y","rationale":"r","status":"superseded","superseded_by":"ds1"}}` + "\n"
	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), runPointerImportOnto(t, seed, artifact, true),
		map[string]Action{"project/pq": ActionSkip, "task/tn1": ActionCreate, "task/tn2": ActionCreate, "task/ts1": ActionSkip, "task/ts2": ActionCreate, "decision/dn1": ActionCreate, "decision/dn2": ActionCreate, "decision/ds1": ActionSkip, "decision/ds2": ActionCreate},
		map[string]string{}, nil,
		map[string]string{"task/tn2": "tn1", "task/ts2": "ts1", "decision/dn2": "dn1", "decision/ds2": "ds1"})
}

func TestAnInArtifactPointerToARecordStoredInItsOwnProjectIsKept(t *testing.T) {
	seed := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pb","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pb","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n"
	artifact := `{"type":"header","schema_version":2}` + "\n" +
		`{"type":"project","project":{"id":"pa","path":"/src/pa","name":"pa"}}` + "\n" +
		`{"type":"project","project":{"id":"pb","path":"/src/pb","name":"pb"}}` + "\n" +
		`{"type":"task","task":{"id":"ta1","project_id":"pa","title":"A task","status":"pending","priority":2}}` + "\n" +
		`{"type":"task","task":{"id":"tb1","project_id":"pb","title":"B task","status":"blocked","priority":2,"blocked_by":"ta1"}}` + "\n" +
		`{"type":"decision","decision":{"id":"da1","project_id":"pa","title":"A decision","decision":"use x","rationale":"r","status":"active"}}` + "\n" +
		`{"type":"decision","decision":{"id":"db1","project_id":"pb","title":"B decision","decision":"use y","rationale":"r","status":"superseded","superseded_by":"da1"}}` + "\n"
	assertPointerOutcome(t, runPointerImportOnto(t, seed, artifact, false), runPointerImportOnto(t, seed, artifact, true),
		map[string]Action{"task/ta1": ActionSkip, "task/tb1": ActionCreate, "decision/da1": ActionSkip, "decision/db1": ActionCreate},
		map[string]string{}, nil,
		map[string]string{"task/tb1": "ta1", "decision/db1": "da1"})
}
