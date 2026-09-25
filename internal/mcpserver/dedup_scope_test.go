package mcpserver

import (
	"strings"
	"testing"
)

// saveIDs extracts the id of the memory just written and, when a fold
// happened, the id it folded into.
//
// The response carries ids and a score and never the candidate's content, so
// asserting on content proves nothing about which row was chosen: a fold into
// the production row reads "linked as a likely duplicate of <prodID>" and
// contains no trace of "PostgreSQL". The target id is the only thing that
// answers the question.
func saveIDs(t *testing.T, out string) (id, duplicateOf string) {
	t.Helper()

	const idMark = "Memory saved (id: "
	i := strings.Index(out, idMark)
	if i < 0 {
		t.Fatalf("save result has no memory id: %q", out)
	}
	rest := out[i+len(idMark):]
	if j := strings.Index(rest, ")"); j >= 0 {
		id = rest[:j]
	} else {
		t.Fatalf("save result has a malformed id: %q", out)
	}

	const dupMark = "linked as a likely duplicate of "
	if k := strings.Index(out, dupMark); k >= 0 {
		rest = out[k+len(dupMark):]
		if j := strings.Index(rest, " (score"); j >= 0 {
			duplicateOf = rest[:j]
		} else {
			t.Fatalf("save result has a malformed duplicate id: %q", out)
		}
	}
	return id, duplicateOf
}

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
	devID, devDup := saveIDs(t, resultText(devFirst))
	if devDup != "" {
		t.Errorf("first save folded into %s with nothing to fold into", devDup)
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
	prodID, prodDup := saveIDs(t, resultText(prodSecond))
	if prodDup != "" {
		t.Errorf("a production memory folded into %s — two environments are not two wordings of one claim:\n%s",
			prodDup, resultText(prodSecond))
	}
	if prodID == devID {
		t.Fatal("both saves returned the same memory id")
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
	// The mirror, asserted on ids rather than content: a save result never
	// quotes the candidate's text, so a content-based check could not have
	// detected a fold into the production row at all — it would have passed
	// with the filter entirely broken.
	//
	// The fold must happen AND land on the development row. Requiring the
	// target rather than merely "not production" also catches a regression
	// where dedup stops folding altogether, which would silently reintroduce
	// the bloat this rule exists to avoid.
	_, againDup := saveIDs(t, resultText(devAgain))
	if againDup != devID {
		t.Errorf("re-saving the development claim folded into %q, want its own row %s — either it reached the production row or folding stopped working entirely:\n%s",
			againDup, devID, resultText(devAgain))
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
