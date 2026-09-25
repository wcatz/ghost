package mcpserver

import (
	"strings"
	"testing"
)

// TestSaveDoesNotClaimCrossScopeDuplicate reproduces the user-visible
// symptom this fix exists for. Saving the production claim after the
// development one reported:
//
//	Memory saved (id: ...), linked as a likely duplicate of ... (score 0.60)
//
// Two claims about two environments, near-identical in wording, announced
// as duplicates of each other. The fold kept both rows, so retrieval still
// worked — but it wrote a duplicate edge between facts that were never one
// claim, and the duplicate penalty then sank one behind a partner it does
// not belong to.
//
// Text similarity cannot be the one to notice this: "development ... SQLite"
// and "production ... PostgreSQL" share their sentence structure. Scope is
// the only signal that says they were never the same claim.
func TestSaveDoesNotClaimCrossScopeDuplicate(t *testing.T) {
	_, session := newCapSession(t)

	devFirst := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "The project database for development is SQLite.",
		"category":   "fact",
		"scope":      map[string]any{"environment": "development"},
	})
	if devFirst.IsError {
		t.Fatalf("development save failed: %s", resultText(devFirst))
	}

	prodSecond := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "The project database for production is PostgreSQL.",
		"category":   "fact",
		"scope":      map[string]any{"environment": "production"},
	})
	if prodSecond.IsError {
		t.Fatalf("production save failed: %s", resultText(prodSecond))
	}
	if out := resultText(prodSecond); strings.Contains(out, "linked as a likely duplicate") {
		t.Errorf("a production memory was announced as a duplicate of a development one — two environments are not two wordings of one claim:\n%s", out)
	}

	// Neither direction. A conflict that only blocked one save would be
	// reinstated by the next one arriving from the other side.
	//
	// Folding into its own original row is correct and expected — identical
	// content in identical scope is what dedup exists to absorb — so the
	// target is what matters, not whether a fold happened at all.
	devAgain := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "The project database for development is SQLite.",
		"category":   "fact",
		"scope":      map[string]any{"environment": "development"},
	})
	if devAgain.IsError {
		t.Fatalf("development re-save failed: %s", resultText(devAgain))
	}
	if out := resultText(devAgain); strings.Contains(out, "PostgreSQL") {
		t.Errorf("re-saving the development claim reached for the production row:\n%s", out)
	}

	// Exempting a fold target must block a relation, never a row: both
	// memories still exist and remain findable under their own scope.
	if out := searchScoped(t, session, "database", map[string]any{"environment": "production"}); !strings.Contains(out, "PostgreSQL") {
		t.Errorf("production memory is not retrievable under production scope:\n%s", out)
	}
	if out := searchScoped(t, session, "database", map[string]any{"environment": "development"}); !strings.Contains(out, "for development is SQLite") {
		t.Errorf("development memory is not retrievable under development scope:\n%s", out)
	}
}
