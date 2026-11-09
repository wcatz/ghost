package memory

import (
	"context"
	"strings"
	"testing"
)

func TestMoveMemoryRoundTripKeepsIdentityLinksHistoryAndSearch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	globalID, err := s.Create(ctx, GlobalProjectID, Memory{
		Category: "fact", Content: "portable mover memory about relay ports", Source: "mcp", Importance: 0.6, Tags: []string{"move"},
	})
	if err != nil {
		t.Fatalf("Create global: %v", err)
	}
	linkedID, err := s.Create(ctx, GlobalProjectID, Memory{
		Category: "fact", Content: "linked context memory", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create linked: %v", err)
	}
	if err := s.CreateLink(ctx, globalID, linkedID, "related", 0.9, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	vec := make([]float32, 8)
	vec[0] = 1
	if err := s.StoreEmbedding(ctx, globalID, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	dest := "move-dest-project"
	if err := s.EnsureProject(ctx, dest, "/tmp/"+dest, dest); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	r1, err := s.MoveMemory(ctx, GlobalProjectID, globalID, dest, MoveMemoryOptions{})
	if err != nil {
		t.Fatalf("Move global->project: %v", err)
	}
	if r1.FromProjectID != GlobalProjectID || r1.ToProjectID != dest || r1.MemoryID != globalID || !r1.Moved {
		t.Fatalf("first move result = %+v", r1)
	}
	mem, err := s.GetByIDs(ctx, []string{globalID})
	if err != nil || len(mem) != 1 {
		t.Fatalf("GetByIDs after first move: %v n=%d", err, len(mem))
	}
	if mem[0].ID != globalID || mem[0].ProjectID != dest {
		t.Fatalf("after global->project: id=%q project=%q", mem[0].ID, mem[0].ProjectID)
	}
	links, err := s.GetLinks(ctx, globalID)
	if err != nil {
		t.Fatalf("GetLinks after first move: %v", err)
	}
	if len(links) != 1 || linkOtherEnd(links[0], globalID) != linkedID {
		t.Fatalf("links after first move = %+v, want other end %s", links, linkedID)
	}
	if _, err := s.GetEmbedding(ctx, globalID); err != nil {
		t.Fatalf("embedding should survive first move: %v", err)
	}
	inDest, err := s.SearchFTS(ctx, dest, "portable", 10)
	if err != nil {
		t.Fatalf("SearchFTS dest: %v", err)
	}
	if !containsMemoryID(inDest, globalID) {
		t.Fatalf("dest search missing moved memory: %+v", inDest)
	}
	inGlobal, err := s.SearchFTS(ctx, GlobalProjectID, "portable", 10)
	if err != nil {
		t.Fatalf("SearchFTS global: %v", err)
	}
	if containsMemoryID(inGlobal, globalID) {
		t.Fatalf("global search still returned moved memory: %+v", inGlobal)
	}

	r2, err := s.MoveMemory(ctx, dest, globalID, GlobalProjectID, MoveMemoryOptions{})
	if err != nil {
		t.Fatalf("Move project->global: %v", err)
	}
	if r2.FromProjectID != dest || r2.ToProjectID != GlobalProjectID || !r2.Moved {
		t.Fatalf("second move result = %+v", r2)
	}
	mem, err = s.GetByIDs(ctx, []string{globalID})
	if err != nil || len(mem) != 1 || mem[0].ProjectID != GlobalProjectID {
		t.Fatalf("after project->global: %v %+v", err, mem)
	}
	links, err = s.GetLinks(ctx, globalID)
	if err != nil || len(links) != 1 || linkOtherEnd(links[0], globalID) != linkedID {
		t.Fatalf("links after round trip: %v %+v, want other end %s", err, links, linkedID)
	}
	inGlobal, err = s.SearchFTS(ctx, GlobalProjectID, "portable", 10)
	if err != nil {
		t.Fatalf("SearchFTS global after return: %v", err)
	}
	if !containsMemoryID(inGlobal, globalID) {
		t.Fatalf("global search missing returned memory: %+v", inGlobal)
	}
	inDest, err = s.SearchFTS(ctx, dest, "portable", 10)
	if err != nil {
		t.Fatalf("SearchFTS dest after return: %v", err)
	}
	for _, m := range inDest {
		if m.ID == globalID && m.ProjectID == dest {
			t.Fatalf("dest bucket still returned memory after return: %+v", inDest)
		}
	}

	history, err := s.MemoryHistory(ctx, globalID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var moves []HistoryEntry
	for _, h := range history {
		if h.Phase == "move" {
			moves = append(moves, h)
		}
	}
	if len(moves) != 2 {
		t.Fatalf("move history rows = %d, want 2: %+v", len(moves), history)
	}
	if moves[0].FromProjectID != GlobalProjectID || moves[0].ToProjectID != dest {
		t.Errorf("first move row = %+v", moves[0])
	}
	if moves[1].FromProjectID != dest || moves[1].ToProjectID != GlobalProjectID {
		t.Errorf("second move row = %+v", moves[1])
	}
}

func containsMemoryID(mems []Memory, id string) bool {
	for _, m := range mems {
		if m.ID == id {
			return true
		}
	}
	return false
}

func linkOtherEnd(l Link, id string) string {
	if l.SourceID == id {
		return l.TargetID
	}
	return l.SourceID
}

func TestMoveMemoryRefusalsAndDryRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	dest := "move-refusal-dest"
	if err := s.EnsureProject(ctx, dest, "/tmp/"+dest, dest); err != nil {
		t.Fatalf("EnsureProject dest: %v", err)
	}
	id, err := s.Create(ctx, dest, Memory{Category: "fact", Content: "refusal subject", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.MoveMemory(ctx, dest, id, "no-such-project", MoveMemoryOptions{}); err == nil {
		t.Fatal("expected unknown target refusal")
	}
	if _, err := s.MoveMemory(ctx, "wrong-project", id, GlobalProjectID, MoveMemoryOptions{}); err == nil {
		t.Fatal("expected wrong project refusal")
	}

	dry, err := s.MoveMemory(ctx, dest, id, GlobalProjectID, MoveMemoryOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Moved || !dry.DryRun || dry.ToProjectID != GlobalProjectID {
		t.Fatalf("dry run result = %+v", dry)
	}
	mem, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(mem) != 1 || mem[0].ProjectID != dest {
		t.Fatalf("dry run should not move: %v %+v", err, mem)
	}
	history, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, h := range history {
		if h.Phase == "move" {
			t.Fatalf("dry run should not record move history: %+v", history)
		}
	}
}

func TestMoveMemoryPinnedAndPersistentRequireConfirm(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	dest := "move-pin-dest"
	if err := s.EnsureProject(ctx, dest, "/tmp/"+dest, dest); err != nil {
		t.Fatalf("EnsureProject dest: %v", err)
	}

	pinnedID, err := s.Create(ctx, dest, Memory{Category: "fact", Content: "pinned move subject", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create pinned: %v", err)
	}
	if err := s.TogglePin(ctx, pinnedID, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	if _, err := s.MoveMemory(ctx, dest, pinnedID, GlobalProjectID, MoveMemoryOptions{}); err == nil {
		t.Fatal("expected pinned refusal")
	}
	if _, err := s.MoveMemory(ctx, dest, pinnedID, GlobalProjectID, MoveMemoryOptions{Confirm: true}); err != nil {
		t.Fatalf("confirmed pinned move: %v", err)
	}

	persistentID, _, _, err := s.UpsertWithOptions(ctx, dest, "fact", "persistent move subject", "mcp", 0.5, nil, UpsertOptions{Retention: RetentionPersistent})
	if err != nil {
		t.Fatalf("Create persistent: %v", err)
	}
	other := "move-pin-other"
	if err := s.EnsureProject(ctx, other, "/tmp/"+other, other); err != nil {
		t.Fatalf("EnsureProject other: %v", err)
	}
	if _, err := s.MoveMemory(ctx, dest, persistentID, other, MoveMemoryOptions{}); err == nil {
		t.Fatal("expected persistent refusal")
	}
	if _, err := s.MoveMemory(ctx, dest, persistentID, other, MoveMemoryOptions{Confirm: true}); err != nil {
		t.Fatalf("confirmed persistent move: %v", err)
	}
}

// TestResolveMoveTargetNamesTheCallersArgument pins that a withheld unknown
// target names the argument the caller sent: "to_project" for the MCP tool,
// "to" for the CLI flag. Before the parameterisation the helper hard-coded
// "to", so an MCP refusal for a credential-shaped or over-256-byte
// to_project read "<to withheld: ...>" instead of "<to_project withheld: ...>".
func TestResolveMoveTargetNamesTheCallersArgument(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	withheldShapes := []string{
		"ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd",
		strings.Repeat("x", ArgEchoMaxBytes+1),
	}
	for _, to := range withheldShapes {
		_, err := s.ResolveMoveTarget(ctx, "to_project", to)
		if err == nil {
			t.Fatalf("ResolveMoveTarget(to_project, %d bytes) succeeded for an unknown target", len(to))
		}
		if !strings.Contains(err.Error(), "to_project withheld") {
			t.Errorf("MCP refusal = %q, want it to name to_project as withheld", err.Error())
		}
		if strings.Contains(err.Error(), "<to withheld") || strings.Contains(err.Error(), "\"to withheld") {
			t.Errorf("MCP refusal = %q, names the CLI flag instead of to_project", err.Error())
		}
	}

	_, err := s.ResolveMoveTarget(ctx, "to", "ghp_"+strings.Repeat("a1B2c3D4e5F6", 3)+"AbCd")
	if err == nil {
		t.Fatal("ResolveMoveTarget(to, ...) succeeded for an unknown target")
	}
	if !strings.Contains(err.Error(), "<to withheld") {
		t.Errorf("CLI refusal = %q, want it to name to as withheld", err.Error())
	}
	if strings.Contains(err.Error(), "to_project withheld") {
		t.Errorf("CLI refusal = %q, names the MCP argument instead of to", err.Error())
	}
}
