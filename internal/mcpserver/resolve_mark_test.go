package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

func seedMarkable(t *testing.T, store *memory.Store, projectID, content string) string {
	t.Helper()
	ctx := context.Background()
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

func TestResolveMarkStampsTheNamedMemory(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClientNamed(t, srv, "claude-code")
	ctx := context.Background()
	id := seedMarkable(t, store, "test-project", "the relay firmware on the edge nodes runs build 4471")
	untouched := seedMarkable(t, store, "test-project", "the staging relay speaks QUIC on port 4471")

	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id[:8]},
	}))
	if !strings.Contains(out, "Marked 1") {
		t.Errorf("the tool did not report its one stamp:\n%s", out)
	}
	if !strings.Contains(out, "the relay firmware on the edge nodes runs build 4471") {
		t.Errorf("the result does not quote the memory it marked:\n%s", out)
	}
	if !isMarkedResolved(t, store, id) {
		t.Error("the named memory is not resolved")
	}
	if isMarkedResolved(t, store, untouched) {
		t.Error("the tool touched a memory it was not asked about")
	}

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
		if e.Agent != "claude-code" {
			t.Errorf("the resolve history row's agent = %q, want %q — it is the only resolve row that can say a reader decided rather than a classifier judged", e.Agent, "claude-code")
		}
		if e.ResolvedAt == nil || *e.ResolvedAt == "" {
			t.Errorf("the resolve history row records no resolved_at, so it is not a version of the stamped state: %+v", e)
		}
	}
	if resolves != 1 {
		t.Errorf("the tool wrote %d resolve history row(s), want 1", resolves)
	}

	hashes, err := store.ResolveKeptHashes(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveKeptHashes: %v", err)
	}
	if got := hashes[id]; got != "" {
		t.Errorf("resolve_kept_hash = %q, want empty: a cached KEEP would un-hide this row on the next pass", got)
	}

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
	if strings.Contains(again, "--reassess --only") {
		t.Errorf("a run that stamped nothing printed a repair command:\n%s", again)
	}
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

func TestResolveMarkNamesAPersistentTierRowAsExempt(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-project", "/tmp/test-project", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "juliet keeps the release key offline in the vault",
		Source: "mcp", Importance: 0.7, Retention: memory.RetentionPersistent,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id},
	}))
	if !strings.Contains(out, "persistent tier (exempt from resolve)") {
		t.Errorf("the result does not name the persistent tier as the reason:\n%s", out)
	}
	if strings.Contains(out, "no longer eligible") {
		t.Errorf("the result reports a persistent-tier row as a concurrent change:\n%s", out)
	}
	if !strings.Contains(out, "Marked 0 of 1") {
		t.Errorf("the result does not report that nothing was marked:\n%s", out)
	}
	if isMarkedResolved(t, store, id) {
		t.Error("the tool stamped a persistent-tier memory")
	}
}

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

func TestResolveMarkWillNotBuryAnotherProjectsMemory(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	ctx := context.Background()
	theirs := seedMarkable(t, store, "other-project", "a note that belongs to another project")
	if err := store.PromoteToGlobal(ctx, "other-project", theirs); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
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

func TestResolveMarkNamesAnIDNoFlagCanCarry(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClient(t, srv)
	// `ghost import` writes an artifact's ids verbatim, and since #791
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

func TestMCPResolveMarkReportNeverClaimsAMarkItDidNotMake(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	fresh := seedMarkable(t, store, "test-project", "a note the write did stamp")
	late := seedMarkable(t, store, "test-project", "a note pinned in the meantime")
	srv.store = &decliningMarkStore{
		MemoryStore: srv.store,
		store:       store,
		decline:     late,
	}
	session := connectedClientNamed(t, srv, "claude-code")
	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{fresh, late},
	}))
	if !strings.Contains(out, "Marked 1") {
		t.Errorf("the result does not report its one stamp:\n%s", out)
	}
	if !isMarkedResolved(t, store, fresh) {
		t.Error("the eligible row was not stamped")
	}
	if isMarkedResolved(t, store, late) {
		t.Error("the declined row was stamped after all")
	}
	if !strings.Contains(out, "not marked") {
		t.Errorf("the declined row is not reported as unmarked:\n%s", out)
	}
	if !strings.Contains(out, fresh) {
		t.Errorf("the follow-up omits the stamped memory:\n%s", out)
	}
	if strings.Contains(out, late) {
		t.Errorf("the follow-up names a memory this call did not stamp:\n%s", out)
	}
}

func TestResolveMarkNamesTheMemoriesAFailedWriteAskedAbout(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	id := seedMarkable(t, store, "test-project", "a note the mark could not stamp")
	srv.store = &failingMarkStore{
		MemoryStore: srv.store,
		store:       store,
		err:         errors.New("database is locked"),
	}
	session := connectedClientNamed(t, srv, "claude-code")

	msg := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id},
	})
	if !strings.Contains(msg, "database is locked") {
		t.Errorf("the error does not carry the store's own reason: %s", msg)
	}
	if !strings.Contains(msg, id[:8]) {
		t.Errorf("the error does not name the memory the call asked about (%s): %s", id[:8], msg)
	}
	if !strings.Contains(msg, "a note the mark could not stamp") {
		t.Errorf("the error does not quote the memory's own text: %s", msg)
	}
	if !strings.Contains(msg, "rolled all") && !strings.Contains(msg, "unchanged") {
		t.Errorf("the error does not say the rows are unchanged: %s", msg)
	}
	if isMarkedResolved(t, store, id) {
		t.Error("the row is resolved: the write did not roll back")
	}
}

func TestResolveMarkDoesNotCallAnAlreadyResolvedMemoryLiveOnAFailure(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	buried := seedMarkable(t, store, "test-project", "a note an earlier pass already buried")
	live := seedMarkable(t, store, "test-project", "a note the mark could not stamp")
	if n, err := store.SetResolved(context.Background(), []string{buried}); err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1 and no error", n, err)
	}

	srv.store = &failingMarkStore{
		MemoryStore: srv.store,
		store:       store,
		err:         errors.New("database is locked"),
	}
	session := connectedClientNamed(t, srv, "claude-code")

	msg := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{buried, live},
	})
	if !strings.Contains(msg, "already resolved") {
		t.Errorf("the failure does not name the already-resolved row as such:\n%s", msg)
	}
	if !strings.Contains(msg, "a note an earlier pass already buried") {
		t.Errorf("the failure does not quote that memory:\n%s", msg)
	}
	if !strings.Contains(msg, "rolled all 1 back") {
		t.Errorf("the failure counts a row nothing was written for as rolled back:\n%s", msg)
	}
	if !strings.Contains(msg, "a note the mark could not stamp") {
		t.Errorf("the failure does not name the eligible row:\n%s", msg)
	}
}

type failingMarkStore struct {
	provider.MemoryStore
	store *memory.Store
	err   error
}

func (f *failingMarkStore) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	return f.store.MemoryIDsByIDPrefix(ctx, projectID, prefix)
}

func (f *failingMarkStore) MarkResolved(context.Context, string, []string, memory.Provenance) ([]string, error) {
	return nil, f.err
}

type decliningMarkStore struct {
	provider.MemoryStore
	store   *memory.Store
	decline string
}

func (d *decliningMarkStore) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	return d.store.MemoryIDsByIDPrefix(ctx, projectID, prefix)
}

func (d *decliningMarkStore) MarkResolved(ctx context.Context, projectID string, ids []string, prov memory.Provenance) ([]string, error) {
	keep := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != d.decline {
			keep = append(keep, id)
		}
	}
	return d.store.MarkResolved(ctx, projectID, keep, prov)
}

func TestResolveMarkRefusesTheGlobalProjectBeforeTheTransaction(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	ctx := context.Background()
	promoted := seedMarkable(t, store, "test-project", "a promoted note every project injects")
	if err := store.PromoteToGlobal(ctx, "test-project", promoted); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	counting := &countingMarkStore{MemoryStore: srv.store, store: store}
	srv.store = counting
	session := connectedClientNamed(t, srv, "claude-code")

	refused := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": memory.GlobalProjectID,
		"memory_ids": []string{promoted},
	})
	if !strings.Contains(refused, "nothing marked: refusing to mark in the _global project") ||
		!strings.Contains(refused, "every project injects") {
		t.Errorf("the refusal is not the up-front one with its reason: %q", refused)
	}
	if strings.Contains(refused, "mark resolved in") || strings.Contains(refused, "FAILED") {
		t.Errorf("the refusal came from the transaction rather than the argument check: %q", refused)
	}
	if counting.lookups != 0 || counting.writes != 0 {
		t.Errorf("the store was reached before the refusal: %d lookups, %d writes", counting.lookups, counting.writes)
	}
	if isMarkedResolved(t, store, promoted) {
		t.Fatal("the promoted row was stamped")
	}

	id := seedMarkable(t, store, "test-project", "an ordinary note the mark may stamp")
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_resolve_mark", Arguments: map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id},
	}}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !isMarkedResolved(t, store, id) || counting.writes != 1 {
		t.Fatalf("an ordinary mark did not land (%d writes)", counting.writes)
	}
}

type countingMarkStore struct {
	provider.MemoryStore
	store           *memory.Store
	lookups, writes int
}

func (c *countingMarkStore) MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error) {
	c.lookups++
	return c.store.MemoryIDsByIDPrefix(ctx, projectID, prefix)
}

func (c *countingMarkStore) MarkResolved(ctx context.Context, projectID string, ids []string, prov memory.Provenance) ([]string, error) {
	c.writes++
	return c.store.MarkResolved(ctx, projectID, ids, prov)
}

func TestResolveMarkAcceptsStringifiedMemoryIDs(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClientNamed(t, srv, "claude-code")
	id := seedMarkable(t, store, "test-project", "a note to mark through a stringified id list")
	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": `["` + id[:8] + `"]`,
	}))
	if !strings.Contains(out, "Marked 1") {
		t.Errorf("the tool did not report its one stamp:\n%s", out)
	}
	if !isMarkedResolved(t, store, id) {
		t.Error("the named memory is not resolved")
	}
}

func TestResolveMarkRefusesMalformedStringifiedMemoryIDs(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClientNamed(t, srv, "claude-code")
	id := seedMarkable(t, store, "test-project", "a note a malformed list must not touch")
	refused := callToolErr(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": "not json",
	})
	if !strings.Contains(refused, "memory_ids") || !strings.Contains(refused, "array of strings") {
		t.Errorf("the refusal must name the field and the shape it wanted: %q", refused)
	}
	if isMarkedResolved(t, store, id) {
		t.Error("a refused request marked a memory")
	}
}

func TestResolveMarkEscapesControlAndFormatCharacters(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	session := connectedClientNamed(t, srv, "claude-code")

	content := "gh\x1b[31most\u202ereversed"
	id := seedMarkable(t, store, "test-project", content)

	out := resultText(callTool(t, session, "ghost_resolve_mark", map[string]any{
		"project_id": "test-project",
		"memory_ids": []string{id[:8]},
	}))

	if strings.Contains(out, "\x1b") {
		t.Errorf("ghost_resolve_mark printed raw ESC: %q", out)
	}
	if strings.Contains(out, "\u202e") {
		t.Errorf("ghost_resolve_mark printed raw U+202E: %q", out)
	}
	if !strings.Contains(out, "\\x1b") {
		t.Errorf("ghost_resolve_mark missing ESC escape: %q", out)
	}
	if !strings.Contains(out, "\\u202e") {
		t.Errorf("ghost_resolve_mark missing U+202E escape: %q", out)
	}
	if !strings.Contains(out, "Marked 1") {
		t.Errorf("ghost_resolve_mark did not report mark: %q", out)
	}
}
