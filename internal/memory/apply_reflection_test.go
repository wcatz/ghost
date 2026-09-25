package memory

import (
	"context"
	"testing"
)

func reflectionMemory(category, content string) Memory {
	return Memory{Category: category, Content: content, Source: "reflection", Importance: 0.7, Tags: []string{}}
}

func TestApplyReflectionPromotesGlobalsAndReplacesProjectTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("fact", "old project memory")); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}

	preserved, promoted, kept, err := s.ApplyReflection(ctx, testProject,
		[]Memory{reflectionMemory("fact", "new project memory")},
		[]Memory{reflectionMemory("preference", "new global memory")},
		"", true)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if len(preserved) != 0 || promoted != 1 || kept != 0 {
		t.Fatalf("result = (%v, %d, %d), want no preserved rows, 1 promoted, 0 kept", preserved, promoted, kept)
	}

	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 1 || project[0].Content != "new project memory" {
		t.Fatalf("project memories = %+v, want only the replacement", project)
	}
	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 1 || globals[0].Content != "new global memory" {
		t.Fatalf("global memories = %+v, want the promoted candidate", globals)
	}
}

func TestApplyReflectionReturnsFailedGlobalToProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("fact", "old project memory")); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_global_insert BEFORE INSERT ON memories
		WHEN NEW.project_id = '_global'
		BEGIN SELECT RAISE(ABORT, 'injected global failure'); END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, promoted, kept, err := s.ApplyReflection(ctx, testProject,
		[]Memory{reflectionMemory("fact", "new project memory")},
		[]Memory{reflectionMemory("preference", "global candidate")},
		"", true)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || kept != 1 {
		t.Fatalf("result promoted=%d kept=%d, want 0/1", promoted, kept)
	}

	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 2 || !containsMemoryContent(project, "global candidate") {
		t.Fatalf("project memories = %+v, want the failed global returned", project)
	}
	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 0 {
		t.Fatalf("global memories = %+v, want none after injected failure", globals)
	}
}

func TestApplyReflectionRollsBackWhenFailedGlobalCannotBeReturned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("fact", "old project memory")); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_global_and_recovery BEFORE INSERT ON memories
		WHEN NEW.content = 'global candidate'
		BEGIN SELECT RAISE(ABORT, 'injected global and recovery failure'); END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	_, promoted, kept, err := s.ApplyReflection(ctx, testProject,
		[]Memory{reflectionMemory("fact", "new project memory")},
		[]Memory{reflectionMemory("preference", "global candidate")},
		"", true)
	if err == nil {
		t.Fatal("ApplyReflection succeeded despite both promotion and recovery failing")
	}
	if promoted != 0 || kept != 0 {
		t.Errorf("result promoted=%d kept=%d, want zero on rollback", promoted, kept)
	}
	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 1 || project[0].Content != "old project memory" {
		t.Fatalf("project memories after rollback = %+v, want the original row", project)
	}
	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 0 {
		t.Fatalf("global memories after rollback = %+v, want none", globals)
	}
}

func TestApplyReflectionKeepsGlobalsProjectScopedByDefault(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil,
		[]Memory{reflectionMemory("preference", "global candidate")}, "", false)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || kept != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 0/0", promoted, kept)
	}
	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 1 || project[0].Content != "global candidate" {
		t.Fatalf("project memories = %+v, want the default project-scoped candidate", project)
	}
	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 0 {
		t.Fatalf("global memories = %+v, want none without opt-in", globals)
	}
}

func TestApplyReflectionKeepsCandidatesWhenGlobalProjectCannotBeEnsured(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_global_project BEFORE INSERT ON projects
		WHEN NEW.id = '_global'
		BEGIN SELECT RAISE(ABORT, 'injected global project failure'); END
	`); err != nil {
		t.Fatalf("create project failure trigger: %v", err)
	}

	_, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil,
		[]Memory{reflectionMemory("preference", "global candidate")}, "", true)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || kept != 1 {
		t.Fatalf("result promoted=%d kept=%d, want 0/1", promoted, kept)
	}
	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 1 || project[0].Content != "global candidate" {
		t.Fatalf("project memories = %+v, want the fallback candidate", project)
	}
}

func TestApplyReflectionPromotesOnlyGlobals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil,
		[]Memory{reflectionMemory("preference", "only global")}, "", true)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 1 || kept != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 1/0", promoted, kept)
	}
	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 1 || globals[0].Content != "only global" {
		t.Fatalf("global memories = %+v, want the only-global result", globals)
	}
}

func containsMemoryContent(memories []Memory, content string) bool {
	for _, m := range memories {
		if m.Content == content {
			return true
		}
	}
	return false
}
