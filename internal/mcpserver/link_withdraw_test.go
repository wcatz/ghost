package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
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
	links, err := store.SupersedesLinksInto(context.Background(), projectID, target)
	if err != nil {
		t.Fatalf("SupersedesLinksInto: %v", err)
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

	msg, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, older)
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
	// SCOPED, and naming the id it withdrew. An unscoped repair re-judges every
	// resolved memory in the project (#702 measured 143 rows proposed, ~35% of
	// them stale), so a result handing the agent the unscoped form is handing it
	// the command that rewrites the most.
	if !strings.Contains(msg, "--reassess --only") {
		t.Errorf("the result does not name a SCOPED repair: %q", msg)
	}
	if !strings.Contains(msg, older) {
		t.Errorf("the result does not name the target it orphaned (%s): %q", older, msg)
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

	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer[:8], older[:8]); err != nil {
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

	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", "", older); err == nil {
		t.Error("an empty source_id was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, ""); err == nil {
		t.Error("an empty target_id was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "no-such-project", newer, older); err == nil {
		t.Error("an unknown project was accepted")
	}
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", older, newer); err == nil {
		t.Error("a reversed pair with no such edge was accepted")
	}
	// None of the refusals moved anything.
	if got := liveInto(t, store, "abc123", older); got != 1 {
		t.Errorf("live edges into the target = %d, want 1: a refusal must write nothing", got)
	}
	// The edge itself is still withdrawable afterwards, so a refusal is not a
	// one-way door for the correct request.
	if _, err := srv.withdrawSupersedesLink(context.Background(), "test-project", newer, older); err != nil {
		t.Errorf("the correct request failed after refusals: %v", err)
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

	_, err := srv.withdrawSupersedesLink(ctx, "test-project", newer, older)
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
