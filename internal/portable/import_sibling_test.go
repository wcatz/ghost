package portable

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestProjectsThatCollideWithinOneArtifactMapOntoTheFirst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pa, pb map[string]any
	}{
		{"same path", map[string]any{"id": "pa", "path": "/src/r", "name": "a"}, map[string]any{"id": "pb", "path": "/src/r", "name": "b"}},
		{"remote query", map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/r"}, map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/r?x=1"}},
		{"remote fragment", map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/r"}, map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/r#main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dry, apply := importBothWays(t, siblingArtifact(t, tc.pa, tc.pb))
			want := []struct {
				id     string
				action Action
			}{{"pa", ActionCreate}, {"pb", ActionSkip}, {"mb1", ActionCreate}, {"tb1", ActionCreate}}
			for _, run := range []struct {
				mode string
				seen []RecordResult
				rep  ImportReport
			}{{"dry", dry.seen, dry.report}, {"apply", apply.seen, apply.report}} {
				if len(run.seen) != len(want) || run.rep.Rejected != 0 {
					t.Fatalf("%s seen=%+v rep=%+v", run.mode, run.seen, run.rep)
				}
				for i, w := range want {
					if got := run.seen[i]; got.ID != w.id || got.Action != w.action {
						t.Fatalf("%s i=%d got=%+v", run.mode, i, got)
					}
				}
				if !strings.Contains(run.seen[1].Detail, "pa") {
					t.Fatalf("%s detail=%q", run.mode, run.seen[1].Detail)
				}
			}
			assertSameVerdicts(t, dry.seen, apply.seen)
			ctx := context.Background()
			projects, _ := apply.store.PortableProjects(ctx)
			memories, _ := apply.store.PortableMemories(ctx, []string{"pa"})
			tasks, _ := apply.store.ListTasks(ctx, "pa", "", 100)
			if len(projects) != 1 || projects[0].ID != "pa" || len(memories) != 1 || memories[0].ID != "mb1" || len(tasks) != 1 || tasks[0].ID != "tb1" {
				t.Fatalf("projects=%+v memories=%+v tasks=%+v", projects, memories, tasks)
			}
		})
	}
}

func TestDistinctProjectsInOneArtifactBothCreate(t *testing.T) {
	artifact := siblingArtifact(t, map[string]any{"id": "pa", "path": "/src/a", "name": "a", "repo_remote": "github.com/o/a"}, map[string]any{"id": "pb", "path": "/src/b", "name": "b", "repo_remote": "github.com/o/b"})
	dry, apply := importBothWays(t, artifact)
	for _, run := range [][]RecordResult{dry.seen, apply.seen} {
		for _, r := range run {
			if r.Action != ActionCreate {
				t.Fatalf("seen=%+v", run)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)
	ctx := context.Background()
	projects, _ := apply.store.PortableProjects(ctx)
	memories, _ := apply.store.PortableMemories(ctx, []string{"pb"})
	if len(projects) != 2 || len(memories) != 1 || memories[0].ID != "mb1" {
		t.Fatalf("projects=%+v memories=%+v", projects, memories)
	}
}

func TestARefusedProjectDoesNotClaimItsCheckout(t *testing.T) {
	artifact := siblingArtifact(t, map[string]any{"id": "pa", "path": "/src/r", "name": ""}, map[string]any{"id": "pb", "path": "/src/r", "name": "b"})
	dry, apply := importBothWays(t, artifact)
	want := []Action{ActionReject, ActionCreate, ActionCreate, ActionCreate}
	for _, seen := range [][]RecordResult{dry.seen, apply.seen} {
		if len(seen) != len(want) {
			t.Fatalf("seen=%+v", seen)
		}
		for i, w := range want {
			if seen[i].Action != w {
				t.Fatalf("i=%d seen=%+v", i, seen)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)
}

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
	for _, seen := range [][]RecordResult{dry.seen, apply.seen} {
		for i, w := range want {
			if seen[i].Action != w {
				t.Fatalf("i=%d seen=%+v", i, seen)
			}
		}
	}
	assertSameVerdicts(t, dry.seen, apply.seen)
	memories, _ := apply.store.PortableMemories(context.Background(), []string{"pa"})
	if len(memories) != 1 || memories[0].ID != "mc1" {
		t.Fatalf("memories=%+v", memories)
	}
}

func siblingArtifact(t *testing.T, pa, pb map[string]any) string {
	t.Helper()
	return strings.Join([]string{
		jsonLine(t, map[string]any{"type": "header", "schema_version": 2}),
		jsonLine(t, map[string]any{"type": "project", "project": pa}),
		jsonLine(t, map[string]any{"type": "project", "project": pb}),
		jsonLine(t, map[string]any{"type": "memory", "memory": map[string]any{"id": "mb1", "project_id": "pb", "category": "gotcha", "content": "under b", "source": "mcp"}}),
		jsonLine(t, map[string]any{"type": "task", "task": map[string]any{"id": "tb1", "project_id": "pb", "title": "b's task", "status": "pending", "priority": 1}}),
	}, "\n") + "\n"
}

type importRun struct {
	store  *memory.Store
	report ImportReport
	seen   []RecordResult
}

func importBothWays(t *testing.T, artifact string) (dry, apply importRun) {
	t.Helper()
	run := func(applyIt bool) importRun {
		s := newTestStore(t)
		out := importRun{store: s}
		report, err := Import(context.Background(), s, strings.NewReader(artifact), ImportOptions{Apply: applyIt}, func(r RecordResult) { out.seen = append(out.seen, r) })
		if err != nil {
			t.Fatalf("apply=%v err=%v", applyIt, err)
		}
		out.report = report
		return out
	}
	return run(false), run(true)
}

func assertSameVerdicts(t *testing.T, dry, apply []RecordResult) {
	t.Helper()
	if len(dry) != len(apply) {
		t.Fatalf("len dry=%d apply=%d", len(dry), len(apply))
	}
	for i := range dry {
		d, a := dry[i], apply[i]
		if d.Type != a.Type || d.ID != a.ID || d.Line != a.Line || d.Action != a.Action {
			t.Fatalf("i=%d dry=%+v apply=%+v", i, d, a)
		}
	}
}
