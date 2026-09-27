package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// TestApplyReflectionKeepsARowsThatWereCarriedThroughUnchanged is the
// store-side half of the per-id contract (#639). A "keep" operation emits the
// stored row's own content, category, importance and tags, and this asserts
// what that buys at the write boundary: the row is updated in place, so its id
// — and with it the embedding and link graph that cascade on delete — survives,
// along with its age and the source that recorded where it came from. The
// defect this replaces was a retyped memory arriving as a fresh id, which
// measured 12 of 13 new ids in one benchmarked project and 15 of 17 in another.
//
// Against a real store rather than a fake, because the failure mode is a
// cascade-deleted embedding and a re-stamped created_at, which only a database
// can show.
func TestApplyReflectionKeepsARowsThatWereCarriedThroughUnchanged(t *testing.T) {
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "reflect.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj", "/tmp/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Two rows that are carried through unchanged, and two that are folded
	// together. The link joins the two survivors, so its survival is the kept
	// rows' own doing — a link to a folded-away row would cascade on that row's
	// delete and prove nothing about identity.
	kept, err := store.Create(ctx, "proj", memory.Memory{
		Category: "gotcha", Content: "SSH to the bastion goes through port 2222, not 22",
		Importance: 0.9, Tags: []string{"ssh", "port"}, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	neighbour, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "the bastion is the only host that reaches the ledger",
		Importance: 0.6, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "Production runs in region fsn1 behind Cloudflare",
		Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(ctx, "proj", memory.Memory{
		Category: "fact", Content: "Cloudflare fronts the production region fsn1",
		Importance: 0.4, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.StoreEmbedding(ctx, kept, []float32{0.1, 0.2, 0.3}, "test-model:3"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := store.CreateLink(ctx, kept, neighbour, "related", 0.7, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	before := mustGetOne(t, store, kept)

	// The shape the ops contract produces for "keep <id>" twice over plus a merge
	// of the other two: byte-identical rows, and one new merged row.
	proposal := []reflection.ReflectMemory{
		{Category: before.Category, Content: before.Content, Importance: before.Importance, Tags: before.Tags},
		{Category: "fact", Content: "the bastion is the only host that reaches the ledger", Importance: 0.6},
		{Category: "fact", Content: "Production in region fsn1 is fronted by Cloudflare", Importance: 0.5},
	}
	if _, _, _, err := applyReflection(ctx, store, "proj", proposal, nil, "", false); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}

	after := mustGetOne(t, store, kept)
	if after.Content != before.Content || after.Category != before.Category {
		t.Errorf("the kept row was retyped: %+v", after)
	}
	if after.CreatedAt != before.CreatedAt {
		t.Errorf("created_at = %q, want the stored %q — a carried-through memory is not refreshed knowledge",
			after.CreatedAt, before.CreatedAt)
	}
	if after.Source != "mcp" {
		t.Errorf("source = %q, want mcp — consolidation must not relabel where a memory came from", after.Source)
	}
	if vec, err := store.GetEmbedding(ctx, kept); err != nil || len(vec) == 0 {
		t.Errorf("the kept row lost its embedding (err=%v, len=%d)", err, len(vec))
	}
	links, err := store.GetLinks(ctx, kept)
	if err != nil || len(links) != 1 || links[0].Relation != "related" {
		t.Errorf("the kept row lost its links (err=%v, links=%+v)", err, links)
	}
	if _, err := store.GetMemoryContent(ctx, neighbour); err != nil {
		t.Errorf("the second kept row was deleted: %v", err)
	}

	// The two folded inputs are gone and the merge is a new row, which is what a
	// merge costs and what a keep does not.
	all, err := store.GetAll(ctx, "proj", -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("project holds %d memories, want 2 kept + 1 merged: %+v", len(all), all)
	}
	for _, m := range all {
		if m.Content == "Production runs in region fsn1 behind Cloudflare" || m.Content == "Cloudflare fronts the production region fsn1" {
			t.Errorf("a merged-away input survived beside its own merge: %q", m.Content)
		}
	}
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
