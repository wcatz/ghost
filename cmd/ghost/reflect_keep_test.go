package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// stubHarness answers a reflect call with a canned reply, the way a real CLI
// harness would answer and without spawning one. The reply is a function so a
// test can quote ids it does not know until after the store has created them.
type stubHarness struct {
	reply func() string
}

func (s stubHarness) Reflect(_ context.Context, _ string) (string, ai.TokenUsage, error) {
	return s.reply(), ai.TokenUsage{}, nil
}

// consolidateForTest runs the real LLM tier — prompt, op parser, grounding
// check, the drop guard — over a store's own memories, and returns the result
// the way runReflect would receive it. Building the proposal by hand instead
// would skip every rule this change added, which is the point of the store-level
// test below.
func consolidateForTest(t *testing.T, input reflection.ReflectionInput, reply func() string) reflection.ReflectionResult {
	t.Helper()
	llm := reflection.NewLlmConsolidator(stubHarness{reply: reply})
	result, err := llm.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, d := range reflection.RetainGuardedDrops(reflection.AuditGuardedDrops(input, result)) {
		result.Memories = append(result.Memories, reflection.ReflectMemory{
			Category: d.Category, Content: d.Content, Importance: d.Importance, Tags: d.Tags,
		})
	}
	return result
}

func newReflectStore(t *testing.T, project string) (*memory.Store, context.Context) {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "reflect.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, project, "/tmp/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, ctx
}

// splitByScope is the project/global split runReflect performs on a consolidation
// result, so a test states a scope and asserts on the two lists the apply and the
// promotion path actually receive.
func splitByScope(mems []reflection.ReflectMemory) (project, global []reflection.ReflectMemory) {
	for _, m := range mems {
		if m.Scope == "global" {
			global = append(global, m)
		} else {
			project = append(project, m)
		}
	}
	return project, global
}

// reflectInputFor builds the consolidation input the way runReflect does: the
// project's own rows, ids and all.
func reflectInputFor(t *testing.T, store *memory.Store, project string) reflection.ReflectionInput {
	t.Helper()
	all, err := store.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	return reflection.ReflectionInput{ProjectName: project, ExistingMemories: all}
}

// idByContent returns the id of the stored row whose content contains substr, so
// a reply can name a memory without depending on the order a list query returns.
func idByContent(t *testing.T, store *memory.Store, project, substr string) string {
	t.Helper()
	all, err := store.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range all {
		if strings.Contains(m.Content, substr) {
			return m.ID
		}
	}
	t.Fatalf("no memory in %s contains %q: %+v", project, substr, all)
	return ""
}
