package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func reflectionMemory(category, content string) Memory {
	return Memory{Category: category, Content: content, Source: "reflection", Importance: 0.7, Tags: []string{}}
}

// TestApplyReflectionReportsAnOverflowDropApartFromAGuardDrop is #1228 at the
// apply boundary: the split the store makes has to reach the caller, because the
// caller's report and its skip-fingerprint decision are what the issue names.
// One emission is refused for each reason in the same round, and the two counts
// must name them separately rather than as one total that reads as "a row
// changed underneath".
func TestApplyReflectionReportsAnOverflowDropApartFromAGuardDrop(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var mems []string
	for i := 0; i < 13; i++ {
		_, mem, _, err := s.RecordDecision(ctx, testProject, fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		mems = append(mems, mem)
	}
	fresh := reflectionMemory("fact", "a brand new consolidated fact")

	_, _, _, projectDropped, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, []Memory{
		{Category: "decision", Content: "Thirteen decisions in one", Importance: 0.9, ReplacesIDs: mems},
		fresh,
	}, nil, "", false, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if projectDropped.Overflow != 1 || projectDropped.OverflowLinks != 13 {
		t.Errorf("project overflow = %+v, want 1 emission over 13 decision links", projectDropped)
	}
	if projectDropped.Changed != 0 {
		t.Errorf("project guard drops = %d, want 0: nothing changed under this pass", projectDropped.Changed)
	}
	if globalDropped.Total() != 0 {
		t.Errorf("global drops = %+v, want none", globalDropped)
	}
}

func TestApplyReflectionPromotesGlobalsAndReplacesProjectTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("fact", "old project memory")); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}

	preserved, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, []Memory{reflectionMemory("fact", "new project memory")}, []Memory{reflectionMemory("preference", "new global memory")}, "", true, nil)

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

	if _, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{candidate}, "", true, nil); err != nil {
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

	if _, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{candidate}, "", true, nil); err != nil {
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

	if _, _, _, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{reflectionMemory("preference", "duplicate candidate")}, "", true, nil); err != nil {
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
	if _, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{development}, "", true, nil); err != nil {
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

	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, []Memory{reflectionMemory("fact", "new project memory")}, []Memory{reflectionMemory("preference", "global candidate")}, "", true, nil)

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

	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, []Memory{reflectionMemory("fact", "new project memory")}, []Memory{reflectionMemory("preference", "global candidate")}, "", true, nil)

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

	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{reflectionMemory("preference", "global candidate")}, "", false, nil)

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

	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{reflectionMemory("preference", "global candidate")}, "", true, nil)

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

	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, nil, []Memory{reflectionMemory("preference", "only global")}, "", true, nil)

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

// TestApplyReflectionPromotionInheritsSourceScope: a cross-project candidate
// promoted into _global must carry the scope its sources agreed on. Reflection
// carries no machine-readable scope, so without resolving it from the sources
// under the project the promotion lands unscoped and a production-only fact
// applies in every project — the widening the project replace already refuses,
// on the one path that leaves the project.
//
// The project emission does NOT claim the source, so the project replace deletes
// it before promotion runs. That ordering is the point: the scope has to be read
// while the sources still exist, or resolving it at promotion time reads an empty
// row and the promoted memory lands unscoped anyway.
func TestApplyReflectionPromotionInheritsSourceScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	src, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a production-only fact that gets promoted",
		Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}

	projectMems := []Memory{{Category: "fact", Content: "an unrelated project fact", Source: "reflection", Importance: 0.4}}
	_, promoted, kept, _, _, _, _, err := s.ApplyReflection(ctx, testProject, projectMems, []Memory{
		{Category: "fact", Content: "a cross-project fact", Source: "reflection", Importance: 0.6, ReplacesIDs: []string{src}},
	}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 1 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 1/0", promoted, len(kept))
	}

	// The project replace ran and took the source with it: without that the
	// fixture would not exercise the read-before-delete ordering.
	if project, err := s.GetAll(ctx, testProject, -1); err != nil {
		t.Fatalf("GetAll project: %v", err)
	} else if containsMemoryContent(project, "a production-only fact that gets promoted") {
		t.Fatalf("project = %+v, want the source deleted by the project replace", project)
	}

	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 1 {
		t.Fatalf("_global = %+v, want one promoted row", globals)
	}
	if globals[0].Scope["environment"] != "production" {
		t.Errorf("promoted scope = %v, want environment=production — the promotion dropped the sources' scope", globals[0].Scope)
	}
}

// TestApplyReflectionPromotionRefusedOnScopeConflict: a candidate whose sources
// disagree on scope is neither promoted nor written into the project. There is
// no single scope to promote under, and an unscoped write-back would replace the
// scoped sources with one unscoped row — the silent widening one level in — so
// the merge is refused outright, counted as a dropped cross-project emission, and
// both sources are held through the project replace.
//
// The project emission does not claim the sources, so the project replace runs
// and would delete them as unaccounted rows if the refusal did not keep them.
func TestApplyReflectionPromotionRefusedOnScopeConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	src1, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "staging fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	projectMems := []Memory{{Category: "fact", Content: "an unrelated project fact", Source: "reflection", Importance: 0.4}}
	_, promoted, kept, _, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, projectMems, []Memory{
		{Category: "fact", Content: "a cross-project fact", Source: "reflection", Importance: 0.6, ReplacesIDs: []string{src1, src2}},
	}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 0/0 — a candidate whose sources disagree is dropped, not kept", promoted, len(kept))
	}
	if globalDropped.Scope != 1 || globalDropped.Changed != 0 {
		t.Errorf("globalDropped = %+v, want one scope refusal for the refused candidate and no concurrent-edit drop", globalDropped)
	}

	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 0 {
		t.Errorf("_global = %+v, want no promotion for a disagreeing candidate", globals)
	}

	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if containsMemoryContent(project, "a cross-project fact") {
		t.Errorf("project = %+v, want no unscoped merged row written", project)
	}
	assertMemoryScope(t, project, "production fact", map[string]string{"environment": "production"})
	assertMemoryScope(t, project, "staging fact", map[string]string{"environment": "staging"})
}

// TestApplyReflectionPromotionRefusedOnScopeMix: a candidate whose sources MIX
// scoped and unscoped rows is refused exactly as a disagreement is. There is no
// single scope it could carry — unscoped would widen the scoped source, scoped
// would narrow the unscoped one — so the merge is dropped and its sources are
// kept. The project replace would otherwise delete both as unaccounted.
func TestApplyReflectionPromotionRefusedOnScopeMix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	scoped, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	unscoped, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "everywhere fact", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create unscoped: %v", err)
	}

	projectMems := []Memory{{Category: "fact", Content: "an unrelated project fact", Source: "reflection", Importance: 0.4}}
	_, promoted, kept, _, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, projectMems, []Memory{
		{Category: "fact", Content: "a cross-project fact", Source: "reflection", Importance: 0.6, ReplacesIDs: []string{scoped, unscoped}},
	}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || len(kept) != 0 {
		t.Fatalf("result promoted=%d kept=%d, want 0/0 — a scoped/unscoped mix is refused like a disagreement", promoted, len(kept))
	}
	if globalDropped.Scope != 1 || globalDropped.Changed != 0 {
		t.Errorf("globalDropped = %+v, want one scope refusal for the refused candidate and no concurrent-edit drop", globalDropped)
	}

	globals, err := s.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll global: %v", err)
	}
	if len(globals) != 0 {
		t.Errorf("_global = %+v, want no promotion for a mixed candidate", globals)
	}

	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	if containsMemoryContent(project, "a cross-project fact") {
		t.Errorf("project = %+v, want no unscoped merged row written", project)
	}
	assertMemoryScope(t, project, "production fact", map[string]string{"environment": "production"})
	assertMemoryScope(t, project, "everywhere fact", nil)
}

// TestApplyReflectionPromotionScopeRefusalIsNotASurvivor is #1492 on the
// promotion half: a cross-project candidate refused over its sources' scopes
// keeps those sources through the shared view, and the project replace must not
// then report them as preserved survivors — they did not move, and every later
// run refuses the same candidate again. Reporting them is what withheld the
// skip fingerprint on the --promote-globals path.
func TestApplyReflectionPromotionScopeRefusalIsNotASurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	src1, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "staging fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	projectMems := []Memory{{Category: "fact", Content: "an unrelated project fact", Source: "reflection", Importance: 0.4}}
	preserved, _, _, projectDropped, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, projectMems, []Memory{
		{Category: "fact", Content: "a cross-project fact", Source: "reflection", Importance: 0.6, ReplacesIDs: []string{src1, src2}},
	}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if globalDropped.Scope != 1 || globalDropped.Changed != 0 || projectDropped.Total() != 0 {
		t.Errorf("drops = project %+v / global %+v, want one global scope refusal and nothing else", projectDropped, globalDropped)
	}
	if len(preserved) != 0 {
		t.Errorf("preserved = %v, want empty: the sources a scope refusal keeps are not survivors a later run can merge", preserved)
	}
	project, err := s.GetAll(ctx, testProject, -1)
	if err != nil {
		t.Fatalf("GetAll project: %v", err)
	}
	assertMemoryScope(t, project, "production fact", map[string]string{"environment": "production"})
	assertMemoryScope(t, project, "staging fact", map[string]string{"environment": "staging"})
}

func containsMemoryContent(memories []Memory, content string) bool {
	for _, m := range memories {
		if m.Content == content {
			return true
		}
	}
	return false
}

// assertMemoryScope fails unless memories holds exactly one row with content and
// its scope equals want (nil means unscoped). A refused candidate must leave its
// sources in place, scopes and all, so this pins both halves of that: the row is
// still there, and it still says what it said.
func assertMemoryScope(t *testing.T, memories []Memory, content string, want map[string]string) {
	t.Helper()
	for _, m := range memories {
		if m.Content != content {
			continue
		}
		if !ScopeEquals(m.Scope, want) {
			t.Errorf("%q scope = %v, want %v", content, m.Scope, want)
		}
		return
	}
	t.Errorf("%q not found in %+v — a refused candidate must keep its sources", content, memories)
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

	if _, promoted, _, _, _, _, _, err := store.ApplyReflection(ctx, "proj", nil, []Memory{first}, "", true, nil); err != nil {
		t.Fatalf("first ApplyReflection: %v", err)
	} else if promoted != 1 {
		t.Fatalf("first promotion = %d, want 1", promoted)
	}
	if _, _, _, _, _, _, _, err := store.ApplyReflection(ctx, "proj", nil, []Memory{second}, "", true, nil); err != nil {
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
