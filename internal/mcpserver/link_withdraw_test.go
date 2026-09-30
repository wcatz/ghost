package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// linkWithdrawServer is testServer with the concrete store kept, because this
// file seeds and reads the link graph the tool acts on.
func linkWithdrawServer(t *testing.T) (*Server, *memory.Store) {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(store, logger, "test"), store
}

// seedLinkWithdrawal stores two memories with a live 'supersedes'/'llm' edge
// between them and returns the ids.
func seedLinkWithdrawal(t *testing.T, store *memory.Store, projectID string) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	var err error
	newer, err = store.Create(ctx, projectID, memory.Memory{
		Category: "fact", Content: "The restore path is safe on one spindle.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	older, err = store.Create(ctx, projectID, memory.Memory{
		Category: "fact", Content: "The restore path needs two spindles to be safe.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	return newer, older
}

func liveInto(t *testing.T, store *memory.Store, projectID, target string) int {
	t.Helper()
	links, err := store.LinksInto(context.Background(), projectID, target, "supersedes")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	return len(links)
}

// TestLinkWithdrawRemovesTheNamedEdge is the MCP counterpart of
// `ghost supersede --withdraw`: an agent that can see a wrong supersession can
// name it and have it withdrawn, which until now it could not do at all — both
// repair passes are CLI-only, and one of them can only withdraw an edge the
// classifier now rejects.
func TestLinkWithdrawRemovesTheNamedEdge(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	newer, older := seedLinkWithdrawal(t, store, "abc123")

	msg, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, older, "")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink: %v", err)
	}
	if !strings.Contains(msg, "Withdrew 1 of 1") {
		t.Errorf("the result does not say what it withdrew: %q", msg)
	}
	// And it names the memory that was being buried, so the caller can tell from
	// the result whether it withdrew the edge it meant to.
	if !strings.Contains(msg, "The restore path needs two spindles to be safe.") {
		t.Errorf("the result does not name the target it was burying: %q", msg)
	}
	// The half of the repair the caller cannot infer for itself: the target stays
	// stamped resolved until the resolve repair pass runs, and the result says so
	// rather than leaving the agent to believe the memory is back in context.
	//
	// The repair is a CLI COMMAND, SCOPED to the ids this call withdrew, and it is
	// named as a command: there is no MCP surface for the repair at all.
	// `ghost_resolve` is the FORWARD pass — it stamps resolved_at on confirmed
	// evidence — and takes no id selector, so an agent pointed at it would bury MORE
	// memories and pay a harness call for it.
	want, _, _ := followup.ResolveCommand("test-project", []string{older})
	if !strings.Contains(msg, want) {
		t.Errorf("the result does not carry the scoped repair command %q: %q", want, msg)
	}
	if !strings.Contains(msg, "SCOPED") {
		t.Errorf("the result does not say the repair must be scoped: %q", msg)
	}
	// And it says the command is not a tool call, so an agent does not reach for
	// ghost_resolve instead.
	if !strings.Contains(msg, "no MCP tool for that repair") {
		t.Errorf("the result does not say the repair is not available on this surface: %q", msg)
	}
	if got := liveInto(t, store, "abc123", older); got != 0 {
		t.Errorf("live edges into the target = %d, want 0", got)
	}
	// And the audit row, which makes the withdrawal a record rather than a silent
	// graph edit.
	entries, err := store.MemoryHistory(context.Background(), older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	sawUnsupersede := false
	for _, e := range entries {
		if e.Phase == "unsupersede" {
			sawUnsupersede = true
		}
	}
	if !sawUnsupersede {
		t.Errorf("no unsupersede history row for the target; phases: %v", entries)
	}
}

// TestLinkWithdrawTakesPrefixes: an agent reads an id out of a search result
// and out of a report, and both abbreviate. Requiring the full 32 characters
// would make the tool undrivable from what the agent can actually see.
func TestLinkWithdrawTakesPrefixes(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	newer, older := seedLinkWithdrawal(t, store, "abc123")

	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer[:8], older[:8], ""); err != nil {
		t.Fatalf("withdrawSupersedesLink: %v", err)
	}
	if got := liveInto(t, store, "abc123", older); got != 0 {
		t.Errorf("live edges into the target = %d, want 0", got)
	}
}

// TestLinkWithdrawRefusesWhatItCannotDo: the tool's failures are refusals, not
// silent successes. A pair with no live edge must not read as a withdrawal, and
// an unknown project must not read as a project.
func TestLinkWithdrawRefusesWhatItCannotDo(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	newer, older := seedLinkWithdrawal(t, store, "abc123")

	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", "", older, ""); err == nil {
		t.Error("an empty source_id was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, "", ""); err == nil {
		t.Error("an empty target_id was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "no-such-project", newer, older, ""); err == nil {
		t.Error("an unknown project was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", older, newer, ""); err == nil {
		t.Error("a reversed pair with no such edge was accepted")
	}
	// A relation the graph cannot hold is refused by name, not defaulted: a typo
	// that silently became 'supersedes' would withdraw the OTHER edge of a pair
	// holding both and leave the one asked about live.
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, older, "caused"); err == nil {
		t.Error("an unknown relation was accepted")
	}
	// None of the refusals moved anything.
	if got := liveInto(t, store, "abc123", older); got != 1 {
		t.Errorf("live edges into the target = %d, want 1: a refusal must write nothing", got)
	}
	// The edge itself is still withdrawable afterwards, so a refusal is not a
	// one-way door for the correct request.
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, older, ""); err != nil {
		t.Errorf("the correct request failed after refusals: %v", err)
	}
}

// raceStore is the one race this tool cannot be handed by the real store alone:
// a concurrent pass taking the edge between this call's read and its write, which
// is InvalidateLink returning 0 with no error. The edge is still live when the
// read happens, so the request is valid and the write moves nothing — a state no
// pre-invalidation can produce, because that fails the read instead.
type raceStore struct {
	// The interface for everything else the tool calls (ResolveProject, and so
	// on), and the concrete store for the three link methods, which
	// provider.MemoryStore does not carry. The link methods are declared here
	// rather than promoted from a second embedded type, because two embeds would
	// both supply GetByIDs and an ambiguous selector is not a satisfied interface.
	provider.MemoryStore
	inner *memory.Store
}

func (r raceStore) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	return r.inner.MemoryIDsByIDPrefix(ctx, projectID, prefix)
}

// Declared because linkCapableStore embeds supersede.WithdrawStore, whose
// interface grew it for a `_global` withdrawal (#786). It is never called on this
// fake — the tool's assertion is at call time, so an incomplete fake here is a
// runtime failure in whichever test happens to name `_global` first, which is the
// worst way to find out.
func (r raceStore) MemoryIDsByIDPrefixAnyProject(ctx context.Context, prefix string) ([]string, error) {
	return r.inner.MemoryIDsByIDPrefixAnyProject(ctx, prefix)
}

func (r raceStore) LinksInto(ctx context.Context, projectID, memoryID, relation string) ([]memory.Link, error) {
	return r.inner.LinksInto(ctx, projectID, memoryID, relation)
}

// InvalidateLink reports that a concurrent pass took the edge first: the write
// moved nothing and reported no error, which is the one state a pre-invalidation
// cannot produce.
func (r raceStore) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	return 0, nil
}

func (s *Server) linkRaceStore(inner *memory.Store) *Server {
	return New(raceStore{MemoryStore: inner, inner: inner}, s.logger, "test")
}

// TestLinkWithdrawSaysSoWhenAConcurrentPassTookTheEdge: nothing failed, so this is
// not an error — but nothing was orphaned either, and the tool must not hand the
// caller a scoped repair pointing at a target no live edge accounts for. The
// per-row marker says which state the edge was in, because "withdrew" and "already
// gone" are the same sentence to a reader and only one of them happened.
func TestLinkWithdrawSaysSoWhenAConcurrentPassTookTheEdge(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	newer, older := seedLinkWithdrawal(t, store, "abc123")
	raced := srv.linkRaceStore(store)

	msg, err := raced.withdrawSupersedesLink(context.Background(), "test-project", newer, older, "")
	if err != nil {
		t.Fatalf("a concurrent withdrawal is not a failure: %v", err)
	}
	if !strings.Contains(msg, "Withdrew 0 of 1") {
		t.Errorf("the result does not report that nothing moved: %q", msg)
	}
	if !strings.Contains(msg, "already gone") {
		t.Errorf("the row does not say the edge was already gone: %q", msg)
	}
	// The target IS repairable even though this call moved nothing: a concurrent
	// pass that took the edge left no live edge and a resolved_at nothing defends,
	// which is exactly the state the repair clears. So the command must be here,
	// naming that one id — and the row must say why nothing moved.
	if !strings.Contains(msg, "already gone") {
		t.Errorf("the row does not say the edge was already gone: %q", msg)
	}
	if !strings.Contains(msg, older) {
		t.Errorf("the repair command does not name the target (%s): %q", older, msg)
	}
	if !strings.Contains(msg, "ghost resolve test-project --reassess --only '"+older+"' --apply") {
		t.Errorf("the result does not carry the scoped repair for the taken edge: %q", msg)
	}
	if !strings.Contains(msg, older[:8]) {
		t.Errorf("the result does not name the edge it was asked about (%s): %q", older, msg)
	}
}

// TestLinkWithdrawIsProjectScoped: another project's edge is neither withdrawable
// nor reported. This is the ownership rule every other project-scoped tool
// applies, and the reason a caller cannot use this one to reach a graph that is
// not its own.
func TestLinkWithdrawIsProjectScoped(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "other", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	newer, older := seedLinkWithdrawal(t, store, "other")

	_, err := srv.withdrawSupersedesLink(ctx, "test-project", newer, older, "")
	if err == nil {
		t.Fatal("the tool reached another project's edge")
	}
	// The refs are resolved inside the named project, so the refusal comes from
	// that resolution rather than from a graph read: this call learns nothing
	// about the other project's edges, not even that they exist.
	if !strings.Contains(err.Error(), "no memory in project") {
		t.Errorf("the refusal does not come from the project-scoped ref resolution: %v", err)
	}
	if got := liveInto(t, store, "other", older); got != 1 {
		t.Errorf("the other project's edge = %d live row(s), want 1: it must be untouched", got)
	}
}

// TestLinkWithdrawRegisteredAsATool: the MCP surface is the reason an agent can
// do this at all, so the tool has to be registered with the rest — a helper
// nothing calls is not a capability.
func TestLinkWithdrawRegisteredAsATool(t *testing.T) {
	_, session := newCapSession(t)
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "ghost_link_withdraw" {
			found = tool
		}
	}
	if found == nil {
		t.Fatal("ghost_link_withdraw is not registered on the MCP server")
	}
	// The tool that removes a graph edge says so in its annotations, so a client
	// can prompt for the confirmation the annotation implies.
	if found.Annotations == nil || found.Annotations.DestructiveHint == nil || !*found.Annotations.DestructiveHint {
		t.Error("ghost_link_withdraw does not declare DestructiveHint")
	}
	if found.Description == "" {
		t.Error("ghost_link_withdraw has no description")
	}
}

// TestLinkWithdrawRemovesACausesEdge is #833 on the MCP surface. Both endpoints
// share created_at AND updated_at, which is the #778 tie the ordinary pass counts
// as Unoriented and refuses to judge — so a 'causes' cycle over such a pair has no
// pass-level repair at all, and a tool that can only withdraw 'supersedes' edges
// leaves the contradiction in the graph with nothing to run. The claim is
// load-bearing since #823 (its direction decides which way the pair is judged), so
// the undo has to exist.
func TestLinkWithdrawRemovesACausesEdge(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	cause, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The migration left the lock table populated.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(cause): %v", err)
	}
	effect, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The replica fell behind by four hours afterwards.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(effect): %v", err)
	}
	if err := store.CreateLink(ctx, cause, effect, "causes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink(causes): %v", err)
	}

	msg, err := srv.withdrawSupersedesLink(ctx, "test-project", cause, effect, "causes")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink(causes): %v", err)
	}
	// The header names the RELATION it withdrew. An agent reporting "withdrew 1
	// supersedes link" over a 'causes' edge would be reporting an edge nobody named,
	// and the wrong one is the edge it would then try to repair.
	if !strings.Contains(msg, "named causes link(s)") {
		t.Errorf("the result does not name the relation it withdrew: %q", msg)
	}
	if links, lerr := store.LinksInto(ctx, "test-project", effect, "causes"); lerr != nil {
		t.Fatalf("LinksInto: %v", lerr)
	} else if len(links) != 0 {
		t.Errorf("live 'causes' edges into the effect = %d, want 0", len(links))
	}
	// And no unsupersede row: a 'causes' claim never demoted its target and never
	// stamped resolved_at, so there is no standing to reverse.
	entries, err := store.MemoryHistory(ctx, effect, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	for _, e := range entries {
		if e.Phase == "unsupersede" {
			t.Errorf("a 'causes' withdrawal wrote the unsupersede row: %+v", e)
		}
	}
}

// TestLinkWithdrawDefaultsToSupersedesAndTakesCausesOnlyWhenNamed: the one case
// where the default decides the edge. A pair holding both relations is the case a
// wrong default withdraws something the caller did not name, so the pin is the
// only way through it — and the pin has to actually pin.
func TestLinkWithdrawDefaultsToSupersedesAndTakesCausesOnlyWhenNamed(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	cause, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The migration left the lock table populated.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(cause): %v", err)
	}
	effect, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The replica fell behind by four hours afterwards.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(effect): %v", err)
	}
	if err := store.CreateLink(ctx, cause, effect, "causes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink(causes): %v", err)
	}
	if err := store.CreateLink(ctx, cause, effect, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink(supersedes): %v", err)
	}

	// Unpinned: 'supersedes', the relation the edge's removal un-hides a memory for.
	msg, err := srv.withdrawSupersedesLink(ctx, "test-project", cause, effect, "")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink (default): %v", err)
	}
	if !strings.Contains(msg, "named supersedes link(s)") {
		t.Errorf("the default did not pick the supersedes edge: %q", msg)
	}
	links, err := store.LinksInto(ctx, "test-project", effect, "causes")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("live 'causes' edges = %d, want 1: the default withdrew the other relation", len(links))
	}

	// Pinned: the 'causes' edge, and the 'supersedes' one this same call just
	// took is not what it withdraws now.
	msg, err = srv.withdrawSupersedesLink(ctx, "test-project", cause, effect, "causes")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink(causes): %v", err)
	}
	if !strings.Contains(msg, "named causes link(s)") {
		t.Errorf("the pinned relation was not the one withdrawn: %q", msg)
	}
	links, err = store.LinksInto(ctx, "test-project", effect, "causes")
	if err != nil {
		t.Fatalf("LinksInto: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("live 'causes' edges = %d, want 0", len(links))
	}
}

// An id holding a comma cannot be named by `--only` at all: the parser splits its
// value on commas, so however the id is quoted it becomes two selectors that name
// nothing. Rendering it would hand the agent a command that runs and judges the
// wrong rows, and this surface writes no --only-file — so the ids are named here
// and the agent is told the file form is the only way to reach them.
func TestLinkWithdrawNamesAnIDThatOnlyTheFileFormCanCarry(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	// An imported artifact writes its ids verbatim, so a memory's id is whatever
	// the file said. The seeding helper names its own ids, so this one is renamed
	// through the store to make the pair a real edge pointing at it.
	// The edge points at an id an import brought in, and that id holds a comma:
	// `ghost import` writes an artifact's ids verbatim, and since #791
	// ImportMemory refuses only the shapes that can break a rendered LINE — a
	// control character, whitespace, a backtick or a «». A comma breaks a
	// SELECTOR rather than a line, so it still reaches the store, and this is
	// what such a row looks like in a real one.
	commy := "imported,note"
	ctx := context.Background()
	// ImportMemory checks the project exists, and Create does not, so the
	// project is made first — which is also what ResolveProject would find.
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
		ID: commy, ProjectID: "test-project", Category: "fact",
		Content: "The restore path needs two spindles to be safe.", Source: "mcp",
	}, memory.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	newer, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The restore path is safe on one spindle.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	if err := store.CreateLink(ctx, newer, commy, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	msg, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, commy, "")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink: %v", err)
	}
	if !strings.Contains(msg, "No --only command can name the target") {
		t.Errorf("the result does not say there is no --only command: %q", msg)
	}
	if !strings.Contains(msg, commy) {
		t.Errorf("the result does not name the id that only the file can carry: %q", msg)
	}
	if !strings.Contains(msg, "--only-file") {
		t.Errorf("the result does not point at the surface that can carry it: %q", msg)
	}
	// And no command it printed may carry it, or the agent runs something that
	// judges the wrong rows. Nor may it print the UNSCOPED repair: that is the
	// project-wide re-judge, and a copy-pasteable line naming it is the one thing
	// this answer must not hand over.
	for _, line := range strings.Split(msg, "\n") {
		if strings.Contains(line, "ghost resolve") && !strings.Contains(line, "--only") && !strings.Contains(line, "--only-file") {
			t.Errorf("the result printed an unscoped repair line: %q", line)
		}
		if strings.Contains(line, "--only ") && strings.Contains(line, commy) {
			t.Errorf("the printed command carries a comma-bearing id: %q", line)
		}
	}
}

// No surface can carry an id holding a newline — `--only` splits on commas and the
// --only-file is one id per line — so the answer has to say the memory stays
// resolved rather than implying a repair exists. An agent that believes otherwise
// leaves a memory out of every session with nothing able to clear it.
//
// The row is written in SQL rather than through ImportMemory, which now REFUSES
// such an id (#791). The state under test is still reachable and the answer is
// still the only correct one: this is a store written before the refusal landed,
// one restored from a snapshot taken by an older Ghost, or a hand-edited
// database. Refusing the id on the way in says nothing about what to do about the
// ones already there, and "the memory stays resolved, re-import it under an id
// this build accepts" has to be what an agent holding one is told.
func TestLinkWithdrawSaysNoSurfaceCanNameANewlineID(t *testing.T) {
	db, store, srv := newStoreWithDB(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	wrapped := "imported\nnote"
	plantMemoryWithID(t, db, "test-project", wrapped, "fact",
		"The restore path needs two spindles to be safe.")
	newer, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "The restore path is safe on one spindle.", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.CreateLink(ctx, newer, wrapped, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	msg, err := srv.withdrawSupersedesLink(ctx, "test-project", newer, wrapped, "")
	if err != nil {
		t.Fatalf("withdrawSupersedesLink: %v", err)
	}
	if !strings.Contains(msg, "no\n") && !strings.Contains(msg, "newline") {
		t.Errorf("the result does not say the id holds a newline: %q", msg)
	}
	if !strings.Contains(msg, "stay resolved") {
		t.Errorf("the result does not say what happens to that memory: %q", msg)
	}
	if !strings.Contains(msg, "re-import it under an id") {
		t.Errorf("the result does not say what actually clears it: %q", msg)
	}
}
