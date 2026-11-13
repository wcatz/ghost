package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func readResourceWireError(t *testing.T, session *mcp.ClientSession, uri string) *jsonrpc.Error {
	t.Helper()
	_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err == nil {
		t.Fatalf("ReadResource(%q) succeeded; want an error", uri)
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("ReadResource(%q) error %v (%T) is not a JSON-RPC error", uri, err, err)
	}
	return wire
}

func TestResourceReadURIWithheld(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 40)
	_, session := newCapSession(t)
	for _, uri := range []string{
		"ghost://project/" + token + "%zz/tasks",
		"https://x-access-token:" + token + "@github.com/o/r.git",
		"ghost://project/https%3A%2F%2F" + token + "%40github.com%2Fo%2Fr%zz/tasks",
	} {
		wire := readResourceWireError(t, session, uri)
		if wire.Code != jsonrpc.CodeInvalidParams || strings.Contains(wire.Message, "ghp_") || strings.Contains(string(wire.Data), "ghp_") || len(wire.Data) != 0 {
			t.Errorf("uri %q leaked token: code=%d msg=%q data=%s", uri, wire.Code, wire.Message, wire.Data)
		}
	}
}

func TestResourceReadURIWithheldUntouched(t *testing.T) {
	_, session := newCapSession(t)
	uri := "ghost://project/plain%zz/tasks"
	wire := readResourceWireError(t, session, uri)
	if wire.Code != jsonrpc.CodeInvalidParams || wire.Message != "Resource not found" || string(wire.Data) != `{"uri":"`+uri+`"}` {
		t.Errorf("unexpected error: (%d, %q, %s)", wire.Code, wire.Message, wire.Data)
	}
}

func TestResourceReadURIWithheldRoutedRefusal(t *testing.T) {
	_, session := newCapSession(t)
	token := "ghp_" + strings.Repeat("b", 40)
	for _, tail := range []string{"tasks", "decisions"} {
		_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ghost://project/" + token + "/" + tail})
		if err == nil {
			t.Fatalf("%s: read of an unregistered project succeeded", tail)
		}
		text := err.Error()
		var wire *jsonrpc.Error
		if errors.As(err, &wire) {
			text += string(wire.Data)
		}
		if strings.Contains(text, "ghp_") {
			t.Errorf("%s: refusal repeats the token: %q", tail, text)
		}
	}
}

func TestResourceReadURIWithheldMessage(t *testing.T) {
	token := "ghp_" + strings.Repeat("c", 40)
	read := func(uri string) (mcp.Result, error) {
		next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return nil, fmt.Errorf("reading resource %s: %w", uri, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "inner"})
		}
		req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: uri}}
		return withholdResourceReadURI(next)(context.Background(), "resources/read", req)
	}

	_, err := read("ghost://project/" + token + "/tasks")
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error %v (%T) is not a JSON-RPC error", err, err)
	}
	if strings.Contains(wire.Message, "ghp_") || len(wire.Data) != 0 {
		t.Errorf("error repeats the token: message %q data %s", wire.Message, wire.Data)
	}
	if !strings.Contains(wire.Message, "resource URI withheld") || wire.Code != jsonrpc.CodeInternalError {
		t.Errorf("error = (%d, %q), want the placeholder and the wrapped code", wire.Code, wire.Message)
	}

	quotedURI := "ghost://project/" + token + `"x%zz/tasks`
	quoted := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, fmt.Errorf("parse %q: invalid URL escape", quotedURI)
	}
	quotedReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: quotedURI}}
	if _, err := withholdResourceReadURI(quoted)(context.Background(), "resources/read", quotedReq); err == nil {
		t.Errorf("quoted-URI error was dropped")
	} else if strings.Contains(err.Error(), "ghp_") {
		t.Errorf("quoted-URI message repeats the token: %q", err.Error())
	}

	plain := "ghost://project/plain/tasks"
	if _, err := read(plain); err == nil || !strings.Contains(err.Error(), plain) {
		t.Errorf("error for a URI with no token = %v, want it untouched", err)
	}
}
