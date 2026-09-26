package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// newTestStore opens a store over a temp-dir database with the logger silenced
// so a failing case does not bury its own message.
func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// seed fills a store with one project, two memories covering the shapes a
// round trip has to preserve (provenance, scope, pin, resolved_at), a task and
// a decision, and returns the project id.
func seed(t *testing.T, s *memory.Store) string {
	t.Helper()
	ctx := context.Background()
	if err := s.EnsureProjectWithRepo(ctx, "p1", "/src/p1", "one", "git@github.com:wcatz/one.git"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	conf := 0.5
	if _, err := s.Create(ctx, "p1", memory.Memory{Category: "gotcha", Content: "first", Source: "manual", Importance: 0.4}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := s.Create(ctx, "p1", memory.Memory{
		Category: "architecture", Content: "second", Source: "mcp", Importance: 0.9,
		Pinned: true, Tags: []string{"a", "b"}, Agent: "opencode", SessionID: "s1",
		SourceRef: "r1", Confidence: &conf, Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{second}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if _, err := s.CreateTask(ctx, "p1", "ship", "the thing", 4); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, _, _, err := s.RecordDecision(ctx, "p1", "title", "decision", "why", []string{"alt"}, nil); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return "p1"
}

// exportBytes runs an export into a buffer and fails the test on error.
func exportBytes(t *testing.T, s *memory.Store, projectFilter string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Export(context.Background(), s, &buf, projectFilter); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return buf.Bytes()
}

// TestExportImportRoundTripIntoAnEmptyStore: the artifact has to carry every
// entity across into a store that has never seen it. This is the whole point of
// the format, so it is asserted on the stores rather than on the bytes: an
// export that merely looked right on disk would pass a shape check and still
// lose the store.
func TestExportImportRoundTripIntoAnEmptyStore(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)
	artifact := exportBytes(t, src, "")

	dst := newTestStore(t)
	report, err := Import(context.Background(), dst, bytes.NewReader(artifact), true, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 0 {
		t.Fatalf("import rejected %d record(s): %v", report.Rejected, report.Errors)
	}
	if report.Created["project"] != 1 || report.Created["task"] != 1 {
		t.Errorf("created = %v, want one project and one task", report.Created)
	}
	// RecordDecision also writes a companion memory, so the source store holds
	// three memories, not the two that were saved directly.
	if report.Created["memory"] != 3 {
		t.Errorf("created memories = %d, want 3 (two saved plus the decision companion)", report.Created["memory"])
	}

	ctx := context.Background()
	srcProjects, err := src.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects(src): %v", err)
	}
	dstProjects, err := dst.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects(dst): %v", err)
	}
	if len(srcProjects) != len(dstProjects) {
		t.Fatalf("projects after round trip = %d, want %d", len(dstProjects), len(srcProjects))
	}
	for i := range srcProjects {
		if srcProjects[i] != dstProjects[i] {
			t.Errorf("project %d differs:\n src %+v\n dst %+v", i, srcProjects[i], dstProjects[i])
		}
	}

	srcMemories, err := src.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories(src): %v", err)
	}
	dstMemories, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories(dst): %v", err)
	}
	if len(srcMemories) != len(dstMemories) || len(dstMemories) != 3 {
		t.Fatalf("memories after round trip = %d, want %d", len(dstMemories), len(srcMemories))
	}
	for i := range srcMemories {
		if !reflect.DeepEqual(srcMemories[i], dstMemories[i]) {
			t.Errorf("memory %d differs:\n src %+v\n dst %+v", i, srcMemories[i], dstMemories[i])
		}
	}

	srcTasks, err := src.ListTasks(ctx, "p1", "", 100)
	if err != nil {
		t.Fatalf("ListTasks(src): %v", err)
	}
	dstTasks, err := dst.ListTasks(ctx, "p1", "", 100)
	if err != nil {
		t.Fatalf("ListTasks(dst): %v", err)
	}
	if len(srcTasks) != len(dstTasks) || len(dstTasks) != 1 {
		t.Fatalf("tasks after round trip = %d, want %d", len(dstTasks), len(srcTasks))
	}
	if !reflect.DeepEqual(srcTasks[0], dstTasks[0]) {
		t.Errorf("task differs:\n src %+v\n dst %+v", srcTasks[0], dstTasks[0])
	}

	srcDecisions, err := src.ListDecisions(ctx, "p1", "", 100)
	if err != nil {
		t.Fatalf("ListDecisions(src): %v", err)
	}
	dstDecisions, err := dst.ListDecisions(ctx, "p1", "", 100)
	if err != nil {
		t.Fatalf("ListDecisions(dst): %v", err)
	}
	if len(srcDecisions) != len(dstDecisions) || len(dstDecisions) != 1 {
		t.Fatalf("decisions after round trip = %d, want %d", len(dstDecisions), len(srcDecisions))
	}
	if !reflect.DeepEqual(srcDecisions[0], dstDecisions[0]) {
		t.Errorf("decision differs:\n src %+v\n dst %+v", srcDecisions[0], dstDecisions[0])
	}
}

// TestExportIsDeterministic: two exports of an unchanged store must be
// byte-identical, or the artifact cannot be diffed, checksummed against a
// previous copy, or reviewed in a pull request.
func TestExportIsDeterministic(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)
	first := exportBytes(t, src, "")
	second := exportBytes(t, src, "")
	if !bytes.Equal(first, second) {
		t.Error("two exports of an unchanged store differ")
	}
	// The header carries the version and nothing else. Asserted byte-exactly
	// rather than by looking for a key, because a timestamp or a record count
	// added here would break byte-reproducibility while every field the test
	// looked for would still be there.
	head := strings.SplitN(string(first), "\n", 2)[0]
	if want := `{"type":"header","schema_version":1}`; head != want {
		t.Errorf("header line = %s, want exactly %s", head, want)
	}
}

// TestExportProjectFilterIsExact: --project selects by name or id. Anything
// else has to be an error naming what was asked for, never a silently empty
// artifact that reads as "there is nothing to export".
func TestExportProjectFilterIsExact(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)

	byName := exportBytes(t, src, "one")
	if !bytes.Contains(byName, []byte(`"name":"one"`)) {
		t.Error("export by name did not include the named project")
	}
	byID := exportBytes(t, src, "p1")
	if !bytes.Equal(byID, byName) {
		t.Error("export by id and by name must select the same project")
	}
	if _, err := Export(context.Background(), src, &bytes.Buffer{}, "nope"); err == nil {
		t.Error("an unmatched project filter must be an error, not an empty export")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error must name the filter it could not match, got %v", err)
	}
}

// TestImportSkipsExistingIDs: importing the same artifact twice must leave the
// second run a no-op. An import that duplicated on a second run would make the
// command unsafe to retry after an interrupted first one.
func TestImportSkipsExistingIDs(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)
	artifact := exportBytes(t, src, "")

	dst := newTestStore(t)
	ctx := context.Background()
	if _, err := Import(ctx, dst, bytes.NewReader(artifact), true, nil); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	before, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}

	var seen []RecordResult
	report, err := Import(ctx, dst, bytes.NewReader(artifact), true, func(r RecordResult) { seen = append(seen, r) })
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if report.Created["memory"] != 0 || report.Created["project"] != 0 || report.Created["task"] != 0 {
		t.Errorf("second import created %v, want nothing", report.Created)
	}
	if report.Skipped["memory"] != 3 {
		t.Errorf("second import skipped %d memories, want all 3", report.Skipped["memory"])
	}
	if report.Rejected != 0 {
		t.Errorf("second import rejected %d records: %v", report.Rejected, report.Errors)
	}
	after, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("second import changed the memory count from %d to %d", len(before), len(after))
	}
	// Every record is reported to the caller, and a skip says so rather than
	// looking like a silent success.
	if len(seen) == 0 {
		t.Fatal("no per-record results were reported")
	}
	for _, r := range seen {
		if r.Action == ActionCreate {
			t.Errorf("record %s/%s reported create on a re-import", r.Type, r.ID)
		}
	}
}

// TestImportDryRunWritesNothing: the default is a preview. It must classify
// every record — created, skipped, rejected — and leave the store exactly as it
// found it, so the report can be trusted before anything is applied.
func TestImportDryRunWritesNothing(t *testing.T) {
	src := newTestStore(t)
	seed(t, src)
	artifact := exportBytes(t, src, "")

	dst := newTestStore(t)
	ctx := context.Background()
	// One memory already present, so the preview has both outcomes to report.
	if err := dst.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	var seen []RecordResult
	report, err := Import(ctx, dst, bytes.NewReader(artifact), false, func(r RecordResult) { seen = append(seen, r) })
	if err != nil {
		t.Fatalf("Import(dry run): %v", err)
	}
	if report.Applied {
		t.Error("a dry run must report Applied = false")
	}
	if report.Created["memory"] == 0 {
		t.Error("a dry run must still report what it would create")
	}
	if len(seen) == 0 {
		t.Error("a dry run must report per record")
	}
	for _, r := range seen {
		if r.Action == ActionCreate {
			continue
		}
		if r.Action != ActionSkip && r.Action != ActionReject {
			t.Errorf("unexpected action %q for %s/%s", r.Action, r.Type, r.ID)
		}
	}

	projects, err := dst.PortableProjects(ctx)
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	// The one project is the one the test created; a dry run added nothing.
	if len(projects) != 1 {
		t.Errorf("a dry run left %d projects, want the 1 the test created", len(projects))
	}
	memories, err := dst.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(memories) != 0 {
		t.Errorf("a dry run wrote %d memories, want none", len(memories))
	}
}

// TestImportRejectsAnUnknownSchemaVersion: an artifact this build cannot read
// must be refused outright. Reading it anyway would insert records whose fields
// mean whatever this build guesses, which is the one outcome an import must
// never produce.
func TestImportRejectsAnUnknownSchemaVersion(t *testing.T) {
	for _, version := range []string{"2", "99", "0"} {
		t.Run("version "+version, func(t *testing.T) {
			store := newTestStore(t)
			body := `{"type":"header","schema_version":` + version + "}\n" +
				`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n"
			_, err := Import(context.Background(), store, strings.NewReader(body), true, nil)
			if err == nil {
				t.Fatalf("import accepted schema version %s", version)
			}
			if !strings.Contains(err.Error(), version) {
				t.Errorf("error must name the version it refused, got %v", err)
			}
			projects, lErr := store.PortableProjects(context.Background())
			if lErr != nil {
				t.Fatalf("PortableProjects: %v", lErr)
			}
			if len(projects) != 0 {
				t.Errorf("a refused artifact still wrote %d rows", len(projects))
			}
		})
	}
}

// TestImportRejectsAMalformedOrMisorderedArtifact: a file that is not an
// artifact, or whose header is not first, has to fail before any record is
// applied. Silently skipping the header would accept a file whose version nobody
// checked.
func TestImportRejectsAMalformedOrMisorderedArtifact(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "empty file",
			body: "",
			want: "nothing to import",
		},
		{
			name: "no header",
			body: `{"type":"project","project":{"id":"p1","path":"/p","name":"p"}}` + "\n",
			want: "header",
		},
		{
			name: "header after a record",
			body: `{"type":"project","project":{"id":"p1","path":"/p","name":"p"}}` + "\n" +
				`{"type":"header","schema_version":1}` + "\n",
			want: "header",
		},
		{
			name: "unparseable first line",
			body: "{not json\n" + `{"type":"header","schema_version":1}` + "\n",
			want: "line 1",
		},
		{
			name: "unknown record type",
			body: `{"type":"header","schema_version":1}` + "\n" +
				`{"type":"souvenir","id":"x"}` + "\n",
			want: "souvenir",
		},
		{
			name: "record with no payload",
			body: `{"type":"header","schema_version":1}` + "\n" + `{"type":"memory"}` + "\n",
			want: "memory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			_, err := Import(context.Background(), store, strings.NewReader(tc.body), true, nil)
			if err == nil {
				t.Fatal("import accepted a file that is not a readable artifact")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			projects, lErr := store.PortableProjects(context.Background())
			if lErr != nil {
				t.Fatalf("PortableProjects: %v", lErr)
			}
			if len(projects) != 0 {
				t.Errorf("a refused file still wrote %d projects", len(projects))
			}
		})
	}
}

// TestImportAppliesTheRecordsAroundAnUnreadableLine: the resilience the
// line-oriented format is chosen for. A line that is not JSON is rejected on its
// own and the rest of the file still imports — which is what makes a truncated
// artifact worth keeping, and what makes the package's claim true rather than
// aspirational. The rejections are counted and named, so a partial import is
// never silent.
func TestImportAppliesTheRecordsAroundAnUnreadableLine(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	// A good project, a good memory, the truncated middle of a record, then
	// another good memory.
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"before","project_id":"p1","category":"fact","content":"kept","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"cut","project_id":"p1","cat` + "\n" +
		`{"type":"memory","memory":{"id":"after","project_id":"p1","category":"fact","content":"also kept","source":"mcp"}}` + "\n"

	var seen []RecordResult
	report, err := Import(ctx, store, strings.NewReader(body), true, func(r RecordResult) { seen = append(seen, r) })
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 1 {
		t.Errorf("rejected %d records, want just the unreadable line: %v", report.Rejected, report.Errors)
	}
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0].Error(), "line 4") {
		t.Errorf("errors = %v, want the unreadable line named as line 4", report.Errors)
	}
	if report.Created["project"] != 1 || report.Created["memory"] != 2 {
		t.Errorf("created = %v, want the project and both readable memories", report.Created)
	}
	// Both sides of the damage landed, which is the whole point.
	memories, err := store.PortableMemories(ctx, nil)
	if err != nil {
		t.Fatalf("PortableMemories: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("the store holds %d memories, want the two readable ones", len(memories))
	}
	// The unreadable line is reported to the caller as a rejected record, with
	// its line number, so the per-record report is not quietly short a line.
	var rejected *RecordResult
	for i := range seen {
		if seen[i].Action == ActionReject {
			rejected = &seen[i]
		}
	}
	if rejected == nil {
		t.Fatal("the unreadable line was not reported as a rejected record")
	}
	if rejected.Line != 4 {
		t.Errorf("the rejected record reported line %d, want 4", rejected.Line)
	}
}

// TestImportRejectsRecordsOneByOneAndKeepsGoing: one bad record in a large
// artifact must not abandon the rest. Each is reported with its line and
// reason, and the good ones still land, which is what makes a re-run of the
// same file a repair rather than a fresh start.
func TestImportRejectsRecordsOneByOneAndKeepsGoing(t *testing.T) {
	store := newTestStore(t)
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"bad","project_id":"p1","category":"not-a-category","content":"x","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"good","project_id":"p1","category":"gotcha","content":"kept","source":"mcp"}}` + "\n"
	var seen []RecordResult
	report, err := Import(context.Background(), store, strings.NewReader(body), true, func(r RecordResult) { seen = append(seen, r) })
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rejected != 1 {
		t.Errorf("rejected %d records, want 1: %v", report.Rejected, report.Errors)
	}
	if report.Created["memory"] != 1 {
		t.Errorf("created %d memories, want the good one to land", report.Created["memory"])
	}
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0].Error(), "category") {
		t.Errorf("errors = %v, want the category rejection named", report.Errors)
	}
	var rejected, created RecordResult
	for _, r := range seen {
		switch r.Action {
		case ActionReject:
			rejected = r
		case ActionCreate:
			created = r
		}
	}
	if rejected.Line != 3 {
		t.Errorf("rejected record reported line %d, want 3", rejected.Line)
	}
	if created.Line != 4 {
		t.Errorf("created record reported line %d, want 4", created.Line)
	}
	if rejected.ID != "bad" || created.ID != "good" {
		t.Errorf("per-record ids = %q / %q, want bad / good", rejected.ID, created.ID)
	}
}

// TestImportCreatesMissingProjects: a project present in the artifact but
// missing locally is created, and the records that follow it land. A record
// whose project is neither in the artifact nor in the store is rejected rather
// than written against a dangling reference.
func TestImportCreatesMissingProjects(t *testing.T) {
	store := newTestStore(t)
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"memory","memory":{"id":"m0","project_id":"unlisted","category":"gotcha","content":"x","source":"mcp"}}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one","repo_remote":"git@github.com:wcatz/one.git"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"gotcha","content":"y","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m2","project_id":"absent","category":"gotcha","content":"z","source":"mcp"}}` + "\n"
	report, err := Import(context.Background(), store, strings.NewReader(body), true, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Created["project"] != 1 {
		t.Errorf("created %d projects, want p1", report.Created["project"])
	}
	if report.Created["memory"] != 1 {
		t.Errorf("created %d memories, want the one under p1", report.Created["memory"])
	}
	if report.Rejected != 2 {
		t.Errorf("rejected %d records, want the two naming an unlisted project: %v", report.Rejected, report.Errors)
	}
	joined := ""
	for _, e := range report.Errors {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{"unlisted", "absent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors = %q, want one naming %q", joined, want)
		}
	}
	// The created project carries its repository remote, or another machine's
	// imported project could never resolve from a checkout of the same repo.
	projects, err := store.PortableProjects(context.Background())
	if err != nil {
		t.Fatalf("PortableProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p1" || projects[0].RepoRemote != "github.com/wcatz/one" {
		t.Errorf("imported projects = %+v", projects)
	}
}

// TestImportOrdersTasksAfterTheirBlocker: tasks.blocked_by is a self-reference,
// so a task whose blocker is written later in the artifact cannot be inserted
// before it. The order the importer applies tasks in has to satisfy the
// references, and a cycle must not be able to block the import.
func TestImportOrdersTasksAfterTheirBlocker(t *testing.T) {
	tasks := []struct{ id, blockedBy string }{
		{id: "t3", blockedBy: "t2"},
		{id: "t2", blockedBy: "t1"},
		{id: "t1", blockedBy: ""},
	}
	got := orderTasks(taskRecords(tasks))
	pos := map[string]int{}
	for i, rec := range got {
		pos[rec.rec.Task.ID] = i
	}
	if len(got) != 3 {
		t.Fatalf("ordered %d tasks, want 3", len(got))
	}
	for _, tk := range tasks {
		if tk.blockedBy != "" && pos[tk.blockedBy] > pos[tk.id] {
			t.Errorf("task %s is written before its blocker %s: %v", tk.id, tk.blockedBy, taskIDs(got))
		}
	}

	// A cycle has no valid order, so the pointers are dropped rather than
	// leaving the import unable to finish.
	cyclic := orderTasks(taskRecords([]struct{ id, blockedBy string }{
		{id: "a", blockedBy: "b"},
		{id: "b", blockedBy: "a"},
	}))
	if len(cyclic) != 2 {
		t.Fatalf("ordered %d tasks from a cycle, want 2", len(cyclic))
	}
	// Whatever a cycle resolves to, every task is still planned exactly once:
	// a lost or duplicated record would be a silent change to the store.
	if ids := taskIDs(cyclic); len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("cycle planning produced %v, want each task once", ids)
	}

	// A pointer to a task that is not in the artifact at all is dropped too:
	// honouring it would fail the foreign key on every attempt.
	absent := orderTasks(taskRecords([]struct{ id, blockedBy string }{
		{id: "a", blockedBy: "nowhere"},
	}))
	if absent[0].rec.Task.BlockedBy != "" {
		t.Errorf("blocked_by = %q for a task whose blocker is not in the artifact, want it dropped", absent[0].rec.Task.BlockedBy)
	}
}

func taskIDs(recs []parsedRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.rec.Task.ID)
	}
	return out
}

func taskRecords(rows []struct{ id, blockedBy string }) []parsedRecord {
	out := make([]parsedRecord, 0, len(rows))
	for i, r := range rows {
		out = append(out, parsedRecord{line: i + 2, rec: record{Type: TypeTask, Task: &memory.Task{
			ID: r.id, ProjectID: "p1", Title: r.id, Status: "pending", Priority: 2, BlockedBy: r.blockedBy,
		}}})
	}
	return out
}

// TestExportWritesNothingToAClosedWriter: a failed write must surface as an
// error rather than an artifact that looks complete on stdout.
func TestExportWritesNothingToAClosedWriter(t *testing.T) {
	store := newTestStore(t)
	seed(t, store)
	if _, err := Export(context.Background(), store, failingWriter{}, ""); err == nil {
		t.Error("Export must report a write failure")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

// TestImportReportsAReadFailure: a truncated file is an error, not a shorter
// successful import.
func TestImportReportsAReadFailure(t *testing.T) {
	store := newTestStore(t)
	_, err := Import(context.Background(), store, failingReader{}, true, nil)
	if err == nil {
		t.Error("Import must report a read failure")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestExportOrdersRecordsProjectsFirstThenByID: the order is the property that
// makes an artifact diffable, and the order an import depends on — a memory
// whose project line came after it would be rejected. Both are pinned here, on
// the bytes, because an export that merely happened to be consistent in one test
// fixture would not be an order at all.
func TestExportOrdersRecordsProjectsFirstThenByID(t *testing.T) {
	src := newTestStore(t)
	ctx := context.Background()
	// Three projects whose names sort the opposite way to their ids, each with
	// two memories whose ids sort the opposite way to their creation order, so
	// "creation order" and "id order" cannot be confused.
	for i, p := range []struct{ id, name string }{
		{"zz", "aaa"}, {"mm", "nnn"}, {"aa", "zzz"},
	} {
		if err := src.EnsureProject(ctx, p.id, "/src/"+p.id, p.name); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p.id, err)
		}
		for j := 0; j < 2; j++ {
			if _, err := src.Create(ctx, p.id, memory.Memory{
				Category: "fact", Content: p.id, Source: "mcp",
			}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			_ = i + j
		}
	}

	lines := strings.Split(strings.TrimRight(string(exportBytes(t, src, "")), "\n"), "\n")
	if len(lines) < 1+3+6 {
		t.Fatalf("artifact has %d lines, want a header, 3 projects and 6 memories", len(lines))
	}
	var kinds []string
	var projectIDs, memoryIDs []string
	for _, line := range lines {
		var rec struct {
			Type    string `json:"type"`
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
			Memory struct {
				ID string `json:"id"`
			} `json:"memory"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		kinds = append(kinds, rec.Type)
		if rec.Type == TypeProject {
			projectIDs = append(projectIDs, rec.Project.ID)
		}
		if rec.Type == TypeMemory {
			memoryIDs = append(memoryIDs, rec.Memory.ID)
		}
	}
	// Every project before every memory, so an import never meets a memory
	// before its project however the file was edited.
	lastProject, firstChild := -1, len(kinds)
	for i, k := range kinds {
		switch k {
		case TypeProject:
			lastProject = i
		case TypeHeader:
		default:
			if i < firstChild {
				firstChild = i
			}
		}
	}
	if firstChild < lastProject {
		t.Errorf("a non-project record at line %d precedes the last project at line %d: %v",
			firstChild+1, lastProject+1, kinds)
	}
	if want := []string{"aa", "mm", "zz"}; !reflect.DeepEqual(projectIDs, want) {
		t.Errorf("project order = %v, want %v (by id, not by name or creation order)", projectIDs, want)
	}
	if !sort.StringsAreSorted(memoryIDs) {
		t.Errorf("memory order = %v, want ascending id", memoryIDs)
	}
	if kinds[0] != TypeHeader {
		t.Errorf("first line is %q, want the header", kinds[0])
	}
}

// TestImportAttachesToTheProjectThisStoreAlreadyHas: the ordinary case for
// moving memories between machines. Project ids are per-install, so an artifact
// names a project the destination has never seen — while the destination very
// often has its own project for the same repository, at a different path. Both
// `projects.path` and `repo_remote` are UNIQUE, so inserting the artifact's
// project would collide; and because every child names the artifact's id, one
// collision would take the whole file with it. The records have to land in the
// project that is already there.
func TestImportAttachesToTheProjectThisStoreAlreadyHas(t *testing.T) {
	// Exported from "the laptop": one project under a remote, two memories, a task.
	artifact := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"laptop-1","path":"/Users/w/git/thing","name":"thing","repo_remote":"git@github.com:wcatz/thing.git"}}` + "\n" +
		`{"type":"memory","memory":{"id":"a","project_id":"laptop-1","category":"gotcha","content":"one","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"b","project_id":"laptop-1","category":"gotcha","content":"two","source":"mcp"}}` + "\n" +
		`{"type":"task","task":{"id":"t","project_id":"laptop-1","title":"ship","status":"pending","priority":2}}` + "\n"

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *memory.Store)
	}{
		{
			name: "same repository, different checkout path",
			setup: func(t *testing.T, s *memory.Store) {
				if err := s.EnsureProjectWithRepo(context.Background(),
					"desktop-9", "/home/w/src/thing", "thing",
					"https://github.com/wcatz/thing"); err != nil {
					t.Fatalf("EnsureProjectWithRepo: %v", err)
				}
			},
		},
		{
			name: "same checkout path, no remote recorded",
			setup: func(t *testing.T, s *memory.Store) {
				if err := s.EnsureProject(context.Background(),
					"desktop-9", "/Users/w/git/thing", "thing"); err != nil {
					t.Fatalf("EnsureProject: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dst := newTestStore(t)
			tc.setup(t, dst)

			// A dry run first: it must not promise a project the write cannot make.
			dry, err := Import(ctx, dst, strings.NewReader(artifact), false, nil)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if dry.Rejected != 0 {
				t.Fatalf("dry run rejected %d records: %v", dry.Rejected, dry.Errors)
			}
			if dry.Created["project"] != 0 {
				t.Errorf("dry run would create a project, but this store already has that checkout or repository")
			}
			if dry.Created["memory"] != 2 || dry.Created["task"] != 1 {
				t.Errorf("dry run would create %v, want the two memories and the task to land", dry.Created)
			}
			projects, err := dst.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}
			if len(projects) != 1 {
				t.Fatalf("a dry run left %d projects, want 1", len(projects))
			}

			report, err := Import(ctx, dst, strings.NewReader(artifact), true, nil)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if report.Rejected != 0 {
				t.Fatalf("apply rejected %d records: %v", report.Rejected, report.Errors)
			}
			projects, err = dst.PortableProjects(ctx)
			if err != nil {
				t.Fatalf("PortableProjects: %v", err)
			}
			// The artifact's project was not created — that would collide — and
			// the local one was not disturbed.
			if len(projects) != 1 {
				t.Fatalf("apply left %d projects: %+v", len(projects), projects)
			}
			if projects[0].ID != "desktop-9" {
				t.Errorf("project = %+v, want the one this store already had", projects[0])
			}
			// The children follow the mapping, so they land in the local project.
			memories, err := dst.PortableMemories(ctx, []string{"desktop-9"})
			if err != nil {
				t.Fatalf("PortableMemories: %v", err)
			}
			if len(memories) != 2 {
				t.Fatalf("the project holds %d memories, want the two from the artifact", len(memories))
			}
			tasks, err := dst.ListTasks(ctx, "desktop-9", "", 100)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(tasks) != 1 {
				t.Errorf("the project holds %d tasks, want the one from the artifact", len(tasks))
			}
		})
	}
}

// TestImportRejectsAProjectItCannotPlace: a project whose id is free but whose
// checkout or repository another project records anyway must arrive as a
// sentence naming the collision, in the dry run as well as the apply, never as a
// bare "UNIQUE constraint failed" from the INSERT. The mapping covers the
// ordinary case; this is the residual a concurrent writer could cause between the
// plan and the write.
func TestImportRejectsAProjectItCannotPlace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProjectWithRepo(ctx, "local", "/src/thing", "thing", "github.com/wcatz/thing"); err != nil {
		t.Fatalf("EnsureProjectWithRepo: %v", err)
	}
	_, err := store.ImportProject(ctx, memory.PortableProject{
		ID: "other", Path: "/src/elsewhere", Name: "thing", RepoRemote: "github.com/wcatz/thing",
	}, false)
	if err == nil {
		t.Fatal("a dry run promised a project whose repository another project records")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("error = %v, want it to name the project it collides with", err)
	}
	projects, lErr := store.PortableProjects(ctx)
	if lErr != nil {
		t.Fatalf("PortableProjects: %v", lErr)
	}
	if len(projects) != 1 {
		t.Errorf("a refused project still left %d rows", len(projects))
	}
}

// TestDryRunClassifiesExactlyWhatApplyWould: a dry run is only worth reading if

// it describes the run that follows. Every record must come out with the same
// action in both modes — including a memory whose project the same run has not
// created yet, which is the case a naive preview gets wrong by rejecting
// everything under a project the artifact is about to create.
func TestDryRunClassifiesExactlyWhatApplyWould(t *testing.T) {
	ctx := context.Background()
	// A small artifact with a record of each outcome: a create, a record whose
	// id is already present, and a rejection. Both destination stores are put
	// into the same state first, by importing the "already here" record into
	// each, so the two runs start from identical stores and any difference
	// between them is the dry run's doing.
	existing := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"keep","project_id":"p1","category":"fact","content":"already here","source":"manual"}}` + "\n"
	artifact := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"project","project":{"id":"p2","path":"/src/p2","name":"two"}}` + "\n" +
		`{"type":"memory","memory":{"id":"keep","project_id":"p1","category":"fact","content":"already here","source":"manual"}}` + "\n" +
		`{"type":"memory","memory":{"id":"fresh","project_id":"p2","category":"gotcha","content":"new","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"orphan","project_id":"absent","category":"gotcha","content":"x","source":"mcp"}}` + "\n" +
		`{"type":"memory","memory":{"id":"bad","project_id":"p2","category":"not-a-category","content":"x","source":"mcp"}}` + "\n"

	collect := func(t *testing.T, applyRun bool) map[string]Action {
		t.Helper()
		s := newTestStore(t)
		if _, err := Import(ctx, s, strings.NewReader(existing), true, nil); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		got := map[string]Action{}
		if _, err := Import(ctx, s, strings.NewReader(artifact), applyRun, func(r RecordResult) {
			got[fmt.Sprintf("%s/%s", r.Type, r.ID)] = r.Action
		}); err != nil && applyRun {
			t.Fatalf("Import(apply): %v", err)
		}
		return got
	}

	dryActions := collect(t, false)
	applyActions := collect(t, true)

	if len(dryActions) != len(applyActions) {
		t.Fatalf("dry run reported %d records (%v), apply reported %d (%v)",
			len(dryActions), dryActions, len(applyActions), applyActions)
	}
	for key, want := range applyActions {
		got, ok := dryActions[key]
		if !ok {
			t.Errorf("dry run did not report %s", key)
			continue
		}
		if got != want {
			t.Errorf("%s: dry run said %q, apply did %q", key, got, want)
		}
	}
	// The fixture has to exercise all three outcomes, or the comparison above
	// holds trivially.
	for key, want := range map[string]Action{
		"memory/keep":   ActionSkip,
		"memory/fresh":  ActionCreate,
		"memory/bad":    ActionReject,
		"memory/orphan": ActionReject,
	} {
		if got := applyActions[key]; got != want {
			t.Errorf("%s was %q, want %q — the fixture is not exercising %v", key, got, want, want)
		}
	}
}
