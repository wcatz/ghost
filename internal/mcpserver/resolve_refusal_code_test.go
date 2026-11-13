package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ambiguousProjectSession(t *testing.T) (*mcp.ClientSession, func()) {
	t.Helper()
	db, store, srv := newStoreWithDB(t)
	ctx := context.Background()
	for _, p := range []struct{ id, path string }{{"dup-one", "/srv/one/dup"}, {"dup-two", "/srv/two/dup"}} {
		if err := store.EnsureProject(ctx, p.id, p.path, "dup"); err != nil {
			t.Fatalf("EnsureProject(%q): %v", p.id, err)
		}
	}
	return connectedClient(t, srv), func() {
		if _, err := db.ExecContext(ctx, `DROP TABLE projects`); err != nil {
			t.Fatalf("drop projects: %v", err)
		}
	}
}

func TestAmbiguousProjectRefusalsAreInvalidParams(t *testing.T) {
	session, _ := ambiguousProjectSession(t)
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		wire := wireErrorOf(t, what, err)
		if wire.Code != jsonrpc.CodeInvalidParams || !strings.Contains(wire.Message, "resolve project: ") || !strings.Contains(wire.Message, "ambiguous") {
			t.Errorf("%s: code=%d msg=%q", what, wire.Code, wire.Message)
		}
	}
	for _, tail := range []string{"context", "decisions", "tasks"} {
		_, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/dup/" + tail})
		check("read "+tail, err)
	}
	_, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "recall_project", Arguments: map[string]string{"project_id": "dup"}})
	check("recall_project", err)
}

func TestProjectRefusals_StoreFailureIsNotInvalidParams(t *testing.T) {
	session, breakStore := ambiguousProjectSession(t)
	breakStore()
	ctx := context.Background()
	for _, tail := range []string{"context", "decisions", "tasks"} {
		_, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/dup/" + tail})
		if err == nil {
			t.Fatalf("read %s with a broken store: got success", tail)
		}
		if wire := wireErrorOf(t, "read "+tail, err); wire.Code == jsonrpc.CodeInvalidParams {
			t.Errorf("resource %s: store failure answered -32602: %q", tail, wire.Message)
		}
	}
	_, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "recall_project", Arguments: map[string]string{"project_id": "dup"}})
	if err == nil {
		t.Fatal("recall_project with a broken store: got success")
	}
	if wire := wireErrorOf(t, "recall_project", err); wire.Code == jsonrpc.CodeInvalidParams {
		t.Errorf("recall_project: store failure answered -32602: %q", wire.Message)
	}
}
