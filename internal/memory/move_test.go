package memory

import (
	"context"
	"strings"
	"testing"
)

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
