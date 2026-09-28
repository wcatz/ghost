package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// seedMarkable stores a memory with no resolution keyword, which is the corpus
// --mark exists for: the prefilter never proposes such a note, so no pass would
// ever stamp it and an agent that has read it alongside a newer one is the only
// thing that can.
func seedMarkable(t *testing.T, store *memory.Store, projectID, content string) string {
	t.Helper()
	ctx := context.Background()
	// Create does not make the project and ImportMemory does, and memories.id
	// carries a foreign key onto it — so a second project is created here rather
	// than in each caller, which is where a missing row would otherwise show up
	// as a constraint failure with nothing to say which project was missing.
	if err := store.EnsureProject(ctx, projectID, "/tmp/"+projectID, projectID); err != nil {
		t.Fatalf("EnsureProject(%s): %v", projectID, err)
	}
	id, err := store.Create(ctx, projectID, memory.Memory{
		Category: "fact", Content: content, Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// callToolErr calls a tool that is expected to be refused and returns the
// refusal's text, so a refusal can be asserted on its own words. Both refusal
// shapes count: a handler error reaches the client as a protocol error for some
// tools and as an IsError result for others, and this suite's tools do both
// depending on where the check lands. The messages name WHICH form of the ref
// failed, and a test that only checked "some refusal" would not notice one of them
// going silent.
func callToolErr(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("CallTool %s was not refused: %s", tool, resultText(res))
	}
	return resultText(res)
}

func isMarkedResolved(t *testing.T, store *memory.Store, id string) bool {
	t.Helper()
	mems, err := store.GetByIDs(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("GetByIDs(%s) returned %d rows", id, len(mems))
	}
	return mems[0].ResolvedAt != nil && *mems[0].ResolvedAt != ""
}

// TestResolveMarkIsRegisteredAsATool: the MCP surface is the reason an agent can
// do this at all, so the tool has to be registered with the rest — a helper
// nothing calls is not a capability.
func TestResolveMarkIsRegisteredAsATool(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "ghost_resolve_mark" {
			found = tool
		}
	}
	if found == nil {
		t.Fatal("ghost_resolve_mark is not registered on the MCP server")
	}
	// The tool that takes a memory out of every later session says so in its
	// annotations, so a client can prompt for the confirmation they imply. It is
	// idempotent rather than not: marking an already-resolved memory is a
	// no-op the tool reports as one, and the memory is not deleted, so a repeat
	// converges instead of compounding.
	if found.Annotations == nil || found.Annotations.DestructiveHint == nil || !*found.Annotations.DestructiveHint {
		t.Error("ghost_resolve_mark does not declare DestructiveHint")
	}
	if !found.Annotations.IdempotentHint {
		t.Error("ghost_resolve_mark does not declare IdempotentHint")
	}
	if found.Description == "" {
		t.Error("ghost_resolve_mark has no description")
	}
}

// TestResolveMarkStampsTheNamedMemory: the MCP counterpart of
// `ghost resolve --mark`. An agent that has read a memory and a newer one saying
// its fix landed can bury it by name, through the same store path the CLI uses.
func TestResolveMarkStampsTheNamedMemory(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	ctx := context.Background()
	id := seedMarkable(t, store, "test-project", "the relay firmware on the edge nodes runs build 4471")
	untouched := seedMarkable(t, store, "test-project", "the staging relay speaks QUIC on port 4471")

	// A prefix, as every Ghost report abbreviates an id to.
	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id[:8]},
	}))
	if !strings.Contains(out, "Marked 1") {
		t.Errorf("the tool did not report its one stamp:\n%s", out)
	}
	// The memory itself, so an agent reporting this to its user is quoting the
	// database rather than the call it made.
	if !strings.Contains(out, "the relay firmware on the edge nodes runs build 4471") {
		t.Errorf("the result does not quote the memory it marked:\n%s", out)
	}
	if !isMarkedResolved(t, store, id) {
		t.Error("the named memory is not resolved")
	}
	if isMarkedResolved(t, store, untouched) {
		t.Error("the tool touched a memory it was not asked about")
	}

	// The 'resolve' history row, with the CALLING CLIENT as the performer: the
	// same provenance every other mutating tool on this surface records, so the
	// row reads like every other write in the history.
	entries, err := store.MemoryHistory(ctx, id, 20)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var resolves int
	for _, e := range entries {
		if e.Phase != "resolve" {
			continue
		}
		resolves++
		if e.Agent == "" {
			t.Error("the resolve history row has no performer: it is indistinguishable from a pass's verdict")
		}
	}
	if resolves != 1 {
		t.Errorf("the tool wrote %d resolve history row(s), want 1", resolves)
	}

	// The KEEP cache is dropped, so a later pass cannot report the row as cached
	// and bring a memory the agent buried straight back.
	hashes, err := store.ResolveKeptHashes(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	if got := hashes[id]; got != "" {
		t.Errorf("resolve_kept_hash = %q, want empty: a cached KEEP would un-hide this row on the next pass", got)
	}

	// The inverse, as a CLI COMMAND and SCOPED to what this call stamped. There is
	// no MCP tool for it because ghost_resolve is the FORWARD pass and would
	// stamp MORE memories, so an agent with no shell cannot undo this and has to
	// be told so rather than pointed at the wrong tool.
	if !strings.Contains(out, "ghost resolve test-project --reassess --only") {
		t.Errorf("the result does not name the scoped CLI repair:\n%s", out)
	}
	if !strings.Contains(out, "no MCP tool for it") {
		t.Errorf("the result does not say the inverse is not a tool call:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("the follow-up does not name the memory it stamped:\n%s", out)
	}
}

// TestResolveMarkReportsAnAlreadyResolvedMemoryAsANoOp: the issue asks for this
// by name, and on this surface it matters more than on the CLI — an agent that
// reads "Marked 1" for a memory that was already resolved will tell its user it
// did something it did not do.
func TestResolveMarkReportsAnAlreadyResolvedMemoryAsANoOp(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	ctx := context.Background()
	id := seedMarkable(t, store, "test-project", "a note an earlier pass already buried")

	callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id},
	})
	again := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id},
	}))
	if !strings.Contains(again, "already resolved") {
		t.Errorf("a repeat mark does not report the no-op:\n%s", again)
	}
	if strings.Contains(again, "Marked 1") {
		t.Errorf("a repeat mark claims a stamp it did not write:\n%s", again)
	}
	// And the follow-up names nothing: there is no stamp of this call to undo, and
	// a command that cleared it would send the agent after a repair the pass would
	// report as having nothing to do.
	if strings.Contains(again, "--reassess --only") {
		t.Errorf("a run that stamped nothing printed a repair command:\n%s", again)
	}
	// No second history row: a history row records a write, and none happened.
	entries, err := store.MemoryHistory(ctx, id, 20)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	var resolves int
	for _, e := range entries {
		if e.Phase == "resolve" {
			resolves++
		}
	}
	if resolves != 1 {
		t.Errorf("the no-op wrote %d resolve history row(s) in total, want 1", resolves)
	}
}

// TestResolveMarkRefusesARefItCannotResolve: an ambiguous prefix is a question
// with two answers and the memory it would bury is not something to guess at, so
// the refusal lists the matches. A ref naming nothing says so, and a request
// naming no memory is a usage error rather than a mark of nothing that reads as a
// completed no-op.
func TestResolveMarkRefusesARefItCannotResolve(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	id := seedMarkable(t, store, "test-project", "a note the operator can name by a short prefix")

	for _, tc := range []struct {
		name, ref, want string
	}{
		{name: "nothing holds it", ref: "ffffffffffffffff", want: "no memory in project"},
		{name: "too short for a prefix", ref: id[:6], want: "too short to be a prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
				"project_id": "test-project",
				"memory_ids": []string{tc.ref},
			})
			if !strings.Contains(got, tc.want) {
				t.Errorf("refusal %q must say %q", got, tc.want)
			}
			if isMarkedResolved(t, store, id) {
				t.Error("a refused request marked a memory")
			}
		})
	}

	if got := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{},
	}); !strings.Contains(got, "memory_ids is required") {
		t.Errorf("a request naming no memory must be refused as such, got %q", got)
	}
	if got := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "",
		"memory_ids": []string{id},
	}); !strings.Contains(got, "project_id is required") {
		t.Errorf("a request with no project must be refused as such, got %q", got)
	}
	if got := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "no-such-project-here",
		"memory_ids": []string{id},
	}); !strings.Contains(got, "not found") {
		t.Errorf("an unknown project must be refused, got %q", got)
	}
}

// TestResolveMarkWillNotBuryAnotherProjectsMemory: the guard that matters most on
// a shared store. A ref resolves inside the project PLUS `_global` — a promotion
// moves a row while keeping the links pointing at it, and both ends of an edge
// have to stay nameable — so a promoted row IS reachable by ref from a project
// that does not own it. Marking it would bury a memory every project shares, on
// the say-so of one of them.
func TestResolveMarkWillNotBuryAnotherProjectsMemory(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	ctx := context.Background()
	theirs := seedMarkable(t, store, "other-project", "a note that belongs to another project")
	if err := store.PromoteToGlobal(ctx, "other-project", theirs); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	// The project that tries to mark, so the refusal below is about the ref.
	seedMarkable(t, store, "test-project", "a note in the project that tries to mark")

	refused := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{theirs},
	})
	if !strings.Contains(refused, "_global") && !strings.Contains(refused, "not to") {
		t.Errorf("the refusal does not say which half of the store the row is in: %q", refused)
	}
	if isMarkedResolved(t, store, theirs) {
		t.Fatal("the promoted row was stamped: a memory every project shares was buried by one of them")
	}
}

// TestResolveMarkNamesAnIDNoFlagCanCarry: `ghost import` writes an artifact's ids
// verbatim, so an id can hold a comma or a newline. `--only` splits on commas, so
// such an id is not nameable by that flag however it is quoted, and the one-per-
// line file is the only surface that reaches it; a newline reaches nothing at all.
// An agent told nothing would run the command above, clear fewer memories than the
// call stamped, and report a repair that did not happen.
func TestResolveMarkNamesAnIDNoFlagCanCarry(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	// The id is written by an import, which is what makes it hold a comma at all:
	// `ghost import` writes an artifact's ids verbatim and ImportMemory refuses
	// only an empty one, so this is what such a row looks like in a real store.
	ctx := context.Background()
	commy := "imported,note"
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
		ID: commy, ProjectID: "test-project", Category: "fact",
		Content: "an imported note whose id holds a comma", Source: "mcp",
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{commy},
	}))
	if !strings.Contains(out, "hold a comma") {
		t.Errorf("an id --only cannot carry is not reported as such:\n%s", out)
	}
	if !strings.Contains(out, "--only-file") {
		t.Errorf("the result does not name the surface that can carry it:\n%s", out)
	}
	if strings.Contains(out, "--reassess --only ") {
		t.Errorf("the result emitted an --only command for a comma-bearing id, which cannot address it:\n%s", out)
	}
}
