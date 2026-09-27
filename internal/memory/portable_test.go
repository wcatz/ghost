package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// portableTestStore opens a store over a temp-dir database, with the logger
// silenced so a failing case does not bury its own message.
func portableTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// TestPortableProjectsCarryRepoRemote: a project exported for another machine
// has to carry its repository remote. ListProjects cannot be used for this —
// it has no repo_remote column — which is why a project read for the portable
// form is its own query.
func TestPortableProjectsCarryRepoRemote(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	// The names deliberately do not sort the way the ids do: an ordering
	// assertion only pins "by id" if the two orders disagree, and "zebra"/"alpha"
	// against "p1"/"p2" is the cheapest way to make them.
	if err := store.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "zebra", "git@github.com:wcatz/one.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if err := store.EnsureProject(ctx, "p2", "/src/p2", "alpha"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	got, err := store.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d projects, want 2", len(got))
	}
	// Ordered by id, so an export of an unchanged database is byte-identical.
	if got[0].ID != "p1" || got[1].ID != "p2" {
		t.Errorf("projects are not ordered by id: %q, %q (names %q, %q)",
			got[0].ID, got[1].ID, got[0].Name, got[1].Name)
	}
	if got[0].RepoRemote != "github.com/wcatz/one" {
		t.Errorf("p1 RepoRemote = %q, want the recorded remote", got[0].RepoRemote)
	}
	if got[0].Name != "zebra" || got[0].Path != "/src/p1" {
		t.Errorf("p1 = %+v, want the recorded name and path", got[0])
	}
	if got[1].RepoRemote != "" {
		t.Errorf("p2 RepoRemote = %q, want empty for a project with no repository", got[1].RepoRemote)
	}
	if got[0].CreatedAt == "" || got[0].UpdatedAt == "" {
		t.Errorf("p1 timestamps = %q/%q, want both recorded", got[0].CreatedAt, got[0].UpdatedAt)
	}
}

// TestPortableMemoriesCarryEveryExportedColumn: the portable read exists
// because the ordinary list readers drop columns a restore needs — the
// validity triple, and the repository remote on the project. This pins the
// whole portable shape, including the two nullable provenance shapes (an
// absent value and a real zero).
func TestPortableMemoriesCarryEveryExportedColumn(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, "p2", "/src/p2", "p2"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, importance, access_count,
			last_accessed, source, tags, pinned, created_at, updated_at, resolved_at,
			valid_from, valid_until, verified_at, agent, session_id, source_ref,
			confidence, scope)
		VALUES ('m1', 'p1', 'gotcha', 'full row', 0.25, 7, '2026-01-02 03:04:05',
			'mcp', '["a","b"]', 1, '2026-01-01 00:00:00', '2026-01-03 00:00:00',
			'2026-01-04 00:00:00', '2026-02-01', '2026-03-01', '2026-01-05',
			'opencode', 'sess-1', 'ref-1', 0.0, '{"environment":"production"}')
	`)
	if err != nil {
		t.Fatalf("seed full row: %v", err)
	}
	if _, err := store.Create(ctx, "p2", Memory{Category: "fact", Content: "other project", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.PortableMemories(ctx, []string{"p1"})
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d memories for p1, want 1 — the filter must exclude other projects", len(got))
	}
	m := got[0]
	if m.ID != "m1" || m.ProjectID != "p1" || m.Category != "gotcha" || m.Content != "full row" {
		t.Errorf("identity fields = %+v", m)
	}
	if m.Importance == nil || *m.Importance != 0.25 || m.AccessCount != 7 || !m.Pinned {
		t.Errorf("axes = importance %v access %d pinned %v", m.Importance, m.AccessCount, m.Pinned)
	}
	if m.LastAccessed == nil || *m.LastAccessed != "2026-01-02 03:04:05" {
		t.Errorf("LastAccessed = %v", m.LastAccessed)
	}
	if m.CreatedAt != "2026-01-01 00:00:00" || m.UpdatedAt != "2026-01-03 00:00:00" {
		t.Errorf("timestamps = %q / %q", m.CreatedAt, m.UpdatedAt)
	}
	if m.ResolvedAt == nil || *m.ResolvedAt != "2026-01-04 00:00:00" {
		t.Errorf("ResolvedAt = %v", m.ResolvedAt)
	}
	if m.ValidFrom == nil || *m.ValidFrom != "2026-02-01" || m.ValidUntil == nil || *m.ValidUntil != "2026-03-01" || m.VerifiedAt == nil || *m.VerifiedAt != "2026-01-05" {
		t.Errorf("validity triple = %v / %v / %v", m.ValidFrom, m.ValidUntil, m.VerifiedAt)
	}
	if m.Agent != "opencode" || m.SessionID != "sess-1" || m.SourceRef != "ref-1" {
		t.Errorf("provenance = %q / %q / %q", m.Agent, m.SessionID, m.SourceRef)
	}
	// A real 0.0 rating is not the same as no rating: nil means "Ghost never
	// learned one", and the portable form has to keep that distinction.
	if m.Confidence == nil || *m.Confidence != 0.0 {
		t.Errorf("Confidence = %v, want a pointer to 0.0", m.Confidence)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "a" || m.Tags[1] != "b" {
		t.Errorf("Tags = %v", m.Tags)
	}
	if len(m.Scope) != 1 || m.Scope["environment"] != "production" {
		t.Errorf("Scope = %v", m.Scope)
	}

	// No project filter at all means every project, in id order.
	all, err := store.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories(nil): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("PortableMemories(nil) returned %d, want every project", len(all))
	}

	// A row with no validity, no provenance and no scope reads as absent
	// rather than as a zero value.
	plain, err := store.PortableMemories(ctx, []string{"p2"})
	if err != nil {
		t.Fatalf("PortableMemories(p2): %v", err)
	}
	if len(plain) != 1 {
		t.Fatalf("got %d memories for p2, want 1", len(plain))
	}
	if plain[0].ValidFrom != nil || plain[0].VerifiedAt != nil || plain[0].Confidence != nil || plain[0].Scope != nil {
		t.Errorf("plain row reported present values: %+v", plain[0])
	}
	if plain[0].ResolvedAt != nil {
		t.Errorf("live row ResolvedAt = %v, want nil", plain[0].ResolvedAt)
	}
}

// TestImportMemoryRoundTripsEveryColumn: an exported row re-inserted under its
// own id must come back identical, in every column the portable form claims to
// carry. A restore that silently reset created_at, pinned or scope would age
// memories, unpin them, and drop where they apply.
func TestImportMemoryRoundTripsEveryColumn(t *testing.T) {
	src := portableTestStore(t)
	ctx := context.Background()
	if err := src.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", "git@github.com:wcatz/one.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if _, err := src.db.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, importance, access_count,
			last_accessed, source, tags, pinned, created_at, updated_at, resolved_at,
			valid_from, valid_until, verified_at, agent, session_id, source_ref,
			confidence, scope)
		VALUES ('m1', 'p1', 'gotcha', 'full row', 0.25, 7, '2026-01-02 03:04:05',
			'mcp', '["a","b"]', 1, '2026-01-01 00:00:00', '2026-01-03 00:00:00',
			'2026-01-04 00:00:00', '2026-02-01', '2026-03-01', '2026-01-05',
			'opencode', 'sess-1', 'ref-1', 0.0, '{"environment":"production"}')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	projects, err := src.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	memories, err := src.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}

	dst := portableTestStore(t)
	if created, err := dst.ImportProject(ctx, projects[0], true); err != nil || !created {
		t.Fatalf("ImportProject = %v, %v; want created", created, err)
	}
	// TrustProvenance, because this test is about the columns: the source and
	// pin rewrite is a policy the default applies on purpose, and asserting the
	// row comes back identical means asserting that policy is off.
	created, clamped, downgraded, err := dst.ImportMemory(ctx, memories[0],
		ImportOptions{Apply: true, TrustProvenance: true})
	if err != nil || !created || clamped || downgraded {
		t.Fatalf("ImportMemory = %v, %v, %v, %v; want created, unclamped, not downgraded", created, clamped, downgraded, err)
	}

	got, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories on destination: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("destination holds %d memories, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0], memories[0]) {
		t.Errorf("round trip changed the row:\n got %+v\nwant %+v", got[0], memories[0])
	}
	// The identity is the exported one, not a fresh hex id — a second import
	// of the same artifact has to be recognised as already present.
	if got[0].ID != "m1" {
		t.Errorf("round trip id = %q, want the exported m1", got[0].ID)
	}
}

// TestImportSkipsAnExistingIDAndNeverOverwrites: re-importing an artifact into
// a store that already has the row must leave it exactly as it was. An import
// that overwrote would be the worst possible failure for a restore, because
// the thing being restored is the older copy.
func TestImportSkipsAnExistingIDAndNeverOverwrites(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(ctx, "p1", Memory{Category: "fact", Content: "newer local text", Source: "manual"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE memories SET id = 'm1' WHERE content = 'newer local text'`); err != nil {
		t.Fatalf("force id: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE memories SET content = 'local wins', pinned = 1 WHERE id = 'm1'`); err != nil {
		t.Fatalf("mark local: %v", err)
	}

	incoming := PortableMemory{
		ID:        "m1",
		ProjectID: "p1",
		Category:  "gotcha",
		Content:   "text from the artifact",
		Source:    "mcp",
		CreatedAt: "2026-01-01 00:00:00",
		UpdatedAt: "2026-01-01 00:00:00",
	}
	created, _, _, err := store.ImportMemory(ctx, incoming, ImportOptions{Apply: true})
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if created {
		t.Error("ImportMemory reported creating a row whose id already exists")
	}
	got, err := store.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d memories, want the single pre-existing row", len(got))
	}
	if got[0].Content != "local wins" || !got[0].Pinned {
		t.Errorf("existing row was overwritten: %+v", got[0])
	}
}

// TestImportDryRunWritesNothing: the default import is a preview, so it must
// validate and classify every record while leaving the store byte-for-byte as
// it found it.
func TestImportDryRunWritesNothing(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	m := PortableMemory{
		ID: "m9", ProjectID: "p1", Category: "gotcha", Content: "would be written",
		Source: "mcp", CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
	}
	created, _, _, err := store.ImportMemory(ctx, m, ImportOptions{})
	if err != nil {
		t.Fatalf("ImportMemory(dry run): %v", err)
	}
	if !created {
		t.Error("a dry run must report what it would create")
	}
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE id = 'm9'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("dry run wrote %d rows", n)
	}
}

// TestImportMemoryClampsOversizedContent: imported content goes through the
// same cap as a normal save. An artifact from another machine can hold a longer
// string than this build accepts, and importing it unclamped would write a row
// the rest of Ghost refuses to produce.
func TestImportMemoryClampsOversizedContent(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	long := strings.Repeat("x", MaxContentLen+500)
	created, clamped, _, err := store.ImportMemory(ctx, PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "gotcha", Content: long, Source: "mcp",
		CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
	}, ImportOptions{Apply: true})
	if err != nil || !created || !clamped {
		t.Fatalf("ImportMemory = %v, %v, %v; want created and clamped", created, clamped, err)
	}
	var content string
	if err := store.db.QueryRowContext(ctx, `SELECT content FROM memories WHERE id = 'm1'`).Scan(&content); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := strings.Repeat("x", MaxContentLen) + TruncationMarker(); content != want {
		t.Errorf("stored content was not clamped to the shared cap:\n got %d bytes ending %q\nwant %d bytes ending %q",
			len(content), content[len(content)-40:], len(want), want[len(want)-40:])
	}
}

// TestImportMemoryRejectsInvalidRecords: a hand-edited or truncated artifact
// must be refused per record, with a message naming the field and the values
// that would have been accepted, and must not write a row the schema CHECK or
// the foreign key would have rejected halfway.
//
// The assertion looks for the import's own wording rather than the field name
// alone. The schema's CHECK constraint would reject these rows too, and
// modernc's message for that quotes the CHECK expression — which contains the
// word "category", so a field-name assertion would pass on the wrong rejection
// and would leave the Go validation untested. "must be one of" appears only in
// the message Ghost builds.
func TestImportMemoryRejectsInvalidRecords(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	cases := []struct {
		name string
		rec  PortableMemory
		want string
	}{
		{
			name: "unknown category",
			rec: PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "not-a-category", Content: "x", Source: "mcp",
			},
			want: "must be one of",
		},
		{
			name: "unknown source",
			rec: PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "telepathy",
			},
			want: "must be one of",
		},
		{
			name: "unknown project",
			rec: PortableMemory{
				ID: "m1", ProjectID: "nope", Category: "fact", Content: "x", Source: "mcp",
			},
			want: "project",
		},
		{
			name: "empty id",
			rec: PortableMemory{
				ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
			},
			want: "id",
		},
		{
			name: "empty content",
			rec: PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			},
			want: "content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := store.ImportMemory(ctx, tc.rec, ImportOptions{Apply: true}); err == nil {
				t.Fatalf("ImportMemory must reject %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must say %q, got %v", tc.want, err)
			}
			// A dry run has to reach the same verdict: a preview that reported
			// "would create" for a row the write would reject is a preview the
			// user cannot act on. The missing-project case is the one exception
			// and is covered by the portable package, which is the caller that
			// knows what the same run will have created — see ImportMemory's
			// comment on apply=false.
			if tc.want != "project" {
				created, _, _, err := store.ImportMemory(ctx, tc.rec, ImportOptions{})
				if err == nil {
					t.Errorf("a dry run accepted %s (created=%v)", tc.name, created)
				}
			}
			var n int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("a rejected record wrote %d rows", n)
			}
		})
	}
}

// TestImportMemoryDefaultsAbsentTimestampsAndImportance: a hand-edited record
// that leaves out created_at must not be stored with the empty string. The
// column is `NOT NULL DEFAULT (datetime('now'))`, but a value bound for a column
// never lets its default apply — and julianday(”) is NULL, so the whole
// time-decay expression is NULL and the memory sorts last out of every ranked
// read. It would look perfectly present in the store and be invisible to recall,
// which is the worst shape a bad import can take. Same for importance: an absent
// field takes the column's 0.5 rather than a bound 0.0, which would read as
// "worth nothing" where the record said nothing at all.
func TestImportMemoryDefaultsAbsentTimestampsAndImportance(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := store.ImportMemory(ctx, PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	var created, updated string
	var importance float64
	if err := store.db.QueryRowContext(ctx,
		`SELECT created_at, updated_at, importance FROM memories WHERE id = 'm1'`).
		Scan(&created, &updated, &importance); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if created == "" || updated == "" {
		t.Errorf("timestamps = %q / %q, want the column's now() default rather than an empty string", created, updated)
	}
	// The stored timestamp has to be a real one the decay expression can
	// compute on, not merely a non-empty string.
	var decidable int
	if err := store.db.QueryRowContext(ctx,
		`SELECT CASE WHEN julianday(?) IS NOT NULL THEN 1 ELSE 0 END`, created).Scan(&decidable); err != nil {
		t.Fatalf("decay probe: %v", err)
	}
	if decidable != 1 {
		t.Errorf("created_at %q makes julianday() NULL, so the row sorts last in every ranked read", created)
	}
	if importance != 0.5 {
		t.Errorf("importance = %v, want the column default 0.5 for a record that stated none", importance)
	}
}

// TestImportMemoryDistinguishesAStatedZeroImportanceFromAnAbsentOne: a memory
// saved without an importance is stored as 0 — `Store.Create` binds the field
// with no default — and exported as `"importance":0`. Re-importing that must
// store 0 again, not the column's 0.5: promoting it is a silent rewrite of the
// artifact's own content, which is the one thing the format promises not to do.
// Only a record that states no importance at all takes the default.
func TestImportMemoryDistinguishesAStatedZeroImportanceFromAnAbsentOne(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A row written the way Create writes one when the caller states nothing: 0.
	if _, err := store.Create(ctx, "p1", Memory{Category: "fact", Content: "saved without an importance", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	exported, err := store.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(exported) != 1 || exported[0].Importance == nil {
		t.Fatalf("export = %+v, want one memory with a stated importance", exported)
	}
	if *exported[0].Importance != 0 {
		t.Fatalf("exported importance = %v, want the stored 0", *exported[0].Importance)
	}
	// The artifact carries it, so the round trip must preserve it.
	dst := portableTestStore(t)
	if _, err := dst.ImportProject(ctx, PortableProject{ID: "p1", Path: "/src/p1", Name: "p1"}, true); err != nil {
		t.Fatalf("ImportProject: %v", err)
	}
	if _, _, _, err := dst.ImportMemory(ctx, exported[0], ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	var got float64
	if err := dst.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, exported[0].ID).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != 0 {
		t.Errorf("imported importance = %v, want the 0 the artifact stated", got)
	}

	// A record that states none is a different case, and takes the default.
	if _, _, _, err := dst.ImportMemory(ctx, PortableMemory{
		ID: "m-absent", ProjectID: "p1", Category: "fact", Content: "x", Source: "mcp",
	}, ImportOptions{Apply: true}); err != nil {
		t.Fatalf("ImportMemory(absent): %v", err)
	}
	if err := dst.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = 'm-absent'`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != 0.5 {
		t.Errorf("imported importance = %v for a record that stated none, want the column default 0.5", got)
	}
}

// TestImportProjectAcceptsARemoteLessProjectBesideOtherRemoteLessProjects: a
// project with no repository is the ordinary shape — every project created by an
// MCP save records an empty remote — so importing one must not be read as a
// collision with every other project that has no repository either. The store
// treats NULL and ” as the same "no remote" and its partial UNIQUE index
// deliberately excludes them, so a check that compared against ” would refuse an
// ordinary import and name an unrelated project and an empty repository.
func TestImportProjectAcceptsARemoteLessProjectBesideOtherRemoteLessProjects(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := store.EnsureProject(ctx, id, "/src/"+id, id); err != nil {
			t.Fatalf("EnsureProject(%s): %v", id, err)
		}
	}
	created, err := store.ImportProject(ctx, PortableProject{
		ID: "d", Path: "/src/d", Name: "d", RepoRemote: "",
	}, true)
	if err != nil {
		t.Fatalf("ImportProject of a remote-less project: %v", err)
	}
	if !created {
		t.Error("a remote-less project was not created")
	}
	projects, err := store.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(projects) != 4 {
		t.Errorf("store holds %d projects, want the 3 seeded plus the imported one", len(projects))
	}

	// The same shape spelled as an unrecognizable remote — a bare host, a
	// filesystem path — normalizes to "" and must be treated as no remote rather
	// than as a repository named "".
	created, err = store.ImportProject(ctx, PortableProject{
		ID: "e", Path: "/src/e", Name: "e", RepoRemote: "not a remote",
	}, true)
	if err != nil {
		t.Fatalf("ImportProject with an unrecognizable remote: %v", err)
	}
	if !created {
		t.Error("a project whose remote does not normalize was not created")
	}
}

// TestImportMemoryDowngradesProvenanceUnlessTrusted: an artifact is a file
// that arrived from somewhere, and on its own authority it must not be able to
// plant rows that read as the user's own material (`manual`) or as Ghost's
// shipped rules (`builtin`) — both of which the store exempts from consolidation
// — nor rows that are pinned, which is exempt whatever their source.
//
// The downgrade happens before the id check, so a dry run reports the rewrite it
// would make rather than the one the artifact asked for.
func TestImportMemoryDowngradesProvenanceUnlessTrusted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		pinned     bool
		trust      bool
		wantSource string
		wantPinned bool
	}{
		{name: "manual", source: "manual", wantSource: DowngradedSource},
		{name: "builtin", source: "builtin", wantSource: DowngradedSource},
		{name: "pinned mcp", source: "mcp", pinned: true, wantSource: DowngradedSource},
		{name: "reflection", source: "reflection", wantSource: DowngradedSource},
		{name: "manual, trusted", source: "manual", trust: true, wantSource: "manual"},
		{name: "builtin, trusted", source: "builtin", trust: true, wantSource: "builtin"},
		{name: "pinned mcp, trusted", source: "mcp", pinned: true, trust: true, wantSource: "mcp", wantPinned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := portableTestStore(t)
			ctx := context.Background()
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			rec := PortableMemory{
				ID: "m1", ProjectID: "p1", Category: "fact", Content: "from an artifact",
				Source: tc.source, Pinned: tc.pinned,
				CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
			}
			created, _, downgraded, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true, TrustProvenance: tc.trust})
			if err != nil || !created {
				t.Fatalf("ImportMemory = %v, %v; want created", created, err)
			}
			wantDowngraded := tc.source != tc.wantSource || tc.pinned != tc.wantPinned
			if downgraded != wantDowngraded {
				t.Errorf("downgraded = %v, want %v", downgraded, wantDowngraded)
			}
			var gotSource string
			var gotPinned int
			if err := store.db.QueryRowContext(ctx,
				`SELECT source, pinned FROM memories WHERE id = 'm1'`).Scan(&gotSource, &gotPinned); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if gotSource != tc.wantSource {
				t.Errorf("source = %q, want %q", gotSource, tc.wantSource)
			}
			if (gotPinned == 1) != tc.wantPinned {
				t.Errorf("pinned = %v, want %v", gotPinned == 1, tc.wantPinned)
			}
		})
	}
}

// TestImportedMemoriesAreConsolidatable: the point of downgrading is not only
// that the provenance reads honestly but that the rows stay ordinary. Both
// `manual` and `builtin` are excluded from ReplaceNonManual by name, and a
// pinned row is excluded whatever its source — so a planted row that kept either
// would be permanently exempt from consolidation. This asserts the exclusion
// itself rather than the label, because the label is an implementation detail of
// the same rule.
func TestImportedMemoriesAreConsolidatable(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Every source the artifact could claim, pinned and not, as a user would
	// write them.
	for i, source := range []string{"manual", "builtin", "mcp", "chat", "reflection", "decision_log"} {
		rec := PortableMemory{
			ID:        fmt.Sprintf("m%d", i),
			ProjectID: "p1", Category: "fact", Content: "planted " + source,
			Source: source, Pinned: true,
			CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
		}
		if _, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true}); err != nil {
			t.Fatalf("ImportMemory(%s): %v", source, err)
		}
	}
	var exempt int
	if err := store.db.QueryRowContext(ctx, `
		SELECT count(*) FROM memories
		WHERE source IN ('manual', 'builtin') OR pinned = 1`).Scan(&exempt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if exempt != 0 {
		t.Errorf("%d imported rows are exempt from consolidation — an untrusted artifact must not be able to plant any", exempt)
	}
	// And the two the store itself writes, for contrast, so the assertion above
	// is about the import and not about a store that never pins anything.
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	var seeds int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE source = 'builtin' AND pinned = 1`).Scan(&seeds); err != nil {
		t.Fatalf("count seeds: %v", err)
	}
	if seeds == 0 {
		t.Error("no builtin seed row is pinned, so the check above would pass against any store")
	}
}

// TestImportMemoryDryRunReportsTheDowngradeItWouldMake: the preview exists so
// the user can read what will happen before it happens, and the provenance
// rewrite is the part of that a reader cannot verify afterwards — the stored
// source is the downgraded one, so the artifact's own value is gone from the
// store entirely.
func TestImportMemoryDryRunReportsTheDowngradeItWouldMake(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	rec := PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Content: "x", Source: "builtin", Pinned: true,
	}
	created, _, downgraded, err := store.ImportMemory(ctx, rec, ImportOptions{})
	if err != nil {
		t.Fatalf("ImportMemory(dry run): %v", err)
	}
	if !created {
		t.Error("a dry run must report what it would create")
	}
	if !downgraded {
		t.Error("a dry run must report the provenance downgrade it would make")
	}
	// With the flag, the dry run says nothing is being downgraded.
	_, _, downgraded, err = store.ImportMemory(ctx, rec, ImportOptions{TrustProvenance: true})
	if err != nil {
		t.Fatalf("ImportMemory(dry run, trusted): %v", err)
	}
	if downgraded {
		t.Error("--trust-provenance must not report a downgrade")
	}
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a dry run wrote %d rows", n)
	}
}

// TestImportMemoryClampsImportance: a normal save clamps importance to [0,1]
// before the write. An import that skipped that would be the only way an
// importance outside the range reaches the database.
func TestImportMemoryClampsImportance(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, tc := range []struct {
		in   float64
		want float64
	}{{in: 4.2, want: 1}, {in: -3, want: 0}, {in: 0, want: 0}} {
		in := tc.in
		rec := PortableMemory{
			ID: "m-" + string(rune('a'+int(tc.in))), ProjectID: "p1", Category: "fact",
			Content: "x", Source: "mcp", Importance: &in,
		}
		if _, _, _, err := store.ImportMemory(ctx, rec, ImportOptions{Apply: true}); err != nil {
			t.Fatalf("ImportMemory(%v): %v", tc.in, err)
		}
		var got float64
		if err := store.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, rec.ID).Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got != tc.want {
			t.Errorf("imported importance %v stored as %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestImportProjectSkipsExistingAndNeverRebinds: importing a project whose id
// already exists must not move it. A recorded path is a project's identity for
// every other command, and rewriting it from a stale artifact would re-point a
// project at a checkout that is no longer its own.
func TestImportProjectSkipsExistingAndNeverRebinds(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProjectWithRepo(ctx, "p1", "/src/local", "local name", "git@github.com:wcatz/local.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	created, err := store.ImportProject(ctx, PortableProject{
		ID: "p1", Path: "/src/from-artifact", Name: "artifact name", RepoRemote: "git@github.com:wcatz/artifact.git",
	}, true)
	if err != nil {
		t.Fatalf("ImportProject: %v", err)
	}
	if created {
		t.Error("ImportProject reported creating a project whose id already exists")
	}
	got, err := store.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d projects, want 1", len(got))
	}
	if got[0].Path != "/src/local" || got[0].Name != "local name" || got[0].RepoRemote != "github.com/wcatz/local" {
		t.Errorf("existing project was rebound: %+v", got[0])
	}

	// A project the store has never seen is created, remote and all.
	created, err = store.ImportProject(ctx, PortableProject{
		ID: "p2", Path: "/src/p2", Name: "two", RepoRemote: "git@github.com:wcatz/two.git",
	}, true)
	if err != nil || !created {
		t.Fatalf("ImportProject(new) = %v, %v; want created", created, err)
	}
	all, err := store.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(all) != 2 || all[1].ID != "p2" || all[1].RepoRemote != "github.com/wcatz/two" {
		t.Errorf("imported project = %+v", all)
	}
}

// findDecision reads one decision back by id, the only decision read the store
// offers being a per-project list.
func findDecision(t *testing.T, s *Store, id string) Decision {
	t.Helper()
	list, err := s.ListDecisions(context.Background(), "p1", "", 100)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	for _, d := range list {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("decision %s not found in %+v", id, list)
	return Decision{}
}

// TestImportTaskAndDecisionRoundTrip: tasks and decisions carry state the
// ordinary writers cannot set — a done task's completed_at, a superseded
// decision's status — so their import has to preserve it under the same id.
func TestImportTaskAndDecisionRoundTrip(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	task := Task{
		ID: "t1", ProjectID: "p1", Title: "ship it", Description: "desc", Status: "done",
		Priority: 4, Branch: "feat/x", PRNumber: 42, Notes: "done",
		CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-02 00:00:00",
		CompletedAt: "2026-01-02 00:00:00",
	}
	created, err := store.ImportTask(ctx, task, true)
	if err != nil || !created {
		t.Fatalf("ImportTask = %v, %v; want created", created, err)
	}
	gotTask, err := store.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.Status != "done" || gotTask.Priority != 4 || gotTask.Branch != "feat/x" || gotTask.PRNumber != 42 || gotTask.CompletedAt != "2026-01-02 00:00:00" {
		t.Errorf("imported task = %+v", gotTask)
	}

	// The superseding decision is written first: superseded_by is a
	// self-reference, and the store refuses a pointer to a decision that is not
	// present rather than failing the INSERT with SQLite's own wording.
	dec := Decision{
		ID: "d2", ProjectID: "p1", Title: "newer", Decision: "d2", Rationale: "why2",
		Status: "active", CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00",
	}
	created, err = store.ImportDecision(ctx, dec, true)
	if err != nil || !created {
		t.Fatalf("ImportDecision(d2) = %v, %v; want created", created, err)
	}
	dec = Decision{
		ID: "d1", ProjectID: "p1", Title: "t", Decision: "d", Alternatives: []string{"a", "b"},
		Rationale: "why", Status: "superseded", SupersededBy: "d2", Tags: []string{"x"},
		CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-02 00:00:00",
	}
	created, err = store.ImportDecision(ctx, dec, true)
	if err != nil || !created {
		t.Fatalf("ImportDecision = %v, %v; want created", created, err)
	}
	// A pointer to a decision the store does not hold names the field rather
	// than surfacing a foreign-key failure.
	if _, err := store.ImportDecision(ctx, Decision{
		ID: "d3", ProjectID: "p1", Title: "t", Decision: "d", Rationale: "r",
		Status: "superseded", SupersededBy: "absent",
	}, true); err == nil || !strings.Contains(err.Error(), "superseded_by") {
		t.Errorf("ImportDecision with a dangling superseded_by = %v, want an error naming the field", err)
	}
	gotDec := findDecision(t, store, "d1")
	if gotDec.Status != "superseded" || gotDec.SupersededBy != "d2" || len(gotDec.Alternatives) != 2 || len(gotDec.Tags) != 1 {
		t.Errorf("imported decision = %+v", gotDec)
	}

	// Re-importing reports a skip and changes nothing.
	created, err = store.ImportTask(ctx, task, true)
	if err != nil || created {
		t.Errorf("ImportTask of an existing id = %v, %v; want skipped", created, err)
	}
	created, err = store.ImportDecision(ctx, dec, true)
	if err != nil || created {
		t.Errorf("ImportDecision of an existing id = %v, %v; want skipped", created, err)
	}
}

// TestImportProjectReportsACheckoutOrRemoteCollision: projects.path is UNIQUE
// and repo_remote carries a partial UNIQUE index, so an artifact naming a
// checkout or a repository this store already records cannot be inserted. The
// id check alone does not see it — project ids are per-install, so a
// cross-machine import collides here routinely — and the INSERT's bare
// "UNIQUE constraint failed" is the worst possible diagnosis for the expected
// case. Both modes must name the project it collides with, so a dry run cannot
// promise a create the write would refuse.
func TestImportProjectReportsACheckoutOrRemoteCollision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		local PortableProject
		in    PortableProject
	}{
		{
			name:  "same path",
			local: PortableProject{ID: "local", Path: "/src/thing", Name: "thing"},
			in:    PortableProject{ID: "other", Path: "/src/thing", Name: "thing"},
		},
		{
			name:  "same repository, different path",
			local: PortableProject{ID: "local", Path: "/src/thing", Name: "thing", RepoRemote: "github.com/wcatz/thing"},
			in:    PortableProject{ID: "other", Path: "/src/elsewhere", Name: "thing", RepoRemote: "git@github.com:wcatz/thing.git"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, apply := range []bool{false, true} {
				store := portableTestStore(t)
				ctx := context.Background()
				if _, err := store.ImportProject(ctx, tc.local, true); err != nil {
					t.Fatalf("seed the local project: %v", err)
				}
				_, err := store.ImportProject(ctx, tc.in, apply)
				if err == nil {
					t.Fatalf("apply=%v: ImportProject promised a project another already records", apply)
				}
				if !strings.Contains(err.Error(), "local") {
					t.Errorf("apply=%v: error = %v, want it to name the colliding project", apply, err)
				}
				projects, lErr := store.PortableProjects(ctx)
				if lErr != nil {
					t.Fatalf("PortableProjects: %v", lErr)
				}
				if len(projects) != 1 {
					t.Errorf("apply=%v: a refused project still left %d rows", apply, len(projects))
				}
			}
		})
	}
}

// TestProjectForCheckoutMatchesTheRecordedIdentity: the lookup the importer uses
// to attach an artifact's records to the project this store already has. A
// recorded path is the stronger claim — it is the directory a session in it
// resolves to — so it is consulted first, and the remote is compared in
// normalized form because that is the form stored. `_global` is never returned:
// it holds every project's memories rather than one checkout, so adopting it
// would put a project's records in the global scope.
func TestProjectForCheckoutMatchesTheRecordedIdentity(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProjectWithRepo(ctx, "byremote", "/src/a", "a", "git@github.com:wcatz/thing.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if err := store.EnsureProjectWithRepo(ctx, "bypath", "/src/b", "b", "github.com:wcatz/other"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	// The remote is matched in normalized form, so two spellings of one
	// repository are one answer.
	got, name, err := store.ProjectForCheckout(ctx, "", "https://github.com/wcatz/thing")
	if err != nil {
		t.Fatalf("ProjectForCheckout(remote): %v", err)
	}
	if got != "byremote" || name != "a" {
		t.Errorf("remote lookup = (%q, %q), want the project recording that repository", got, name)
	}
	// The path wins over the remote when both could match something: a session in
	// that directory resolves to the project that records it.
	got, _, err = store.ProjectForCheckout(ctx, "/src/b", "github.com/wcatz/thing")
	if err != nil {
		t.Fatalf("ProjectForCheckout(path): %v", err)
	}
	if got != "bypath" {
		t.Errorf("path lookup = %q, want the project recording that directory", got)
	}
	// Nothing recorded is a miss, not an error.
	got, _, err = store.ProjectForCheckout(ctx, "/nowhere", "github.com:wcatz/nowhere")
	if err != nil || got != "" {
		t.Errorf("unknown checkout = (%q, %v), want a miss", got, err)
	}
	// `_global` records the literal path "_global" and no remote, so a lookup for
	// it must not come back as the global project to attach to.
	got, _, err = store.ProjectForCheckout(ctx, "_global", "")
	if err != nil {
		t.Fatalf("ProjectForCheckout(_global path): %v", err)
	}
	if got == "_global" {
		t.Error("_global must never be returned as a project to attach records to")
	}
}

// TestImportTaskAndDecisionRejectInvalidState: status and priority are CHECK
// constraints, so a hand-edited artifact carrying values this build does not
// accept has to be refused with Ghost's own message. Asserting for the field
// name alone would pass on the schema's rejection — modernc quotes the CHECK
// expression, which contains the field name — and would leave the Go validation
// untested, so the wording that only Ghost produces is what is asserted.
func TestImportTaskAndDecisionRejectInvalidState(t *testing.T) {
	store := portableTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.ImportTask(ctx, Task{
		ID: "t1", ProjectID: "p1", Title: "x", Status: "sideways", Priority: 2,
	}, true); err == nil || !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("ImportTask with a bad status = %v, want an error listing the accepted values", err)
	}
	if _, err := store.ImportTask(ctx, Task{
		ID: "t1", ProjectID: "p1", Title: "x", Status: "pending", Priority: 99,
	}, true); err == nil || !strings.Contains(err.Error(), "must be between") {
		t.Errorf("ImportTask with a bad priority = %v, want an error naming the range", err)
	}
	if _, err := store.ImportDecision(ctx, Decision{
		ID: "d1", ProjectID: "p1", Title: "t", Decision: "d", Rationale: "r", Status: "maybe",
	}, true); err == nil || !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("ImportDecision with a bad status = %v, want an error listing the accepted values", err)
	}
	// A dry run refuses them too, so the preview cannot promise a write that
	// would fail.
	if _, err := store.ImportTask(ctx, Task{
		ID: "t1", ProjectID: "p1", Title: "x", Status: "sideways", Priority: 2,
	}, false); err == nil {
		t.Error("a dry run accepted a task with a bad status")
	}
	if _, err := store.ImportDecision(ctx, Decision{
		ID: "d1", ProjectID: "p1", Title: "t", Decision: "d", Rationale: "r", Status: "maybe",
	}, false); err == nil {
		t.Error("a dry run accepted a decision with a bad status")
	}
	// A task naming a project this store does not hold is refused by name rather
	// than surfacing as a foreign-key failure from the INSERT. Only the apply
	// path checks it: a dry run cannot know what the same run will have created,
	// which is the caller's question (see ImportMemory's comment on apply=false).
	if _, err := store.ImportTask(ctx, Task{
		ID: "t9", ProjectID: "absent", Title: "x", Status: "pending", Priority: 2,
	}, true); err == nil {
		t.Error("ImportTask accepted a task naming an absent project")
	} else if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error = %v, want it to name the missing project", err)
	}
	if _, err := store.ImportDecision(ctx, Decision{
		ID: "d9", ProjectID: "absent", Title: "t", Decision: "d", Rationale: "r", Status: "active",
	}, true); err == nil {
		t.Error("ImportDecision accepted a decision naming an absent project")
	} else if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error = %v, want it to name the missing project", err)
	}
	var n int
	if err := store.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM tasks) + (SELECT count(*) FROM decisions)`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a rejected record wrote %d rows", n)
	}
}
