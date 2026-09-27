package mcpserver

import (
	"strings"
	"testing"
)

// TestMemoryDeletePurgesAnAlreadyDeletedMemory: the second stage of a
// redaction, and the one a delete cannot perform. The handler resolves the
// project, then looks the id up in `memories` and refuses a miss — so an agent
// told to redact a credential whose memory it deleted an hour ago was told "not
// found", nothing was purged, and the text stayed in memory_history. The
// response text had promised that this tool was the equivalent of
// `ghost history purge`, which does reach it.
//
// The tombstone is the feature: the history outlives the row on purpose. So the
// lookup falls through to the history, which still names the project the row
// belonged to (the ownership check is not skipped for it), and the purge erases
// the text without restoring the row.
func TestMemoryDeletePurgesAnAlreadyDeletedMemory(t *testing.T) {
	_, session := newCapSession(t)

	// Plain text, not a credential, and the reason is worth stating: the subject
	// of this test is the TOMBSTONE FALL-THROUGH — the handler looks the id up in
	// `memories`, finds a miss, and reaches the history instead — which is the
	// same mechanism the next test exercises with ordinary content. What used to
	// make this one a credential test was incidental, and it is no longer
	// reachable from here at all: `ghost_memory_save` refuses credential-shaped
	// content, and the pre-#656 state this scenario is really about (a credential
	// already on disk, in the row AND in its history) can only be constructed from
	// inside internal/memory, where TestDeleteWithPurgeHistoryLeavesNothing and
	// TestPurgeReachesTheSnapshotThatCouldRestoreTheRow assert exactly that and
	// then purge it. Between them the two facts are covered; splitting them here
	// keeps this test testing the thing it is named for.
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

	// Retire it, keeping the history — the ordinary case that leaves a tombstone.
	res = callTool(t, session, "ghost_memory_delete", map[string]any{
		"project_id": "test-project",
		"memory_id":  id,
	})
	if res.IsError {
		t.Fatalf("delete: %s", resultText(res))
	}

	// Now the redaction. The row is gone; only the history has the text.
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
	// The row must NOT come back: this erases recorded text, it is not a restore.
	if !strings.Contains(out, "not restored") {
		t.Errorf("the reply does not say the memory was not restored: %q", out)
	}

	// And there is nothing left to read.
	hist := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "zqxk",
	})
	if !hist.IsError && strings.Contains(resultText(hist), "zqxk") {
		t.Errorf("the purged text is still searchable:\n%s", resultText(hist))
	}
}

// TestMemoryDeleteRefusesATombstoneWithoutPurge: the other half of the branch.
// A plain delete of something that does not exist is still "not found" — there is
// nothing to retire, and the caller may simply have the wrong id. Only a
// redaction reaches the history.
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
