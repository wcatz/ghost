package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests send raw JSON-RPC frames. The SDK client cannot show a refused
// subscription to a test: its Subscribe sends subscriptions/listen without
// waiting for the answer and records the URI as subscribed before sending, so
// a refusal never reaches the caller. Reading the connection directly is the
// only way to see what the server answered.

const (
	// wireFrameWait bounds a wait on a frame the server is expected to send.
	wireFrameWait = 10 * time.Second

	// wireLegacyProtocol is the newest protocol version go-sdk v1.8.0 still
	// serves through resources/subscribe. shared.go's negotiatedVersion caps
	// the initialize handshake below 2026-07-28, the first version that uses
	// subscriptions/listen, so 2025-11-25 is the newest legacy version.
	wireLegacyProtocol = "2025-11-25"

	// wireListenProtocol is the version a request names in its _meta to take
	// the subscriptions/listen path.
	wireListenProtocol = "2026-07-28"
)

// wireConn is a raw client connection to a Server with a reader goroutine, so
// every wait on a frame can carry a timeout.
type wireConn struct {
	t      *testing.T
	conn   mcp.Connection
	frames chan jsonrpc.Message
	nextID int
}

// newWireConn connects a raw connection to srv and completes the initialize
// handshake at the given protocol version.
func newWireConn(t *testing.T, srv *Server, protocol string) *wireConn {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	ss, err := srv.mcp.Connect(ctx, serverTransport, nil)
	if err != nil {
		cancel()
		t.Fatalf("server Connect: %v", err)
	}
	conn, err := clientTransport.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("client transport Connect: %v", err)
	}
	w := &wireConn{t: t, conn: conn, frames: make(chan jsonrpc.Message, 64)}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ss.Close()
		cancel()
	})
	go func() {
		defer close(w.frames)
		for {
			msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			select {
			case w.frames <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	res := w.call("initialize", map[string]any{
		"protocolVersion": protocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "wire-client", "version": "0"},
	})
	if res.Error != nil {
		t.Fatalf("initialize: %v", res.Error)
	}
	w.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return w
}

func (w *wireConn) send(frame map[string]any) {
	w.t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		w.t.Fatalf("marshal frame: %v", err)
	}
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		w.t.Fatalf("decode frame %s: %v", data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wireFrameWait)
	defer cancel()
	if err := w.conn.Write(ctx, msg); err != nil {
		w.t.Fatalf("write %s: %v", data, err)
	}
}

// start sends a request and returns its id without waiting for the answer.
func (w *wireConn) start(method string, params any) int {
	w.t.Helper()
	w.nextID++
	w.send(map[string]any{"jsonrpc": "2.0", "id": w.nextID, "method": method, "params": params})
	return w.nextID
}

// call sends a request and returns its response, discarding any notification
// that arrives before it.
func (w *wireConn) call(method string, params any) *jsonrpc.Response {
	w.t.Helper()
	res, _ := w.await(w.start(method, params))
	return res
}

// await reads frames until the response to id arrives, and returns it with the
// resources/updated URIs seen before it. Any other frame is skipped.
func (w *wireConn) await(id int) (*jsonrpc.Response, []string) {
	w.t.Helper()
	var updated []string
	timeout := time.After(wireFrameWait)
	for {
		select {
		case msg, ok := <-w.frames:
			if !ok {
				w.t.Fatalf("connection closed before the response to request %d", id)
			}
			switch m := msg.(type) {
			case *jsonrpc.Response:
				if fmt.Sprint(m.ID.Raw()) == fmt.Sprint(id) {
					return m, updated
				}
			case *jsonrpc.Request:
				if uri, ok := updatedURI(m); ok {
					updated = append(updated, uri)
				}
			}
		case <-timeout:
			w.t.Fatalf("no response to request %d within %s", id, wireFrameWait)
		}
	}
}

func updatedURI(req *jsonrpc.Request) (string, bool) {
	if req.Method != "notifications/resources/updated" {
		return "", false
	}
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return "", false
	}
	return p.URI, true
}

// listenParams builds subscriptions/listen params for the new protocol.
func listenParams(uris ...string) map[string]any {
	return map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    wireListenProtocol,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
		"notifications": map[string]any{"resourceSubscriptions": uris},
	}
}

// saveMemory drives the real ghost_memory_save handler over the wire and
// returns the resources/updated URIs that arrived before its response. The
// handler notifies before it returns, so a notification it sent to this
// connection is always ahead of the response. A refused save is a successful
// JSON-RPC response whose CallToolResult has isError set, so that is checked as
// well as the protocol error: a refused save notifies nobody, which would read
// as "no notification" to a caller asserting that none arrived.
func (w *wireConn) saveMemory(content string) []string {
	w.t.Helper()
	id := w.start("tools/call", map[string]any{
		"name":      "ghost_memory_save",
		"arguments": map[string]any{"project_id": "abc123", "content": content, "category": "fact"},
	})
	res, updated := w.await(id)
	if res.Error != nil {
		w.t.Fatalf("ghost_memory_save: %v", res.Error)
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res.Result, &result); err != nil {
		w.t.Fatalf("ghost_memory_save: decode result %s: %v", res.Result, err)
	}
	if result.IsError {
		w.t.Fatalf("ghost_memory_save was refused: %s", res.Result)
	}
	return updated
}

const wireContextURI = "ghost://project/abc123/context"

func TestSubscribeWire_ListenRefusesNonGhostURI(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	w := newWireConn(t, srv, wireListenProtocol)

	res := w.call("subscriptions/listen", listenParams("http://evil/x"))
	if res.Error == nil {
		t.Fatalf("subscriptions/listen for http://evil/x answered a result %s, want an error", res.Result)
	}
}

func TestSubscribeWire_ListenRefusesMixedURIsAsAWhole(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	w := newWireConn(t, srv, wireListenProtocol)

	res := w.call("subscriptions/listen", listenParams(wireContextURI, "file:///etc/hosts"))
	if res.Error == nil {
		t.Fatalf("mixed subscriptions/listen answered a result %s, want an error", res.Result)
	}

	// The valid URI must have been rolled back with the refusal: a project
	// write now reaches nobody.
	if updated := w.saveMemory("a write after a refused mixed listen"); len(updated) != 0 {
		t.Errorf("notified %v after a refused listen, want no notification", updated)
	}
}

func TestSubscribeWire_LegacySubscribeNotifiesOnProjectWrite(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	w := newWireConn(t, srv, wireLegacyProtocol)

	// resources/subscribe is a request with a response, so unlike the listen
	// path the registration is complete once it is answered.
	res := w.call("resources/subscribe", map[string]any{"uri": wireContextURI})
	if res.Error != nil {
		t.Fatalf("resources/subscribe: %v", res.Error)
	}

	// The handler notifies before it answers, so the notification is on the
	// wire ahead of the tool response saveMemory waits for.
	updated := w.saveMemory("a write after a legacy subscribe")
	if !slices.Contains(updated, wireContextURI) {
		t.Fatalf("notifications before the save response = %v, want one for %q", updated, wireContextURI)
	}
}

func TestSubscribeWire_LegacySubscribeRefusesNonGhostURI(t *testing.T) {
	_, _, srv := newStoreWithDB(t)
	w := newWireConn(t, srv, wireLegacyProtocol)

	res := w.call("resources/subscribe", map[string]any{"uri": "http://evil/x"})
	if res.Error == nil {
		t.Fatalf("resources/subscribe for http://evil/x answered a result %s, want an error", res.Result)
	}
}
