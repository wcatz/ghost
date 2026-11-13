package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

func TestParseProjectIDFromURI_DecodesSegmentOnce(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{"encoded slash names the whole id", "ghost://project/team%2Fapi/context", "team/api"},
		{"lowercase hex slash", "ghost://project/team%2fapi/tasks", "team/api"},
		{"path-shaped id", "ghost://project/%2Fsrv%2Fu%2Frepo/context", "/srv/u/repo"},
		{"encoded space", "ghost://project/my%20proj/decisions", "my proj"},
		{"double-encoded slash decodes once", "ghost://project/team%252Fapi/context", "team%2Fapi"},
		{"encoded slash beside a sub-delim", "ghost://project/a%2Fb%3Ac%40d/context", "a/b:c@d"},
		{"utf-8 id", "ghost://project/caf%C3%A9/context", "café"},
		{"plain name", "ghost://project/ghost/context", "ghost"},
		{"hash id", "ghost://project/6bdc098af7f5/tasks", "6bdc098af7f5"},
		{"global bucket", "ghost://project/_global/context", "_global"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProjectIDFromURI(tc.uri)
			if err != nil {
				t.Fatalf("parseProjectIDFromURI(%q): %v", tc.uri, err)
			}
			if got != tc.want {
				t.Errorf("parseProjectIDFromURI(%q) = %q, want %q", tc.uri, got, tc.want)
			}
		})
	}
}

func TestProjectResourceURI_RoundTrips(t *testing.T) {
	ids := []string{
		"myproj", "6bdc098af7f5", "_global", "a-b.c~d",
		"team/api", "/srv/u/repo", "my proj", "a:b", "a@b", "a+b", "a,b",
		"a=b&c", "100%", "team%2Fapi", "café", "日本",
	}
	for _, id := range ids {
		for _, suffix := range []string{"context", "decisions", "tasks"} {
			uri := projectResourceURI(id, suffix)
			got, err := parseProjectIDFromURI(uri)
			if err != nil {
				t.Errorf("parse(%q) built from %q: %v", uri, id, err)
				continue
			}
			if got != id {
				t.Errorf("parse(projectResourceURI(%q, %q)) = %q (uri %q)", id, suffix, got, uri)
			}
		}
	}
}

func TestProjectResourceURI_EncodesLikeTheTemplate(t *testing.T) {
	tests := []struct{ id, want string }{
		{"myproj", "ghost://project/myproj/context"},
		{"6bdc098af7f5", "ghost://project/6bdc098af7f5/context"},
		{"_global", "ghost://project/_global/context"},
		{"a-b.c~d", "ghost://project/a-b.c~d/context"},
		{"team/api", "ghost://project/team%2Fapi/context"},
		{"/srv/u/repo", "ghost://project/%2Fsrv%2Fu%2Frepo/context"},
		{"my proj", "ghost://project/my%20proj/context"},
		{"a:b@c", "ghost://project/a%3Ab%40c/context"},
		{"a,b", "ghost://project/a%2Cb/context"},
		{"café", "ghost://project/caf%C3%A9/context"},
	}
	for _, tc := range tests {
		if got := projectResourceURI(tc.id, "context"); got != tc.want {
			t.Errorf("projectResourceURI(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

type encodedURIFixture struct {
	srv     *Server
	session *mcp.ClientSession
	updated chan string
}

func newEncodedURIFixture(t *testing.T) *encodedURIFixture {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	ctx := context.Background()

	for _, p := range []struct{ id, path, name string }{
		{"team", "/srv/team", "team"},
		{"team/api", "/srv/team/api", "team-api"},
		{"/srv/u/repo", "/srv/u/repo", "repo"},
		{"my proj", "/srv/my proj", "my proj"},
		{"a:b@c+d", "/srv/a:b@c+d", "subdelims"},
		{"/srv/u/my repo", "/srv/u/my repo", "my repo"},
	} {
		if err := store.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject(%q): %v", p.id, err)
		}
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	updated := make(chan string, 64)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return &encodedURIFixture{srv: srv, session: session, updated: updated}
}

func (f *encodedURIFixture) read(t *testing.T, uri string) string {
	t.Helper()
	res, err := f.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Errorf("ReadResource(%q): %v", uri, err)
		return ""
	}
	var sb strings.Builder
	for _, c := range res.Contents {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func TestProjectResources_EncodedIDReadsTheNamedProject(t *testing.T) {
	f := newEncodedURIFixture(t)
	marker := map[string]string{
		"team":        "alphateamonlymarker",
		"team/api":    "betaapionlymarker",
		"/srv/u/repo": "gammarepoonlymarker",
		"my proj":     "deltaspaceonlymarker",
		"a:b@c+d":     "epsilonsubdelimmarker",
	}
	for id, m := range marker {
		callTool(t, f.session, "ghost_memory_save", map[string]any{
			"project_id": id, "content": "memory " + m, "category": "fact",
		})
		callTool(t, f.session, "ghost_task_create", map[string]any{
			"project_id": id, "title": "task " + m,
		})
	}

	if text := f.read(t, "ghost://project/team%2Fapi/context"); strings.Contains(text, marker["team"]) || !strings.Contains(text, marker["team/api"]) {
		t.Errorf("team%%2Fapi/context did not read team/api alone:\n%s", text)
	}
	if text := f.read(t, "ghost://project/%2Fsrv%2Fu%2Frepo/tasks"); !strings.Contains(text, marker["/srv/u/repo"]) {
		t.Errorf("%%2Fsrv%%2Fu%%2Frepo/tasks did not read the path-shaped project:\n%s", text)
	}

	for id, own := range marker {
		for _, suffix := range []string{"context", "tasks"} {
			uri := projectResourceURI(id, suffix)
			text := f.read(t, uri)
			if !strings.Contains(text, own) {
				t.Errorf("read %q: missing %q's own row %q:\n%s", uri, id, own, text)
			}
			for other, m := range marker {
				if other != id && strings.Contains(text, m) {
					t.Errorf("read %q: leaked project %q's row %q:\n%s", uri, other, m, text)
				}
			}
		}
	}
}

func awaitOnly(t *testing.T, updated <-chan string, want string) {
	t.Helper()
	awaitNotification(t, updated, want)
	quiet := time.After(subscriptionQuiet)
	for {
		select {
		case got := <-updated:
			t.Errorf("unexpected extra notification %q after %q", got, want)
		case <-quiet:
			return
		}
	}
}

func TestResourceSubscription_EncodedIDNotifiesAReadableURI(t *testing.T) {
	f := newEncodedURIFixture(t)
	ctx := context.Background()

	cases := []struct{ id, encoded, marker string }{
		{"team/api", "ghost://project/team%2Fapi/context", "notifyapimarker"},
		{"/srv/u/repo", "ghost://project/%2Fsrv%2Fu%2Frepo/context", "notifyrepomarker"},
		{"my proj", "ghost://project/my%20proj/context", "notifyspacemarker"},
	}
	const prefixURI = "ghost://project/team/context"
	subs := []string{prefixURI}
	for _, c := range cases {
		subs = append(subs, c.encoded)
	}
	for _, uri := range subs {
		if err := f.session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			t.Fatalf("Subscribe(%q): %v", uri, err)
		}
	}
	awaitResourceSubscriptions(t, f.srv, ctx, f.updated, subs...)

	for _, c := range cases {
		callTool(t, f.session, "ghost_memory_save", map[string]any{
			"project_id": c.id, "content": "memory " + c.marker, "category": "fact",
		})
		awaitOnly(t, f.updated, c.encoded)
		if text := f.read(t, c.encoded); !strings.Contains(text, c.marker) {
			t.Errorf("re-read of notified %q does not show the write:\n%s", c.encoded, text)
		}
	}
}

func TestResourceSubscription_EncodedNameAndIDBothNotified(t *testing.T) {
	f := newEncodedURIFixture(t)
	ctx := context.Background()

	const idURI = "ghost://project/%2Fsrv%2Fu%2Fmy%20repo/context"
	const nameURI = "ghost://project/my%20repo/context"
	for _, uri := range []string{idURI, nameURI} {
		if err := f.session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			t.Fatalf("Subscribe(%q): %v", uri, err)
		}
	}
	awaitResourceSubscriptions(t, f.srv, ctx, f.updated, idURI, nameURI)

	callTool(t, f.session, "ghost_memory_save", map[string]any{
		"project_id": "/srv/u/my repo", "content": "memory twoaliasmarker", "category": "fact",
	})
	awaitNotification(t, f.updated, idURI)
	awaitOnly(t, f.updated, nameURI)
	for _, uri := range []string{idURI, nameURI} {
		if text := f.read(t, uri); !strings.Contains(text, "twoaliasmarker") {
			t.Errorf("re-read of notified %q does not show the write:\n%s", uri, text)
		}
	}
}

func TestResourceSubscription_ProjectDeleteNotifiesEncodedURI(t *testing.T) {
	f := newEncodedURIFixture(t)
	ctx := context.Background()

	const idURI = "ghost://project/%2Fsrv%2Fu%2Frepo/context"
	const nameURI = "ghost://project/repo/context"
	for _, uri := range []string{idURI, nameURI} {
		if err := f.session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			t.Fatalf("Subscribe %s: %v", uri, err)
		}
	}
	awaitResourceSubscriptions(t, f.srv, ctx, f.updated, idURI, nameURI)

	res := callTool(t, f.session, "ghost_project_delete", map[string]any{"project": "/srv/u/repo", "apply": true})
	if res.IsError {
		t.Fatalf("ghost_project_delete: %s", resultText(res))
	}
	want := map[string]bool{idURI: true, nameURI: true}
	for len(want) > 0 {
		select {
		case got := <-f.updated:
			if !want[got] {
				t.Fatalf("unexpected notification %q; still waiting for %v", got, want)
			}
			delete(want, got)
		case <-time.After(notificationWait):
			t.Fatalf("no resources/updated notification for %v within %s", want, notificationWait)
		}
	}
	select {
	case got := <-f.updated:
		t.Errorf("unexpected extra notification %q", got)
	case <-time.After(subscriptionQuiet):
	}
}

func TestProjectResourceURI_GlobalIsUnchanged(t *testing.T) {
	if got := projectResourceURI(memory.GlobalProjectID, "context"); got != "ghost://project/_global/context" {
		t.Errorf("global context URI = %q", got)
	}
}
