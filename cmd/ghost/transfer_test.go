package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// transferTestStore opens a store over a temp-dir database. Every CLI-level
// test here goes through one of these: none of them may resolve the real data
// directory, and none may re-exec the test binary.
func transferTestStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

func TestParseBackupArgs(t *testing.T) {
	t.Run("no flags is the default path", func(t *testing.T) {
		opts, err := parseBackupArgs(nil)
		if err != nil {
			t.Fatalf("no flags must not error: %v", err)
		}
		if opts.Out != "" {
			t.Errorf("opts = %+v, want the default path", opts)
		}
	})
	t.Run("both flag spellings", func(t *testing.T) {
		for _, args := range [][]string{{"--out", "/tmp/a.db"}, {"--out=/tmp/a.db"}} {
			opts, err := parseBackupArgs(args)
			if err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if opts.Out != "/tmp/a.db" {
				t.Errorf("%v parsed as %q", args, opts.Out)
			}
		}
	})
	t.Run("unknown and valueless flags error", func(t *testing.T) {
		if _, err := parseBackupArgs([]string{"--overwrite"}); err == nil {
			t.Error("an unknown flag must error rather than being ignored")
		}
		if _, err := parseBackupArgs([]string{"--out"}); err == nil {
			t.Error("a value flag with no argument must error")
		}
		if _, err := parseBackupArgs([]string{"project"}); err == nil {
			t.Error("a stray positional must error")
		}
		// -h never reaches a parser: runCLI's handleHelp answers it before the
		// dispatch, which is what keeps a help request free of side effects. A
		// parser that also accepted it would be a second, less careful gate.
		if _, err := parseBackupArgs([]string{"-h"}); err == nil {
			t.Error("-h must not be accepted by the parser; handleHelp is the only gate")
		}
	})
}

// TestTransferCommandsAreRegisteredForHelp: the #630 contract is that handleHelp
// answers -h/--help before any subcommand runs, and it can only do that for a
// command whose usage is in the table. A dispatched-but-unregistered command
// would answer `ghost export -h` with "unknown argument" — and the contract it
// breaks is that a help request must not reach the command at all. Nothing else
// fails when one is left out, which is why the three are named here.
func TestTransferCommandsAreRegisteredForHelp(t *testing.T) {
	for _, cmd := range []string{"backup", "export", "import"} {
		usage, found := usageByCommand[cmd]
		if !found {
			t.Errorf("%q is dispatched but has no usage registered, so -h/--help would reach the command", cmd)
			continue
		}
		if !strings.Contains(usage, "ghost "+cmd) {
			t.Errorf("%q usage does not name the command: %q", cmd, strings.SplitN(usage, "\n", 2)[0])
		}
		// handleHelp must actually claim it: the registered path alone is not
		// enough if the flag scan rejects the arguments.
		if !handleHelp([]string{cmd, "-h"}) {
			t.Errorf("%q -h did not reach handleHelp's table", cmd)
		}
	}
}

func TestParseExportArgs(t *testing.T) {
	opts, err := parseExportArgs([]string{"--project", "ghost", "--out", "/tmp/x.jsonl", "--out=-"})
	if err != nil {
		t.Fatalf("parseExportArgs: %v", err)
	}
	if opts.Project != "ghost" || opts.Out != "-" {
		t.Errorf("opts = %+v, want project=ghost out=-", opts)
	}
	if _, err := parseExportArgs([]string{"--project"}); err == nil {
		t.Error("a value flag with no argument must error")
	}
	if _, err := parseExportArgs([]string{"--dry-run"}); err == nil {
		t.Error("an unknown flag must error")
	}
}

func TestParseImportArgs(t *testing.T) {
	t.Run("file required", func(t *testing.T) {
		if _, err := parseImportArgs(nil); err == nil {
			t.Error("import with no file must error")
		}
		if _, err := parseImportArgs([]string{"--apply"}); err == nil {
			t.Error("--apply is not a file: it must not be taken as one")
		}
	})
	t.Run("dry run by default", func(t *testing.T) {
		opts, err := parseImportArgs([]string{"x.jsonl"})
		if err != nil {
			t.Fatalf("parseImportArgs: %v", err)
		}
		if opts.File != "x.jsonl" || opts.Apply {
			t.Errorf("opts = %+v, want file=x.jsonl and a dry run", opts)
		}
	})
	t.Run("apply and extra positionals", func(t *testing.T) {
		opts, err := parseImportArgs([]string{"--apply", "x.jsonl"})
		if err != nil || !opts.Apply || opts.File != "x.jsonl" {
			t.Errorf("opts = %+v (err %v), want file=x.jsonl and apply", opts, err)
		}
		if _, err := parseImportArgs([]string{"a.jsonl", "b.jsonl"}); err == nil {
			t.Error("two files must error: import reads exactly one artifact")
		}
	})
}

// TestBackupDefaultPathIsTimestampedBesideTheDatabase: the default backup name
// has to be inside the data directory, carry a UTC timestamp, and never collide
// with the live database or the pre-migrate copy.
func TestBackupDefaultPathIsTimestampedBesideTheDatabase(t *testing.T) {
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	got, err := backupDefaultPath(filepath.Join("/data/ghost", "ghost.db"), "", at)
	if err != nil {
		t.Fatalf("backupDefaultPath: %v", err)
	}
	if want := filepath.Join("/data/ghost", "ghost.db.backup-20260926T153207Z"); got != want {
		t.Errorf("default path = %q, want %q", got, want)
	}
	explicit, err := backupDefaultPath(filepath.Join("/data/ghost", "ghost.db"), "/elsewhere/snap.db", at)
	if err != nil {
		t.Fatalf("backupDefaultPath(explicit): %v", err)
	}
	if explicit != "/elsewhere/snap.db" {
		t.Errorf("explicit --out = %q, want it used verbatim", explicit)
	}
	if strings.HasSuffix(got, "ghost.db") {
		t.Error("the default must never be the live database itself")
	}
}

func TestExportDefaultPathIsTimestampedBesideTheDatabase(t *testing.T) {
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	got, err := exportDefaultPath("/data/ghost", "", at)
	if err != nil {
		t.Fatalf("exportDefaultPath: %v", err)
	}
	if want := filepath.Join("/data/ghost", "ghost-export-20260926T153207Z.jsonl"); got != want {
		t.Errorf("default path = %q, want %q", got, want)
	}
	if got, err := exportDefaultPath("/data/ghost", "-", at); err != nil || got != "-" {
		t.Errorf(`--out - = %q (err %v), want the stdout marker "-"`, got, err)
	}
}

// TestRunBackupCoreWritesAndReports: the CLI's backup path is the real store
// method against a temp database, and the report has to name the file and every
// row count a restore would check.
func TestRunBackupCoreWritesAndReports(t *testing.T) {
	store := transferTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "one", Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "snap.db")
	var out strings.Builder
	if err := runBackupCore(ctx, store, &out, dest); err != nil {
		t.Fatalf("runBackupCore: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("no backup at %s: %v", dest, err)
	}
	text := out.String()
	if !strings.Contains(text, dest) {
		t.Errorf("report does not name the file it wrote:\n%s", text)
	}
	for _, want := range []string{"projects:     1", "memories:     1", "memory_links: 0", "tasks:        0", "decisions:    0"} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}

	// A second run into the same path fails rather than replacing it, and the
	// report is not printed as if it had succeeded.
	var second strings.Builder
	if err := runBackupCore(ctx, store, &second, dest); err == nil {
		t.Error("a second backup into the same path must fail")
	}
	if second.Len() != 0 {
		t.Errorf("a failed backup printed a report:\n%s", second.String())
	}
}

// TestRunExportCoreThenRunImportCoreRoundTrip: the two commands against each
// other — export writes an artifact, import reads it back into a store that has
// never seen the data, dry run first and then applied.
func TestRunExportCoreThenRunImportCoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := transferTestStore(t)
	if err := src.EnsureProject(ctx, "p1", "/src/p1", "one"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := src.Create(ctx, "p1", memory.Memory{Category: "gotcha", Content: "remembered", Source: "manual"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	artifact := filepath.Join(t.TempDir(), "export.jsonl")
	var summary strings.Builder
	if err := runExportCore(ctx, src, &summary, artifact, ""); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	if !strings.Contains(summary.String(), artifact) {
		t.Errorf("export summary does not name the artifact:\n%s", summary.String())
	}
	for _, want := range []string{"1 project", "1 memory"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("export summary is missing %q:\n%s", want, summary.String())
		}
	}

	dst := transferTestStore(t)
	// Dry run: reports what it would do and writes nothing.
	var dry strings.Builder
	if err := runImportCore(ctx, dst, artifact, false, &dry); err != nil {
		t.Fatalf("runImportCore(dry run): %v", err)
	}
	if !strings.Contains(dry.String(), "would import") {
		t.Errorf("dry run did not say it would import:\n%s", dry.String())
	}
	if !strings.Contains(dry.String(), "--apply") {
		t.Errorf("dry run did not say how to apply it:\n%s", dry.String())
	}
	if n := countMemories(t, dst); n != 0 {
		t.Errorf("dry run wrote %d memories", n)
	}

	var applied strings.Builder
	if err := runImportCore(ctx, dst, artifact, true, &applied); err != nil {
		t.Fatalf("runImportCore(apply): %v", err)
	}
	if !strings.Contains(applied.String(), "imported") {
		t.Errorf("apply did not report what it imported:\n%s", applied.String())
	}
	if n := countMemories(t, dst); n != 1 {
		t.Errorf("apply left %d memories, want 1", n)
	}

	// Re-running is a no-op that says so, which is what makes a re-run the
	// repair after a rejected record.
	var again strings.Builder
	if err := runImportCore(ctx, dst, artifact, true, &again); err != nil {
		t.Fatalf("second runImportCore: %v", err)
	}
	if !strings.Contains(again.String(), "skipped") {
		t.Errorf("a re-import did not report the skips:\n%s", again.String())
	}
	if n := countMemories(t, dst); n != 1 {
		t.Errorf("a re-import changed the memory count to %d", n)
	}
}

// TestRunImportCoreReportsARejectedRecordAndFails: a record this build cannot
// accept has to leave a non-zero exit, because a silent partial import reads as
// a complete one.
func TestRunImportCoreReportsARejectedRecordAndFails(t *testing.T) {
	body := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"not-a-category","content":"x","source":"mcp"}}` + "\n"
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	store := transferTestStore(t)
	var out strings.Builder
	err := runImportCore(context.Background(), store, path, true, &out)
	if err == nil {
		t.Fatal("an import with a rejected record must report an error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want it to say a record was rejected", err)
	}
	if !strings.Contains(out.String(), "reject") {
		t.Errorf("report is missing the rejected record:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "m1") {
		t.Errorf("report does not name the rejected record:\n%s", out.String())
	}
}

// TestRunImportCoreRefusesAnUnknownSchemaVersion: the CLI reports the refusal
// rather than importing what it cannot interpret.
func TestRunImportCoreRefusesAnUnknownSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"header","schema_version":7}`+"\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	store := transferTestStore(t)
	err := runImportCore(context.Background(), store, path, true, &strings.Builder{})
	if err == nil {
		t.Fatal("an unknown schema version must be refused")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Errorf("error = %v, want it to name the version", err)
	}
}

func TestRunImportCoreReportsAMissingFile(t *testing.T) {
	store := transferTestStore(t)
	err := runImportCore(context.Background(), store, filepath.Join(t.TempDir(), "gone.jsonl"), true, &strings.Builder{})
	if err == nil {
		t.Fatal("a missing artifact must be an error")
	}
	if !strings.Contains(err.Error(), "gone.jsonl") {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// TestRunExportCoreRejectsAnUnknownProject: an unmatched filter must not write
// an empty artifact that reads like "there is nothing here".
func TestRunExportCoreRejectsAnUnknownProject(t *testing.T) {
	store := transferTestStore(t)
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "x.jsonl")
	err := runExportCore(context.Background(), store, &strings.Builder{}, dest, "nope")
	if err == nil {
		t.Fatal("an unmatched --project must fail")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a failed export left an artifact behind")
	}
}

// TestPrintBackupReportNamesEveryTable: a report that omitted a table would
// leave a reader unable to tell an empty store from a truncated copy.
func TestPrintBackupReportNamesEveryTable(t *testing.T) {
	var out strings.Builder
	err := printBackupReport(&out, memory.BackupResult{
		Path:   "/data/ghost.db.backup-20260926T153207Z",
		Bytes:  4096,
		Counts: memory.BackupCounts{Projects: 2, Memories: 5, MemoryLinks: 1, Tasks: 1, Decisions: 1},
	})
	if err != nil {
		t.Fatalf("printBackupReport: %v", err)
	}
	text := out.String()
	if !strings.HasPrefix(text, "backed up /data/ghost.db.backup-20260926T153207Z") {
		t.Errorf("first line = %q, want the path", strings.SplitN(text, "\n", 2)[0])
	}
	if !strings.Contains(text, "4096 bytes") {
		t.Errorf("report does not state the size:\n%s", text)
	}
	for _, want := range []string{
		"projects:     2", "memories:     5", "memory_links: 1", "tasks:        1", "decisions:    1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
}

func TestPrintImportReportNamesTheFileAndTheCounts(t *testing.T) {
	report := portable.ImportReport{
		Applied: true,
		Created: map[string]int{"project": 1, "memory": 3},
		Skipped: map[string]int{"memory": 2},
	}
	var out strings.Builder
	if err := printImportReport(&out, "/tmp/x.jsonl", report, true); err != nil {
		t.Fatalf("printImportReport: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "/tmp/x.jsonl") {
		t.Errorf("report does not name the artifact:\n%s", text)
	}
	if !strings.Contains(text, "1 project") || !strings.Contains(text, "3 memories") {
		t.Errorf("report does not state what was created:\n%s", text)
	}
	if !strings.Contains(text, "skipped 2") {
		t.Errorf("report does not state the skips:\n%s", text)
	}
	// A plural-aware noun for a count of 1, so a single record is not reported
	// as "1 memories".
	report = portable.ImportReport{Applied: false, Created: map[string]int{"task": 1}}
	out.Reset()
	if err := printImportReport(&out, "x.jsonl", report, false); err != nil {
		t.Fatalf("printImportReport: %v", err)
	}
	if strings.Contains(out.String(), "1 tasks") {
		t.Errorf("report pluralised a single task:\n%s", out.String())
	}
}

func countMemories(t *testing.T, s *memory.Store) int {
	t.Helper()
	mems, err := s.GetAll(context.Background(), "p1", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	return len(mems)
}
