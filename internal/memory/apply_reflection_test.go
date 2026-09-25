package memory

import (
	"context"
	"path/filepath"
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
	if len(preserved) != 0 || promoted != 1 || len(kept) != 0 {
		t.Fatalf("result = (%v, %d, %d), want no preserved rows, 1 promoted, 0 kept", preserved, promoted, len(kept))
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

func TestApplyReflectionPreservesScopeAndProvenanceOnPromotion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	confidence := 0.73
	candidate := reflectionMemory("preference", "scoped global")
	candidate.Scope = map[string]string{"environment": "development"}
	candidate.Agent = "opencode"
	candidate.SessionID = "session-1"
	candidate.SourceRef = "PR-567"
	candidate.Confidence = &confidence

	if _, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil, []Memory{candidate}, "", true); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	} else if promoted != 1 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 1/0", promoted, len(kept))
	}

	global, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(global) != 1 {
		t.Fatalf("global memories = %+v, want one row", global)
	}
	got := global[0]
	if got.Scope["environment"] != "development" || got.Agent != "opencode" || got.SessionID != "session-1" || got.SourceRef != "PR-567" || got.Confidence == nil || *got.Confidence != confidence {
		t.Errorf("promoted metadata = %+v, want scope and provenance preserved", got)
	}
}

func TestApplyReflectionPreservesScopeAndProvenanceOnRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	confidence := 0.61
	candidate := reflectionMemory("preference", "recovered global")
	candidate.Scope = map[string]string{"environment": "staging"}
	candidate.Agent = "claude-code"
	candidate.SessionID = "session-2"
	candidate.SourceRef = "PR-567"
	candidate.Confidence = &confidence
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_global_metadata BEFORE INSERT ON memories
		WHEN NEW.project_id = '_global'
		BEGIN SELECT RAISE(ABORT, 'injected global failure'); END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil, []Memory{candidate}, "", true); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	} else if promoted != 0 || len(kept) != 1 {
		t.Fatalf("result promoted=%d kept=%d, want 0/1", promoted, len(kept))
	}
	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if len(project) != 1 {
		t.Fatalf("project memories = %+v, want one recovered row", project)
	}
	got := project[0]
	if got.Scope["environment"] != "staging" || got.Agent != "claude-code" || got.SessionID != "session-2" || got.SourceRef != "PR-567" || got.Confidence == nil || *got.Confidence != confidence {
		t.Errorf("recovered metadata = %+v, want scope and provenance preserved", got)
	}
}

func TestApplyReflectionRecoveryUsesProjectDedupSemantics(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("preference", "duplicate candidate")); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fail_global_dedup BEFORE INSERT ON memories
		WHEN NEW.project_id = '_global'
		BEGIN SELECT RAISE(ABORT, 'injected global failure'); END
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, _, _, err := s.ApplyReflection(ctx, testProject, nil,
		[]Memory{reflectionMemory("preference", "duplicate candidate")}, "", true); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	var links int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memory_links
		WHERE relation = 'duplicate' AND source_id != target_id
	`).Scan(&links); err != nil {
		t.Fatalf("count duplicate links: %v", err)
	}
	if links == 0 {
		t.Fatal("recovery wrote a duplicate without the normal duplicate link")
	}
}

func TestApplyReflectionDoesNotFoldAcrossScopes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	production := reflectionMemory("preference", "scoped duplicate")
	production.Scope = map[string]string{"environment": "production"}
	if _, err := s.Create(ctx, "_global", production); err != nil {
		t.Fatalf("seed production memory: %v", err)
	}

	development := production
	development.Scope = map[string]string{"environment": "development"}
	if _, promoted, kept, err := s.ApplyReflection(ctx, testProject, nil, []Memory{development}, "", true); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	} else if promoted != 1 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 1/0", promoted, len(kept))
	}

	global, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(global) != 2 {
		t.Fatalf("global memories = %+v, want separate production/development rows", global)
	}
	foundDevelopment := false
	for _, row := range global {
		if row.Scope["environment"] == "development" {
			foundDevelopment = true
		}
	}
	if !foundDevelopment {
		t.Errorf("development candidate was folded into the production-scoped row: %+v", global)
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
	if promoted != 0 || len(kept) != 1 {
		t.Fatalf("result promoted=%d kept=%d, want 0/1", promoted, len(kept))
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
	if promoted != 0 || len(kept) != 0 {
		t.Errorf("result promoted=%d kept=%d, want zero on rollback", promoted, len(kept))
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
	if promoted != 0 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 0/0", promoted, len(kept))
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
	if promoted != 0 || len(kept) != 1 {
		t.Fatalf("result promoted=%d kept=%d, want 0/1", promoted, len(kept))
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
	if promoted != 1 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 1/0", promoted, len(kept))
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

// TestApplyReflectionFoldsParaphrasesIntoOneGlobalRow is the #544 regression at
// the level that actually runs it. The option existed and was unit-tested, but
// promoteReflectionCandidate never passed it, so FoldOnly had no production
// caller and the default fold stored every paraphrase as its own row — the exact
// shape issue #544 measured, 68 redundant rows in 19 clusters with nine
// paraphrases of a single gouroboros fact.
//
// Two promotions of the same fact, in separate rounds, must leave one _global
// row. Asserted against a real store because the wiring, not the option, is what
// is under test.
func TestApplyReflectionFoldsParaphrasesIntoOneGlobalRow(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "promote.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The same fact as two different projects' reflect passes would phrase it.
	// The same sentence, re-cased and re-punctuated by a different project's
	// reflect pass: the shape FoldOnly is defined to collapse.
	first := Memory{Category: "preference", Content: "run the gouroboros release checklist from the ops runbook", Source: "reflection", Importance: 0.6}
	second := Memory{Category: "preference", Content: "Run The Gouroboros Release Checklist From The Ops Runbook.", Source: "reflection", Importance: 0.6}

	if _, promoted, _, err := store.ApplyReflection(ctx, "proj", nil, []Memory{first}, "", true); err != nil {
		t.Fatalf("first ApplyReflection: %v", err)
	} else if promoted != 1 {
		t.Fatalf("first promotion = %d, want 1", promoted)
	}
	if _, _, _, err := store.ApplyReflection(ctx, "proj", nil, []Memory{second}, "", true); err != nil {
		t.Fatalf("second ApplyReflection: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = '_global'`).Scan(&n); err != nil {
		t.Fatalf("count _global: %v", err)
	}
	if n != 1 {
		rows, _ := store.GetAll(ctx, "_global", 10)
		t.Errorf("_global holds %d rows, want 1 — FoldOnly is not reaching the promotion path: %+v", n, rows)
	}
}
