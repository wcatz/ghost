package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
)

func TestRegistry_RegisterAndExecute(t *testing.T) {
	r := NewRegistry()

	r.Register(
		ai.ToolDefinition{Name: "test_tool", Description: "a test"},
		func(ctx context.Context, projectPath string, input json.RawMessage) Result {
			return Result{Content: "executed:" + string(input)}
		},
		ApprovalNone,
	)

	result := r.Execute(context.Background(), "test_tool", "/tmp", json.RawMessage(`{"key":"val"}`))
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if result.Content != `executed:{"key":"val"}` {
		t.Errorf("content = %q", result.Content)
	}
	if result.Duration == 0 {
		t.Error("duration should be > 0")
	}
}

func TestRegistry_UnknownTool(t *testing.T) {
	r := NewRegistry()

	result := r.Execute(context.Background(), "nonexistent", "/tmp", nil)
	if !result.IsError {
		t.Fatal("expected error for unknown tool")
	}
	if result.Content != "unknown tool: nonexistent" {
		t.Errorf("content = %q", result.Content)
	}
}

func TestRegistry_Definitions(t *testing.T) {
	r := NewRegistry()

	r.Register(ai.ToolDefinition{Name: "tool_a", Description: "A"}, nil, ApprovalNone)
	r.Register(ai.ToolDefinition{Name: "tool_b", Description: "B"}, nil, ApprovalWarn)
	r.Register(ai.ToolDefinition{Name: "tool_c", Description: "C"}, nil, ApprovalRequire)

	defs := r.Definitions()
	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{"tool_a", "tool_b", "tool_c"} {
		if !names[want] {
			t.Errorf("missing definition for %q", want)
		}
	}
}

func TestRegistry_ApprovalLevels(t *testing.T) {
	r := NewRegistry()

	r.Register(ai.ToolDefinition{Name: "none_tool"}, nil, ApprovalNone)
	r.Register(ai.ToolDefinition{Name: "warn_tool"}, nil, ApprovalWarn)
	r.Register(ai.ToolDefinition{Name: "require_tool"}, nil, ApprovalRequire)

	tests := []struct {
		name string
		tool string
		want ApprovalLevel
	}{
		{"none level", "none_tool", ApprovalNone},
		{"warn level", "warn_tool", ApprovalWarn},
		{"require level", "require_tool", ApprovalRequire},
		{"unknown defaults to require", "unknown", ApprovalRequire},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.GetApprovalLevel(tt.tool)
			if got != tt.want {
				t.Errorf("GetApprovalLevel(%q) = %d, want %d", tt.tool, got, tt.want)
			}
		})
	}
}

func TestRegistry_OverwriteRegistration(t *testing.T) {
	r := NewRegistry()

	r.Register(
		ai.ToolDefinition{Name: "tool", Description: "v1"},
		func(ctx context.Context, projectPath string, input json.RawMessage) Result {
			return Result{Content: "v1"}
		},
		ApprovalNone,
	)
	r.Register(
		ai.ToolDefinition{Name: "tool", Description: "v2"},
		func(ctx context.Context, projectPath string, input json.RawMessage) Result {
			return Result{Content: "v2"}
		},
		ApprovalWarn,
	)

	result := r.Execute(context.Background(), "tool", "/tmp", nil)
	if result.Content != "v2" {
		t.Errorf("expected v2 after overwrite, got %q", result.Content)
	}
	if r.GetApprovalLevel("tool") != ApprovalWarn {
		t.Error("approval level should be updated to Warn")
	}
}

func TestRegisterAll_NineTools(t *testing.T) {
	store := testStoreForTool(t)
	r := NewRegistry()
	RegisterAll(r, store)

	defs := r.Definitions()
	if len(defs) != 9 {
		t.Fatalf("expected 9 tools, got %d", len(defs))
	}

	expected := map[string]ApprovalLevel{
		"file_read":     ApprovalNone,
		"grep":          ApprovalNone,
		"glob":          ApprovalNone,
		"git":           ApprovalWarn,
		"file_write":    ApprovalWarn,
		"file_edit":     ApprovalWarn,
		"bash":          ApprovalRequire,
		"memory_save":   ApprovalNone,
		"memory_search": ApprovalNone,
	}

	for name, wantLevel := range expected {
		gotLevel := r.GetApprovalLevel(name)
		if gotLevel != wantLevel {
			t.Errorf("%s: approval level = %d, want %d", name, gotLevel, wantLevel)
		}
	}
}
