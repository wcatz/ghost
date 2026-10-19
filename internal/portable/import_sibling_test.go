package portable

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// An artifact can carry two projects that name the same checkout path or the
// same repository: an older release stored remotes raw, so its export can hold
// `github.com/o/r` beside `github.com/o/r?x=1`, and a hand-merged pair of
// exports from two machines can hold the same path twice. The second project
// is the same checkout as the first, so it is mapped onto the first exactly as
// it would be mapped onto a project the store already held. The dry run and the
// apply have to agree on that, record for record, and the children of the
// second project have to land under the first.
func TestProjectsThatCollideWithinOneArtifactMapOntoTheFirst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pa, pb map[string]any
	}{
		{
			name: "same path",
			pa:   map[string]any{"id": "pa", "path": "/src/r", "name": "a"},
			pb:   map[string]any{"id": "pb", "path": "/src/r", "name": "b"},
		},
		{
			name: "remotes differing by a query",
			pa:   map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/r"},
			pb:   map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/r?x=1"},
		},
		{
			name: "remotes differing by a fragment",
			pa:   map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/r"},
			pb:   map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/r#main"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := siblingArtifact(t, tc.pa, tc.pb)
			dry, apply := importBothWays(t, artifact)

			want := []struct {
				id     string
				action Action
			}{
				{"pa", ActionCreate},
				{"pb", ActionSkip},
				{"mb1", ActionCreate},
				{"tb1", ActionCreate},
			}
			for _, run := range []struct {
				mode string
				seen []RecordResult
			}{{"dry run", dry.seen}, {"apply", apply.seen}} {
				if len(run.seen) != len(want) {
					t.Fatalf("%s reported %d records, want %d: %+v", run.mode, len(run.seen), len(want), run.seen)
				}
				for i, w := range want {
					got := run.seen[i]
					if got.ID != w.id || got.Action != w.action {
						t.Errorf("%s record %d = %s %s (%v), want %s %s", run.mode, i, got.ID, got.Action, got.Error, w.id, w.action)
					}
				}
				// The skip says where the project went, so the reader sees
				// an attach rather than "already imported".
				if !strings.Contains(run.seen[1].Detail, "pa") {
					t.Errorf("%s: the second project's line %q does not name the project it maps onto", run.mode, run.seen[1].Detail)
				}
			}
			if dry.report.Rejected != 0 || apply.report.Rejected != 0 {
				t.Errorf("rejected: dry run %d %v, apply %d %v; want none",
					dry.report.Rejected, dry.report.Errors, apply.report.Rejected, apply.report.Errors)
			}
			assertSameVerdicts(t, dry.seen, apply.seen)

			// The apply wrote one project, and the second project's children
			// live under it.
			ctx := context.Background()
			projects, err := apply.store.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}
			if len(projects) != 1 || projects[0].ID != "pa" {
				t.Fatalf("apply left projects %+v, want only pa", projects)
			}
			memories, err := apply.store.PortableMemories(ctx, []string{"pa"})
			if err != nil {
				t.Fatalf("PortableMemories: %v", err)
			}
			if len(memories) != 1 || memories[0].ID != "mb1" {
				t.Errorf("pa holds memories %+v, want mb1", memories)
			}
			tasks, err := apply.store.ListTasks(ctx, "pa", "", 100)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(tasks) != 1 || tasks[0].ID != "tb1" {
				t.Errorf("pa holds tasks %+v, want tb1", tasks)
			}
		})
	}
}

// The control: two projects with their own path and their own repository are
// two projects, and both are created in both modes.
func TestDistinctProjectsInOneArtifactBothCreate(t *testing.T) {
	artifact := siblingArtifact(t,
		map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/a"},
		map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/b"},
	)
	dry, apply := importBothWays(t, artifact)
	for _, run := range []struct {
		mode string
		seen []RecordResult
	}{{"dry run", dry.seen}, {"apply", apply.seen}} {
		for _, r := range run.seen {
			if r.Action != ActionCreate {
				t.Errorf("%s: %s %s = %s (%v), want create", run.mode, r.Type, r.ID, r.Action, r.Error)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)

	ctx := context.Background()
	projects, err := apply.store.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("apply left projects %+v, want pa and pb", projects)
	}
	memories, err := apply.store.PortableMemories(ctx, []string{"pb"})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(memories) != 1 || memories[0].ID != "mb1" {
		t.Errorf("pb holds memories %+v, want mb1", memories)
	}
}

// A first project the run refuses writes nothing, so it claims no checkout: the
// second project with the same path is judged on its own record and created,
// in both modes, rather than mapped onto a project that will never exist.
func TestARefusedProjectDoesNotClaimItsCheckout(t *testing.T) {
	artifact := siblingArtifact(t,
		// No name: the store refuses the project on its own record.
		map[string]any{"id": "pa", "path": "/src/r", "name": ""},
		map[string]any{"id": "pb", "path": "/src/r", "name": "b"},
	)
	dry, apply := importBothWays(t, artifact)
	want := []Action{ActionReject, ActionCreate, ActionCreate, ActionCreate}
	for _, run := range []struct {
		mode string
		seen []RecordResult
	}{{"dry run", dry.seen}, {"apply", apply.seen}} {
		if len(run.seen) != len(want) {
			t.Fatalf("%s reported %d records, want %d", run.mode, len(run.seen), len(want))
		}
		for i, w := range want {
			if run.seen[i].Action != w {
				t.Errorf("%s record %d (%s) = %s (%v), want %s", run.mode, i, run.seen[i].ID, run.seen[i].Action, run.seen[i].Error, w)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)
}

// A repeated project id whose first record is refused is decided by the next
// record with that id, as the apply decides it: the second pa is created and
// claims the path, so pc, naming the same path, is mapped onto pa rather than
// promised a create the apply would refuse.
func TestARepeatedIDIsDecidedByItsFirstAcceptedRecord(t *testing.T) {
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "pa", "path": "/src/r", "name": ""}}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "pa", "path": "/src/r", "name": "b"}}),
		jsonLine(t, map[string]any{"type": "project", "project": map[string]any{"id": "pc", "path": "/src/r", "name": "c"}}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "mc1", "project_id": "pc", "category": "gotcha", "content": "under c", "source": "mcp"}}),
	}
	dry, apply := importBothWays(t, strings.Join(lines, "\n")+"\n")
	want := []Action{ActionReject, ActionCreate, ActionSkip, ActionCreate}
	for _, run := range []struct {
		mode string
		seen []RecordResult
	}{{"dry run", dry.seen}, {"apply", apply.seen}} {
		if len(run.seen) != len(want) {
			t.Fatalf("%s reported %d records, want %d", run.mode, len(run.seen), len(want))
		}
		for i, w := range want {
			if run.seen[i].Action != w {
				t.Errorf("%s record %d (%s) = %s (%v), want %s", run.mode, i, run.seen[i].ID, run.seen[i].Action, run.seen[i].Error, w)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)

	memories, err := apply.store.PortableMemories(context.Background(), []string{"pa"})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(memories) != 1 || memories[0].ID != "mc1" {
		t.Errorf("pa holds memories %+v, want mc1", memories)
	}
}

// siblingArtifact is an artifact holding the two projects given, then a memory
// and a task under the second.
func siblingArtifact(t *testing.T, pa, pb map[string]any) string {
	t.Helper()
	lines := []string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": pa}),
		jsonLine(t, map[string]any{"type": "project", "project": pb}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "mb1", "project_id": "pb", "category": "gotcha", "content": "under b", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{"id": "tb1", "project_id": "pb", "title": "b's task", "status": "pending", "priority": 1}}),
	}
	return strings.Join(lines, "\n") + "\n"
}

// importRun is one Import of an artifact into a fresh store.
type importRun struct {
	store  *memory.Store
	report ImportReport
	seen   []RecordResult
}

// importBothWays imports the artifact as a dry run into one fresh store and
// as an apply into another.
func importBothWays(t *testing.T, artifact string) (dry, apply importRun) {
	t.Helper()
	run := func(applyIt bool) importRun {
		s := newTestStore(t)
		out := importRun{store: s}
		report, err := Import(context.Background(), s, strings.NewReader(artifact), ImportOptions{Apply: applyIt}, func(r RecordResult) {
			out.seen = append(out.seen, r)
		})
		if err != nil {
			t.Fatalf("Import (apply=%v): %v", applyIt, err)
		}
		out.report = report
		return out
	}
	return run(false), run(true)
}

// assertSameVerdicts fails when the dry run and the apply disagree about any
// record.
func assertSameVerdicts(t *testing.T, dry, apply []RecordResult) {
	t.Helper()
	if len(dry) != len(apply) {
		t.Fatalf("the dry run reported %d records and the apply %d", len(dry), len(apply))
	}
	for i := range dry {
		d, a := dry[i], apply[i]
		if d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action {
			t.Errorf("record %d disagrees: dry %s %s line %d = %s, apply %s %s line %d = %s (%v)",
				i, d.Type, d.ID, d.Line, d.Action, a.Type, a.ID, a.Line, a.Action, a.Error)
		}
	}
}
