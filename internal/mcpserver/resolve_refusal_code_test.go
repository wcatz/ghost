package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A refusal that the caller's own argument caused (a project name that matches
// more than one project, a subscribe URI that is not ghost://) must reach the
// wire as -32602, while a failed store read stays a plain error. These tests
// read the code back the way a client sees it.

// ambiguousProjectSession returns a client session over a server that holds two
// projects answering to the name "dup", and the db behind it.
func ambiguousProjectSession(t *testing.T) (*mcp.ClientSession, func()) {
	t.Helper()
	db, store, srv := newStoreWithDB(t)
	ctx := context.Background()
	for _, p := range []struct{ id, path string }{
		{"dup-one", "/srv/one/dup"},
		{"dup-two", "/srv/two/dup"},
	} {
		if err := store.EnsureProject(ctx, p.id, p.path, "dup"); err != nil {
			t.Fatalf("EnsureProject(%q): %v", p.id, err)
		}
	}
	breakStore := func() {
		if _, err := db.ExecContext(ctx, `DROP TABLE projects`); err != nil {
			t.Fatalf("drop projects: %v", err)
		}
	}
	return connectedClient(t, srv), breakStore
}

func TestProjectContextResource_AmbiguousProjectIsInvalidParams(t *testing.T) {
	session, _ := ambiguousProjectSession(t)
	_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ghost://project/dup/context"})
	wire := wireErrorOf(t, "read context of dup", err)
	if wire.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want %d (invalid params): %q", wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
	}
	if !strings.Contains(wire.Message, "resolve project: ") || !strings.Contains(wire.Message, "ambiguous") {
		t.Errorf("message changed: %q", wire.Message)
	}
}

func TestRecallProject_AmbiguousProjectIsInvalidParams(t *testing.T) {
	session, _ := ambiguousProjectSession(t)
	_, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name: "recall_project", Arguments: map[string]string{"project_id": "dup"},
	})
	wire := wireErrorOf(t, "recall_project dup", err)
	if wire.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want %d (invalid params): %q", wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
	}
	if !strings.Contains(wire.Message, "resolve project: ") || !strings.Contains(wire.Message, "ambiguous") {
		t.Errorf("message changed: %q", wire.Message)
	}
}

// The control: a store failure during resolve is the server's fault, so it is
// not reported as a bad argument.
func TestProjectRefusals_StoreFailureIsNotInvalidParams(t *testing.T) {
	session, breakStore := ambiguousProjectSession(t)
	breakStore()
	ctx := context.Background()

	_, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/dup/context"})
	if err == nil {
		t.Fatal("read context with a broken store: got success")
	}
	if wire := wireErrorOf(t, "read context", err); wire.Code == jsonrpc.CodeInvalidParams {
		t.Errorf("resource: store failure answered -32602: %q", wire.Message)
	}

	_, err = session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name: "recall_project", Arguments: map[string]string{"project_id": "dup"},
	})
	if err == nil {
		t.Fatal("recall_project with a broken store: got success")
	}
	if wire := wireErrorOf(t, "recall_project", err); wire.Code == jsonrpc.CodeInvalidParams {
		t.Errorf("recall_project: store failure answered -32602: %q", wire.Message)
	}
}

// rawCall writes one request frame on conn and returns the response that
// answers it, skipping any notification the server sends in between.
func rawCall(t *testing.T, conn mcp.Connection, id int64, method string, params any) *jsonrpc.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	rid, err := jsonrpc.MakeID(float64(id))
	if err != nil {
		t.Fatalf("MakeID: %v", err)
	}
	if err := conn.Write(ctx, &jsonrpc.Request{ID: rid, Method: method, Params: raw}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	for {
		msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read reply to %s: %v", method, err)
		}
		if resp, ok := msg.(*jsonrpc.Response); ok && resp.ID == rid {
			return resp
		}
	}
}

// ClientSession.Subscribe cannot show this refusal under the newer protocol
// (it is fire-and-forget there), so the frame is sent by hand on a connection
// initialised with the older one.
func TestResourcesSubscribe_NonGhostURIIsInvalidParams(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	conn, err := clientTransport.Connect(ctx)
	if err != nil {
		t.Fatalf("client transport Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	init := rawCall(t, conn, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "raw", "version": "0"},
	})
	if init.Error != nil {
		t.Fatalf("initialize: %v", init.Error)
	}
	if err := conn.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("write initialized: %v", err)
	}

	resp := rawCall(t, conn, 2, "resources/subscribe", map[string]any{"uri": "http://example.com/x"})
	var wire *jsonrpc.Error
	if !errors.As(resp.Error, &wire) || wire == nil {
		t.Fatalf("subscribe reply %+v carries no JSON-RPC error", resp)
	}
	if wire.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want %d (invalid params): %q", wire.Code, jsonrpc.CodeInvalidParams, wire.Message)
	}
	if !strings.Contains(wire.Message, "unknown resource URI") {
		t.Errorf("message changed: %q", wire.Message)
	}
}
