package mcpserver

import (
	"strings"
	"testing"
)

func TestMemoryDeletePurgesAnAlreadyDeletedMemory(t *testing.T) {
	_, session := newCapSession(t)

	// content, and the pre-#656 state this scenario is really about (a credential
	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "the zqxk marker was retired with its history kept",
		"category":   "gotcha",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok || id == "" {
		t.Fatalf("the save response carries no memory id: %q", resultText(res))
	}

	res = callTool(t, session, "ghost_memory_delete", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
	})
	if res.IsError {
		t.Fatalf("delete: %s", resultText(res))
	}

	res = callTool(t, session, "ghost_memory_delete", map[string]any{
		"project_id":    "test-project",
		"memory_id":     id,
		"purge_history": true,
	})
	if res.IsError {
		t.Fatalf("purge of a deleted memory: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "already deleted") {
		t.Errorf("the reply does not say the memory was already gone: %q", out)
	}
	if !strings.Contains(out, "not restored") {
		t.Errorf("the reply does not say the memory was not restored: %q", out)
	}

	hist := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "zqxk",
	})
	if !hist.IsError && strings.Contains(resultText(hist), "zqxk") {
		t.Errorf("the purged text is still searchable:\n%s", resultText(hist))
	}
}

func TestMemoryDeleteRefusesATombstoneWithoutPurge(t *testing.T) {
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "a fact that is retired, not erased",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save: %s", resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok || id == "" {
		t.Fatalf("the save response carries no memory id: %q", resultText(res))
	}
	if res := callTool(t, session, "ghost_memory_delete", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
	}); res.IsError {
		t.Fatalf("delete: %s", resultText(res))
	}

	res = callTool(t, session, "ghost_memory_delete", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
	})
	if !res.IsError {
		t.Fatalf("a second plain delete of a deleted memory reported success: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "not found") {
		t.Errorf("the refusal does not say what happened: %q", resultText(res))
	}
}
