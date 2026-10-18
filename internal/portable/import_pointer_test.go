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

// chainIntoCycle is n task records, task i blocked by task i+1, the last two
// blocking each other: one long chain that ends in a 2-cycle.
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

// TestCycleMembersIsLinear: finding a cycle's members walks each record once.
// It counts the calls made to the id and pointer accessors rather than timing
// anything, so it fails on a quadratic or cubic walk however fast the machine.
// The walk that preceded it re-walked every record's whole path with a linear
// scan per step and made a dry run of 4000 such tasks take minutes; at this
// size it fails in seconds rather than minutes.
func TestCycleMembersIsLinear(t *testing.T) {
	const n = 1000
	recs := taskRecords(chainIntoCycle(n))
	calls := 0
	idOf := func(r parsedRecord) string { calls++; return r.rec.Task.ID }
	refOf := func(r parsedRecord) string { calls++; return r.rec.Task.BlockedBy }

	members := cycleMembers(recs, idOf, refOf)
	for i, on := range members {
		if want := i >= n-2; on != want {
			t.Errorf("record c%d on a cycle = %v, want %v", i, on, want)
		}
	}
	if calls > 4*n {
		t.Errorf("cycleMembers made %d accessor calls for %d records, want at most %d — the walk is no longer linear", calls, n, 4*n)
	}
}

// TestTopoOrderOfAChainIntoACycleIsBounded: the whole ordering of that chain —
// the stall, the cycle broken, and one pass per link after it — stays within a
// quadratic number of accessor calls. A membership test that scanned the
// pending slice made each pass quadratic and the whole ordering cubic. Every
// link keeps its pointer and follows its target; only the two cycle members
// lose theirs.
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
		t.Fatalf("ordered %d records, want %d", len(got), n)
	}
	pos := map[string]int{}
	for i, r := range got {
		pos[r.rec.Task.ID] = i
	}
	for i, r := range got {
		task := r.rec.Task
		cycle := task.ID == fmt.Sprintf("c%d", n-1) || task.ID == fmt.Sprintf("c%d", n-2)
		if cycle != (task.BlockedBy == "") {
			t.Errorf("record %s blocked_by = %q after ordering", task.ID, task.BlockedBy)
		}
		if task.BlockedBy != "" && pos[task.BlockedBy] > i {
			t.Errorf("record %s is ordered before its blocker %s", task.ID, task.BlockedBy)
		}
	}
	if limit := 4 * n * n; calls > limit {
		t.Errorf("topoOrder made %d accessor calls for %d records, want at most %d — the ordering is worse than quadratic", calls, n, limit)
	}
}

// TestCycleMembersMarksOnlyCycles: membership is exact whatever the order the
// records are walked in — a tail listed after its cycle, a second tail joining
// the first one partway, a record pointing at itself, a pointer to an id
// outside the set, and two separate cycles. A walk that reaches a record an
// earlier walk finished must not mark its own path as a cycle.
func TestCycleMembersMarksOnlyCycles(t *testing.T) {
	rows := []struct{ id, blockedBy string }{
		{id: "a", blockedBy: "b"}, // cycle a <-> b
		{id: "b", blockedBy: "a"},
		{id: "c", blockedBy: "a"},       // tail into the cycle, listed after it
		{id: "d", blockedBy: "c"},       // tail of the tail
		{id: "e", blockedBy: "d"},       // joins the finished path at d
		{id: "s", blockedBy: "s"},       // points at itself
		{id: "x", blockedBy: "outside"}, // names an id outside the set
		{id: "p", blockedBy: "q"},       // a second cycle p -> q -> r -> p
		{id: "q", blockedBy: "r"},
		{id: "r", blockedBy: "p"},
		{id: "f", blockedBy: "q"}, // tail into the second cycle
		{id: "g", blockedBy: ""},
	}
	want := map[string]bool{"a": true, "b": true, "s": true, "p": true, "q": true, "r": true}
	recs := taskRecords(rows)
	members := cycleMembers(recs,
		func(r parsedRecord) string { return r.rec.Task.ID },
		func(r parsedRecord) string { return r.rec.Task.BlockedBy })
	for i, r := range rows {
		if members[i] != want[r.id] {
			t.Errorf("record %s on a cycle = %v, want %v", r.id, members[i], want[r.id])
		}
	}
}

// TestAPointerIntoAnotherProjectIsDropped: a blocked_by or superseded_by naming
// a record the store holds, but in a different project from the one the record
// is imported under, does not resolve. The store's own readers join a successor
// on the decision's project and no live writer links across projects, so such a
// pointer is a link nothing can follow. It is dropped and reported exactly like
// a pointer to a record that exists nowhere, in the dry run and the apply alike.
//
// Kept, it was also a link another project could rewrite: deleting the project
// that holds the target cleared both pointers through ON DELETE SET NULL,
// leaving a blocked task with no blocker and a superseded decision with no
// successor in a project the delete never named. So the test deletes that
// project afterwards and holds every row of the imported project unchanged.
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

	ctx := context.Background()
	before := projectRows(t, applied.store, "pb")
	if _, err := applied.store.DeleteProject(ctx, "pa", true); err != nil {
		t.Fatalf("DeleteProject(pa): %v", err)
	}
	if after := projectRows(t, applied.store, "pb"); after != before {
		t.Errorf("deleting project pa changed project pb's rows:\n before %s\n after  %s", before, after)
	}
}

// TestAPointerIntoTheMappedProjectIsKept: the project a record is imported
// under is the one the mapping chose, not the id the artifact names. A project
// whose checkout the store already records attaches to the store's project, and
// a pointer to a record that project holds resolves and is kept, unreported, in
// the dry run and the apply alike. A check that compared against the artifact's
// own project id would drop it.
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
		map[string]string{},
		nil,
		map[string]string{"task/tq1": "tx1", "decision/dq1": "dx1"})
}

// projectRows renders every task and decision row of one project, whole, so a
// comparison catches any field a later write changed.
func projectRows(t *testing.T, s *memory.Store, projectID string) string {
	t.Helper()
	tasks, err := selectTasks(context.Background(), s, []string{projectID})
	if err != nil {
		t.Fatalf("selectTasks: %v", err)
	}
	decisions, err := selectDecisions(context.Background(), s, []string{projectID})
	if err != nil {
		t.Fatalf("selectDecisions: %v", err)
	}
	return fmt.Sprintf("tasks=%d %+v decisions=%d %+v", len(tasks), tasks, len(decisions), decisions)
}

// TestAnInArtifactPointerIntoAnotherProjectIsDropped: a blocked_by or
// superseded_by naming a record of the same artifact is kept only when that
// record lands in the project the dependent is imported under. An artifact
// carrying two projects, pa with the targets and pb with the dependents, used to
// keep both pointers across the projects with nothing on the report: the state
// the store-held check refuses, which no live writer can make and which deleting
// pa rewrites through ON DELETE SET NULL. The pointer is now dropped and
// reported like one naming a record that exists nowhere, in the dry run and the
// apply alike, and deleting pa leaves pb's rows unchanged.
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

// TestAnInArtifactPointerToARecordStoredInAnotherProjectIsDropped: the target
// is in the artifact under pb, but the store already holds that id under pa, so
// the target record is a skip and the row the pointer would name lives in pa.
// A skip used to count as landed whatever project held the row, and the pointer
// was kept across projects. What decides is the project the stored row is in:
// the pointer is dropped and reported like one naming a record that exists
// nowhere, in both modes, and deleting pa leaves pb's rows unchanged.
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

// TestAnInArtifactPointerIntoTheMappedProjectIsKept is the control for the two
// above: the comparison is made after project mapping, on the project a record
// is imported under. The artifact's pq records the checkout the store's pb
// already has, so its records attach to pb. A target the artifact creates under
// pq, and a target it carries under pq that the store already holds under pb,
// both land in pb with their dependents, and both pointers are kept and nothing
// is reported, in the dry run and the apply alike. A comparison against the
// artifact's own project id, or one that treated every skip as landing
// elsewhere, would drop them.
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
		map[string]Action{
			"project/pq": ActionSkip,
			"task/tn1":   ActionCreate, "task/tn2": ActionCreate, "task/ts1": ActionSkip, "task/ts2": ActionCreate,
			"decision/dn1": ActionCreate, "decision/dn2": ActionCreate, "decision/ds1": ActionSkip, "decision/ds2": ActionCreate,
		},
		map[string]string{},
		nil,
		map[string]string{"task/tn2": "tn1", "task/ts2": "ts1", "decision/dn2": "dn1", "decision/ds2": "ds1"})
}

// TestAnInArtifactPointerToARecordStoredInItsOwnProjectIsKept: the other side
// of the skipped-target rule. The artifact puts ta1 and da1 under pa, but the
// store already holds both under pb, the project of the records pointing at
// them. A skip lands where the stored row is, so each pointer names a row in its
// own record's project and is kept, unreported, in the dry run and the apply
// alike. A rule that went by the project the artifact names would drop it.
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
		map[string]string{},
		nil,
		map[string]string{"task/tb1": "ta1", "decision/db1": "da1"})
}

// assertDeleteLeavesProject deletes project gone and holds every task and
// decision row of project kept unchanged across it.
func assertDeleteLeavesProject(t *testing.T, s *memory.Store, gone, kept string) {
	t.Helper()
	before := projectRows(t, s, kept)
	if _, err := s.DeleteProject(context.Background(), gone, true); err != nil {
		t.Fatalf("DeleteProject(%s): %v", gone, err)
	}
	if after := projectRows(t, s, kept); after != before {
		t.Errorf("deleting project %s changed project %s's rows:\n before %s\n after  %s", gone, kept, before, after)
	}
}
