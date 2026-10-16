package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A refused save or decision writes NOTHING: no memory, no project row, and no
// checkout binding. Every argument the store can refuse is judged before the
// handler opens the project, so the refusal cannot leave an empty project
// behind that the next session start in this checkout would name.

// refusalToken is a GitHub-token-shaped value: `ghp_` plus 36 characters, the
// shape the token guard refuses in every field it covers.
const refusalToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

type refusedWriteCase struct {
	name string
	tool string
	args map[string]any
}

func baseRefusedArgs(tool, projectID string) map[string]any {
	args := map[string]any{"project_id": projectID}
	switch tool {
	case "ghost_memory_save":
		args["content"] = "the notifier retries three times"
		args["category"] = "fact"
	case "ghost_save_global":
		delete(args, "project_id")
		args["content"] = "every service retries three times"
	case "ghost_decision_record":
		args["title"] = "Retry the notifier three times"
		args["decision"] = "Three attempts with exponential backoff"
		args["rationale"] = "The upstream answers 503 under load and recovers within seconds"
		args["alternatives"] = []string{"Give up immediately", "Retry forever"}
	}
	return args
}

// refusedWriteCases is every argument refusal that used to come AFTER the
// project was opened: the scope checks, the source_ref checks and the token
// guard on the stored text, for each tool that opens a project.
func refusedWriteCases(projectID string) []refusedWriteCase {
	with := func(tool string, set map[string]any) map[string]any {
		args := baseRefusedArgs(tool, projectID)
		for k, v := range set {
			args[k] = v
		}
		return args
	}
	manyKeys := map[string]any{}
	for i := 0; i <= memory.MaxScopeKeys; i++ {
		manyKeys[fmt.Sprintf("k%02d", i)] = "v"
	}
	const save = "ghost_memory_save"
	const decision = "ghost_decision_record"
	const global = "ghost_save_global"
	return []refusedWriteCase{
		{"save non-string scope value", save, with(save, map[string]any{"scope": map[string]any{"environment": 5}})},
		{"save scope over the key count", save, with(save, map[string]any{"scope": manyKeys})},
		{"save scope key over the byte bound", save, with(save, map[string]any{"scope": map[string]any{strings.Repeat("k", memory.MaxScopeKeyLen+1): "v"}})},
		{"save token-shaped scope value", save, with(save, map[string]any{"scope": map[string]any{"auth": refusalToken}})},
		{"save reserved source_ref", save, with(save, map[string]any{"source_ref": "decision:abc123"})},
		{"save token-shaped source_ref", save, with(save, map[string]any{"source_ref": refusalToken})},
		{"save over-long source_ref", save, with(save, map[string]any{"source_ref": strings.Repeat("r", memory.MaxSourceRefLen+1)})},
		{"save token-shaped content", save, with(save, map[string]any{"content": "the deploy token is " + refusalToken})},
		{"save token-shaped tag", save, with(save, map[string]any{"tags": []string{refusalToken}})},
		{"decision token-shaped title", decision, with(decision, map[string]any{"title": "Use " + refusalToken})},
		{"decision token-shaped decision", decision, with(decision, map[string]any{"decision": "Authenticate with " + refusalToken})},
		{"decision token-shaped rationale", decision, with(decision, map[string]any{"rationale": "Because " + refusalToken + " works"})},
		{"decision token-shaped alternative", decision, with(decision, map[string]any{"alternatives": []string{refusalToken}})},
		{"decision token-shaped tag", decision, with(decision, map[string]any{"tags": []string{refusalToken}})},
		{"global reserved source_ref", global, with(global, map[string]any{"source_ref": "decision:abc123"})},
		{"global token-shaped content", global, with(global, map[string]any{"content": "the deploy token is " + refusalToken})},
	}
}

// targetOf is the project row a case would have opened.
func targetOf(c refusedWriteCase, projectID string) string {
	if c.tool == "ghost_save_global" {
		return memory.GlobalProjectID
	}
	return projectID
}

// TestRefusedWriteOpensNoProjectAndBindsNoCheckout is the regression: each
// refusal shape, made from a fresh git checkout with a name the store has never
// seen, is an error result and leaves no project row and no checkout binding.
func TestRefusedWriteOpensNoProjectAndBindsNoCheckout(t *testing.T) {
	const typo = "chekout-typo"
	for _, c := range refusedWriteCases(typo) {
		t.Run(c.name, func(t *testing.T) {
			store := testStore(t)
			dir := filepath.Join(physTemp(t), "checkout")
			gitInit(t, dir)
			session := connectedClient(t, checkoutServer(t, store, dir))

			res := callTool(t, session, c.tool, c.args)
			if !res.IsError {
				t.Fatalf("%s was accepted, want a refusal: %s", c.tool, resultText(res))
			}
			if _, ok := projectPath(t, store, targetOf(c, typo)); ok {
				t.Fatalf("a refused %s opened project %q: %s", c.tool, targetOf(c, typo), resultText(res))
			}
			if id, _, err := store.ResolveProject(context.Background(), dir); err != nil || id != "" {
				t.Fatalf("checkout resolves to %q (%v) after a refused %s, want no project", id, err, c.tool)
			}
		})
	}
}

// TestRefusedTypoDoesNotCaptureTheCheckout is the user-visible story: a refused
// save under a misspelled name, then the corrected save. The corrected name must
// be the one bound to the checkout, and the misspelling must not exist.
func TestRefusedTypoDoesNotCaptureTheCheckout(t *testing.T) {
	for _, tool := range []string{"ghost_memory_save", "ghost_decision_record"} {
		t.Run(tool, func(t *testing.T) {
			store := testStore(t)
			dir := filepath.Join(physTemp(t), "checkout")
			gitInit(t, dir)
			srv := checkoutServer(t, store, dir)
			session := connectedClient(t, srv)

			refused := baseRefusedArgs(tool, "chekout-typo")
			if tool == "ghost_memory_save" {
				refused["scope"] = map[string]any{"auth": refusalToken}
			} else {
				refused["rationale"] = "Because " + refusalToken + " works"
			}
			if res := callTool(t, session, tool, refused); !res.IsError {
				t.Fatalf("refused call accepted: %s", resultText(res))
			}

			saveUnder(t, srv, tool, "checkout")

			if _, ok := projectPath(t, store, "chekout-typo"); ok {
				t.Fatal("the refused misspelling left a project behind")
			}
			if got, ok := projectPath(t, store, "checkout"); !ok || got != dir {
				t.Fatalf("checkout path = %q (found %v), want the checkout %q", got, ok, dir)
			}
			if id, _, err := store.ResolveProject(context.Background(), dir); err != nil || id != "checkout" {
				t.Fatalf("checkout resolves to %q (%v), want the corrected project", id, err)
			}
		})
	}
}

// TestRefusedWriteLeavesAnExistingProjectUntouched is the control on the other
// side: a checkout already bound to a project keeps that binding, and the
// project's row and memory count are byte-for-byte what they were, whether the
// refused call names a new project, the existing one by name, or the existing
// one by its checkout path.
func TestRefusedWriteLeavesAnExistingProjectUntouched(t *testing.T) {
	ctx := context.Background()
	for _, address := range []string{"new-name", "real", "path"} {
		t.Run(address, func(t *testing.T) {
			for _, c := range refusedWriteCases("") {
				t.Run(c.name, func(t *testing.T) {
					store := testStore(t)
					dir := filepath.Join(physTemp(t), "checkout")
					gitInit(t, dir)
					srv := checkoutServer(t, store, dir)
					saveUnder(t, srv, "ghost_memory_save", "real")

					projectID := address
					if address == "path" {
						projectID = dir
					}
					args := map[string]any{}
					for k, v := range c.args {
						args[k] = v
					}
					if c.tool != "ghost_save_global" {
						args["project_id"] = projectID
					}

					before, err := store.ListProjects(ctx)
					if err != nil {
						t.Fatal(err)
					}
					count, err := store.CountMemories(ctx, "real")
					if err != nil {
						t.Fatal(err)
					}

					if res := callTool(t, connectedClient(t, srv), c.tool, args); !res.IsError {
						t.Fatalf("refused call accepted: %s", resultText(res))
					}

					after, err := store.ListProjects(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("project rows changed under a refused %s:\nbefore %+v\nafter  %+v", c.tool, before, after)
					}
					if n, err := store.CountMemories(ctx, "real"); err != nil || n != count {
						t.Fatalf("real holds %d memories (%v), want %d", n, err, count)
					}
					if id, _, err := store.ResolveProject(ctx, dir); err != nil || id != "real" {
						t.Fatalf("checkout resolves to %q (%v), want it to stay bound to real", id, err)
					}
				})
			}
		})
	}
}

// TestAcceptedWriteStillOpensAndBinds is the success control: the same three
// tools, with nothing refusable in their arguments, still open the project and
// bind the checkout exactly as before.
func TestAcceptedWriteStillOpensAndBinds(t *testing.T) {
	for _, tool := range []string{"ghost_memory_save", "ghost_decision_record"} {
		t.Run(tool, func(t *testing.T) {
			store := testStore(t)
			dir := filepath.Join(physTemp(t), "checkout")
			gitInit(t, dir)
			srv := checkoutServer(t, store, dir)
			args := baseRefusedArgs(tool, "notifier")
			if tool == "ghost_memory_save" {
				args["scope"] = map[string]any{"environment": "production"}
				args["source_ref"] = "docs/runbook.md"
			}
			if res := callTool(t, connectedClient(t, srv), tool, args); res.IsError {
				t.Fatalf("%s failed: %s", tool, resultText(res))
			}
			if got, ok := projectPath(t, store, "notifier"); !ok || got != dir {
				t.Fatalf("notifier path = %q (found %v), want %q", got, ok, dir)
			}
			if n, err := store.CountMemories(context.Background(), "notifier"); err != nil || n != 1 {
				t.Fatalf("notifier holds %d memories (%v), want 1", n, err)
			}
		})
	}
	t.Run("ghost_save_global", func(t *testing.T) {
		store := testStore(t)
		srv := checkoutServer(t, store, "")
		if res := callTool(t, connectedClient(t, srv), "ghost_save_global", baseRefusedArgs("ghost_save_global", "")); res.IsError {
			t.Fatalf("ghost_save_global failed: %s", resultText(res))
		}
		if _, ok := projectPath(t, store, memory.GlobalProjectID); !ok {
			t.Fatal("an accepted global save did not open _global")
		}
	})
}
