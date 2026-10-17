//go:build !windows

package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProjectContextNamesANonRegularImportEntryAsSkipped: a symlink named *.md
// in the Claude memory directory is refused unopened by the first-contact
// import, and the answer names it under its own group. It is not worded as a
// failed write a later run retries, because no retry can make a link a file.
func TestProjectContextNamesANonRegularImportEntryAsSkipped(t *testing.T) {
	srv, _, memDir := importNotifyServer(t)
	plantClaudeMemory(t, memDir, "clean-note.md", "A plain project note with nothing sensitive in it.")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("text from a file outside the claude memory directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(memDir, "a_link.md")); err != nil {
		t.Fatal(err)
	}
	session, _ := subscribeTestClient(t, srv)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_project_context",
		Arguments: map[string]any{"project_id": importNotifyName},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_context: %v", err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	got := text.String()
	const want = "Claude memory import skipped files (not a regular file; never read): " + "«a_link.md»"
	if !strings.Contains(got, want) {
		t.Errorf("the answer does not name the link as skipped (want %q):\n%s", want, got)
	}
	if strings.Contains(got, "could not store") || strings.Contains(got, "token guard") {
		t.Errorf("the link is worded as a failed or refused write:\n%s", got)
	}
	if strings.Contains(got, "outside the claude memory directory") {
		t.Errorf("the link's target reached the answer:\n%s", got)
	}
}
