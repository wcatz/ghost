package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func saveGlobalRow(t *testing.T, session *mcp.ClientSession, content string) string {
	t.Helper()
	res := callTool(t, session, "ghost_save_global", map[string]any{"content": content, "category": "fact"})
	if res.IsError {
		t.Fatalf("save global %q: %s", content, resultText(res))
	}
	id, ok := extractID(resultText(res))
	if !ok {
		t.Fatalf("save global carried no id: %q", resultText(res))
	}
	return id
}

// rowLine is the one line that starts a memory with id, failing on anything but
// exactly one: the marker is a field on the memory's line, never a line.
func rowLine(t *testing.T, out, id string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+id+"` (") {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one line for %s, got %d in:\n%s", id, len(found), out)
	}
	return found[0]
}

// linesFor returns every rendered memory line naming id. The withheld side of a
// separated pair renders none, so a caller checking a pair needs a non-fatal
// form rather than rowLine.
func linesFor(out, id string) []string {
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+id+"` (") {
			found = append(found, l)
		}
	}
	return found
}

// wantSeparatedPair asserts the pair is separated on this surface: exactly one
// side renders, and its line names the other as the row stage 5 withheld.
func wantSeparatedPair(t *testing.T, surface, out, a, b string) {
	t.Helper()
	la, lb := linesFor(out, a), linesFor(out, b)
	switch {
	case len(la) == 1 && len(lb) == 0:
		if !strings.Contains(la[0], "conflicts_with=`"+b+"`") {
			t.Errorf("%s: %s does not name the withheld %s: %q", surface, a, b, la[0])
		}
	case len(lb) == 1 && len(la) == 0:
		if !strings.Contains(lb[0], "conflicts_with=`"+a+"`") {
			t.Errorf("%s: %s does not name the withheld %s: %q", surface, b, a, lb[0])
		}
	default:
		t.Errorf("%s: a separated pair rendered %d line(s) for %s and %d for %s, want exactly one side",
			surface, len(la), a, len(lb), b)
	}
}

func TestEverySurfaceSeparatesAContradictingPair(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()

	a := saveValidityRow(t, session, "vproj: the cache is redis", nil)
	b := saveValidityRow(t, session, "vproj: the cache is memcached", nil)
	c := saveValidityRow(t, session, "vproj: an unrelated durable fact", nil)
	ga := saveGlobalRow(t, session, "always use tabs for indentation")
	gb := saveGlobalRow(t, session, "never use tabs for indentation")
	for _, e := range [][2]string{{a, b}, {ga, gb}} {
		if err := st.CreateLink(ctx, e[0], e[1], "contradicts", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	pc := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	wantSeparatedPair(t, "ghost_project_context", pc, a, b)
	wantSeparatedPair(t, "ghost_project_context", pc, ga, gb)
	if l := rowLine(t, pc, c); strings.Contains(l, "conflicts_with") {
		t.Errorf("an unlinked row is marked: %q", l)
	}

	wantSeparatedPair(t, "ghost://memories/global", renderGlobalMemoriesResource(t, srv), ga, gb)

	search := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "vproj", "query": "cache"}))
	wantSeparatedPair(t, "ghost_memory_search", search, a, b)
}

func TestNoSurfaceMarksAWithdrawnOrHalfWithheldPair(t *testing.T) {
	st := newValidityStore(t)
	srv, session := validityServerFor(t, st)
	ctx := context.Background()

	a := saveValidityRow(t, session, "vproj: events travel over kafka topics", nil)
	b := saveValidityRow(t, session, "vproj: nats carries the realtime fanout", nil)
	ga := saveGlobalRow(t, session, "prefer spaces for indentation")
	gb := saveGlobalRow(t, session, "prefer tabs for indentation")
	gone := saveValidityRow(t, session, "vproj: rabbit handled the legacy jobs",
		map[string]any{"valid_from": "2020-01-01", "valid_until": "2021-01-01"})
	for _, e := range [][2]string{{a, b}, {ga, gb}} {
		if err := st.CreateLink(ctx, e[0], e[1], "contradicts", 1, "manual"); err != nil {
			t.Fatalf("link: %v", err)
		}
		if n, err := st.InvalidateLink(ctx, e[0], e[1], "contradicts"); err != nil || n != 1 {
			t.Fatalf("withdraw: n=%d err=%v", n, err)
		}
	}
	// A live edge whose other side is expired: stage 2 withholds it.
	if err := st.CreateLink(ctx, a, gone, "contradicts", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}

	pc := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	search := resultText(callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "vproj", "query": "kafka OR nats"}))
	for name, out := range map[string]string{
		"ghost_project_context":   pc,
		"ghost_memory_search":     search,
		"ghost://memories/global": renderGlobalMemoriesResource(t, srv),
	} {
		if strings.Contains(out, "conflicts_with") {
			t.Errorf("%s marks a withdrawn or half-withheld pair:\n%s", name, out)
		}
	}
	if !strings.Contains(pc, a) || strings.Contains(pc, gone) {
		t.Fatalf("precondition: a admitted and the expired row withheld (a=%s gone=%s):\n%s", a, gone, pc)
	}
}
