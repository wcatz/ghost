package main

import (
	"context"
	"fmt"
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

// TestRunReflectApplyKeepsARowsTheConsolidatorCarriedThrough is the store-side
// half of the per-id contract (#639), driven end to end: the harness's operations
// are parsed and executed by the real tier, the drop guard runs, and the result
// reaches ReplaceNonManual. A memory the response carried through — whether by
// `keep` or by never naming it — is re-emitted byte for byte, so the row is
// updated in place and its id, embedding, link graph, age and source survive. The
// defect this replaces was a retyped memory arriving as a fresh id, measured at
// 12 of 13 new ids in one benchmarked project and 15 of 17 in another.
//
// The carried-through row is deliberately one the drop guard cannot rescue: the
// exporter note the harness explicitly kept shares 55% of its tokens, so a guard
// asking "is this memory absorbed somewhere" answers yes and stays quiet. Before
// the pass-through the row was emitted by nobody, so a guard false positive in
// that direction was a silent deletion with no warning and no --allow-drops —
// which is why this test checks the row rather than the absence of a warning.
//
// Against a real store rather than a fake, because the failure mode is a
// cascade-deleted embedding and a re-stamped created_at, which only a database
// can show.
func TestRunReflectApplyKeepsARowsTheConsolidatorCarriedThrough(t *testing.T) {
	store, ctx := newReflectStore(t, "proj")

	// Two rows the harness merges, and three it never mentions. The link joins
	// two rows that survive, so its survival is the carried-through rows' own
	// doing — a link to a merged-away row would cascade on that row's delete and
	// prove nothing about identity.
	manifests, err := store.Create(ctx, "proj", memory.Memory{
		Category: "gotcha", Content: "the ingest pipeline writes run manifests under /var/lib/ghost",
		Importance: 0.9, Tags: []string{"ingest", "manifests"}, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	neighbour, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the ingest pipeline reads its watermark from the same directory",
		Importance: 0.6, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the exporter also writes run manifests into that ingest pipeline",
		Importance: 0.4, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fsn1, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare",
		Importance: 0.5, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cloudflare, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "Cloudflare fronts the production region fsn1",
		Importance: 0.45, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.StoreEmbedding(ctx, manifests, []float32{0.1, 0.2, 0.3}, "test-model:3"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := store.CreateLink(ctx, manifests, neighbour, "related", 0.7, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	before := mustGetOne(t, store, manifests)

	// The harness merges the two region notes, explicitly keeps the exporter note,
	// and never mentions the two pipeline notes. The kept note is what blinds the
	// drop guard for one of them.
	result := consolidateForTest(t, reflectInputFor(t, store, "proj"), func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["keep %s","merge %s,%s -> Production in region fsn1 is fronted by Cloudflare"]}`,
			idByContent(t, store, "proj", "the exporter also"), fsn1, cloudflare)
	})

	projectMems, globalMems := splitByScope(result.Memories)
	if len(globalMems) != 0 {
		t.Fatalf("the merge is project-scoped, but %d candidates are global: %+v", len(globalMems), result.Memories)
	}
	if _, _, _, err := applyReflection(ctx, store, "proj", projectMems, globalMems, "", false); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	after := mustGetOne(t, store, manifests)
	if after.Content != before.Content || after.Category != before.Category {
		t.Errorf("the carried-through row was retyped: %+v", after)
	}
	if after.CreatedAt != before.CreatedAt {
		t.Errorf("created_at = %q, want the stored %q — a carried-through memory is not refreshed knowledge",
			after.CreatedAt, before.CreatedAt)
	}
	if after.Source != "mcp" {
		t.Errorf("source = %q, want mcp — consolidation must not relabel where a memory came from", after.Source)
	}
	if vec, err := store.GetEmbedding(ctx, manifests); err != nil || len(vec) == 0 {
		t.Errorf("the carried-through row lost its embedding (err=%v, len=%d)", err, len(vec))
	}
	links, err := store.GetLinks(ctx, manifests)
	if err != nil || len(links) != 1 || links[0].Relation != "related" {
		t.Errorf("the carried-through row lost its links (err=%v, links=%+v)", err, links)
	}
	if _, err := store.GetMemoryContent(ctx, neighbour); err != nil {
		t.Errorf("the id the harness never mentioned was deleted: %v", err)
	}

	// The two merged inputs are gone and the merge is a new row, which is what a
	// merge costs and a pass-through does not.
	all, err := store.GetAll(ctx, "proj", -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("project holds %d memories, want 3 carried through + 1 merged: %+v", len(all), all)
	}
	for _, m := range all {
		if m.Content == "Production runs in region fsn1 behind Cloudflare" || m.Content == "Cloudflare fronts the production region fsn1" {
			t.Errorf("a merged-away input survived beside its own merge: %q", m.Content)
		}
	}
}

// TestMergeCandidateReachesGlobalUnderPromoteGlobals covers the other half of the
// promotion path. A merge is the only thing an LLM tier can offer it now, since
// the harness states no scope: the text is classified, a global candidate is
// routed to globalMems, and --promote-globals is what actually writes it to
// _global. Without this, the flag has nothing to promote from a harness-backed
// run and the whole cross-repo half of the corpus stays project-scoped.
func TestMergeCandidateReachesGlobalUnderPromoteGlobals(t *testing.T) {
	store, ctx := newReflectStore(t, "proj")
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "convention", Content: "tabs, not spaces, in every repository we touch", Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "convention", Content: "run the linter before pushing from any repo", Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	preExisting, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the ledger ingests through the bastion on port 2222", Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One merge of the two conventions, and the unrelated fact passed through.
	// Ids are looked up by content rather than by position: GetAll orders by
	// importance and created_at, and these rows are created in the same second.
	// The merge text restates both sources' words, which is what the prompt asks
	// for — a merge that summarised one away would have that source re-added.
	result := consolidateForTest(t, reflectInputFor(t, store, "proj"), func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["merge %s,%s -> across all repos, use tabs not spaces in every repository we touch, and run the linter before pushing"]}`,
			idByContent(t, store, "proj", "tabs, not spaces"), idByContent(t, store, "proj", "run the linter"))
	})

	var projectMems, globalMems []reflection.ReflectMemory
	projectMems, globalMems = splitByScope(result.Memories)
	if len(globalMems) != 1 {
		t.Fatalf("global candidates = %d, want the one cross-repo merge: %+v", len(globalMems), result.Memories)
	}
	if len(projectMems) != 1 || projectMems[0].Content != "the ledger ingests through the bastion on port 2222" {
		t.Fatalf("the passed-through row did not stay project-scoped: %+v", projectMems)
	}

	_, promoted, kept, err := applyReflection(ctx, store, "proj", projectMems, globalMems, "", true)
	if err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	if promoted != 1 || len(kept) != 0 {
		t.Fatalf("promotion = (%d, %d kept), want (1, 0)", promoted, len(kept))
	}

	globals, err := store.GetAll(ctx, "_global", -1)
	if err != nil {
		t.Fatalf("GetAll(_global): %v", err)
	}
	if len(globals) != 1 || globals[0].Content != globalMems[0].Content {
		t.Fatalf("_global holds %d rows, want the promoted merge: %+v", len(globals), globals)
	}
	// The pass-through stayed in the project, and the merged inputs did not
	// reappear there.
	all, err := store.GetAll(ctx, "proj", -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 || all[0].ID != preExisting {
		t.Fatalf("project holds %+v, want only the passed-through row %s", all, preExisting)
	}
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

// mustGetOne reads the stored row by id, so the assertion can compare fields the
// list queries do not carry.
func mustGetOne(t *testing.T, store *memory.Store, id string) memory.Memory {
	t.Helper()
	rows, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs(%s): %v", id, err)
	}
	if len(rows) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows, want 1", id, len(rows))
	}
	return rows[0]
}
