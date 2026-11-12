package mcpserver

import (
	"strings"
	"testing"
)

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

	devAgain := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "The project database for development is SQLite.",
		"category":   "fact",
		"scope":      map[string]any{"environment": "development"},
	})
	if devAgain.IsError {
		t.Fatalf("development re-save failed: %s", resultText(devAgain))
	}
	_, againDup := saveIDs(t, resultText(devAgain))
	if againDup != devID {
		t.Errorf("re-saving the development claim folded into %q, want its own row %s — either it reached the production row or folding stopped working entirely:\n%s",
			againDup, devID, resultText(devAgain))
	}

	if out := searchScoped(t, session, "database", map[string]any{"environment": "production"}); !strings.Contains(out, "PostgreSQL") {
		t.Errorf("production memory is not retrievable under production scope:\n%s", out)
	}
	if out := searchScoped(t, session, "database", map[string]any{"environment": "development"}); !strings.Contains(out, "for development is SQLite") {
		t.Errorf("development memory is not retrievable under development scope:\n%s", out)
	}
}
