package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/ai"
)

// ApprovalLevel controls the safety gate for tool operations.
type ApprovalLevel int

const (
	ApprovalNone    ApprovalLevel = iota // execute immediately
	ApprovalWarn                         // show warning, auto-proceed
	ApprovalRequire                      // block until user confirms
)

// Result holds the output of a tool execution.
type Result struct {
	Content  string
	IsError  bool
	Duration time.Duration
	Metadata map[string]string // optional extra data (e.g. "diff" for file_edit)
}

// Executor runs a tool and returns the result.
type Executor func(ctx context.Context, projectPath string, input json.RawMessage) Result

type registeredTool struct {
	def      ai.ToolDefinition
	exec     Executor
	approval ApprovalLevel
}

// Registry manages tool definitions and executors.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registeredTool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]registeredTool),
	}
}

// Register adds a tool to the registry.
func (r *Registry) Register(def ai.ToolDefinition, exec Executor, approval ApprovalLevel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Name] = registeredTool{def: def, exec: exec, approval: approval}
}

// Definitions returns all tool definitions for the Claude API tools parameter.
func (r *Registry) Definitions() []ai.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]ai.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.def)
	}
	return defs
}

// Execute runs a tool by name and returns the result.
func (r *Registry) Execute(ctx context.Context, name, projectPath string, input json.RawMessage) Result {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()

	if !ok {
		return Result{Content: fmt.Sprintf("unknown tool: %s", name), IsError: true}
	}

	start := time.Now()
	result := t.exec(ctx, projectPath, input)
	result.Duration = time.Since(start)
	return result
}

// GetApprovalLevel returns the approval level for a tool.
func (r *Registry) GetApprovalLevel(name string) ApprovalLevel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools[name]; ok {
		return t.approval
	}
	return ApprovalRequire // default to most restrictive
}
