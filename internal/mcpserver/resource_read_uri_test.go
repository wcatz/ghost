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

// readResourceWireError reads uri over a real client session and returns the
// JSON-RPC error the client received. Data is read off the wire error itself:
// its Error() is the message alone, so asserting on err.Error() would pass with
// data.uri still carrying the token.
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

// TestResourceReadURIWithheld covers the URIs that match no registered resource
// or template, so the SDK answers them itself: a token-shaped segment in one must
// not come back in the error's message or data.
func TestResourceReadURIWithheld(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 40)
	cases := []struct {
		name string
		uri  string
	}{
		{"bad escape after a token segment", "ghost://project/" + token + "%zz/tasks"},
		{"unknown scheme carrying a token", "https://x-access-token:" + token + "@github.com/o/r.git"},
		{"percent-encoded clone URL with a bad escape", "ghost://project/https%3A%2F%2F" + token + "%40github.com%2Fo%2Fr%zz/tasks"},
	}
	_, session := newCapSession(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := readResourceWireError(t, session, tc.uri)
			if wire.Code != jsonrpc.CodeInvalidParams {
				t.Errorf("code = %d, want %d (the not-found code must survive)", wire.Code, jsonrpc.CodeInvalidParams)
			}
			for what, text := range map[string]string{"message": wire.Message, "data": string(wire.Data)} {
				if strings.Contains(text, "ghp_") {
					t.Errorf("error %s repeats the token: %q", what, text)
				}
			}
			if len(wire.Data) != 0 {
				t.Errorf("data = %s, want it omitted for a withheld URI", wire.Data)
			}
		})
	}
}

// TestResourceReadURIWithheldUntouched is the positive control: an unroutable URI
// with no token keeps the SDK's data.uri, which also proves the client sees Data
// at all, so the withheld cases above cannot pass for want of it.
func TestResourceReadURIWithheldUntouched(t *testing.T) {
	_, session := newCapSession(t)
	uri := "ghost://project/plain%zz/tasks"
	wire := readResourceWireError(t, session, uri)
	if wire.Code != jsonrpc.CodeInvalidParams || wire.Message != "Resource not found" {
		t.Errorf("error = (%d, %q), want the SDK's own not-found", wire.Code, wire.Message)
	}
	if want := `{"uri":"` + uri + `"}`; string(wire.Data) != want {
		t.Errorf("data = %s, want %s", wire.Data, want)
	}
}

// TestResourceReadURIWithheldRoutedRefusal: a URI that does route reaches Ghost's
// own handler, whose refusal already withholds the project id; the middleware must
// leave it withheld rather than add the URI back.
func TestResourceReadURIWithheldRoutedRefusal(t *testing.T) {
	_, session := newCapSession(t)
	token := "ghp_" + strings.Repeat("b", 40)
	for _, tail := range []string{"tasks", "decisions"} {
		_, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
			URI: "ghost://project/" + token + "/" + tail,
		})
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

// TestResourceReadURIWithheldMessage drives the middleware directly with an error
// whose message carries the URI, the shape of the SDK's own `reading resource %s`
// errors: the copy in the message is replaced, the wrapped code survives, and an
// error for a URI with no token is returned as it came.
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

	// A message that quotes the URI, the shape of url.Parse's own errors. The URI
	// holds a '"', so its %q form escapes it and the raw URI is not a substring of
	// the message: only the quoted-form replacement can withhold this copy.
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
