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
	for _, args := range [][]string{{"--out", "/tmp/a.db"}, {"--out=/tmp/a.db"}} {
		if opts, err := parseBackupArgs(args); err != nil || opts.Out != "/tmp/a.db" {
			t.Errorf("parseBackupArgs(%v) = %+v, %v", args, opts, err)
		}
	}
	if opts, err := parseBackupArgs(nil); err != nil || opts.Out != "" {
		t.Errorf("no flags = %+v, %v, want the default path", opts, err)
	}
	// -h never reaches a parser: handleHelp answers it before dispatch.
	for _, args := range [][]string{{"--overwrite"}, {"--out"}, {"project"}, {"-h"}} {
		if _, err := parseBackupArgs(args); err == nil {
			t.Errorf("parseBackupArgs(%v) was accepted", args)
		}
	}
}

// TestTransferCommandsAreRegisteredForHelp (#630): handleHelp answers -h/--help
// before any subcommand runs, but only for a command whose usage is in the
// table; nothing else fails when one is left out.
func TestTransferCommandsAreRegisteredForHelp(t *testing.T) {
	for _, cmd := range []string{"backup", "export", "import"} {
		usage, found := usageByCommand[cmd]
		if !found || !strings.Contains(usage, "ghost "+cmd) {
			t.Errorf("%q has no usage registered naming the command", cmd)
		}
		if !handleHelp([]string{cmd, "-h"}) {
			t.Errorf("%q -h did not reach handleHelp's table", cmd)
		}
	}
}

func TestParseExportArgs(t *testing.T) {
	opts, err := parseExportArgs([]string{"--project", "ghost", "--out=-"})
	if err != nil || opts.Project != "ghost" || opts.Out != "-" {
		t.Errorf("opts = %+v, %v, want project=ghost out=-", opts, err)
	}
	for _, args := range [][]string{{"--project"}, {"--dry-run"}} {
		if _, err := parseExportArgs(args); err == nil {
			t.Errorf("parseExportArgs(%v) was accepted", args)
		}
	}
	// A project is taken in both spellings, and a dash-leading name is a name.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--project", "ghost"}, "ghost"},
		{[]string{"--project=ghost"}, "ghost"},
		{[]string{"--project", "-myproj"}, "-myproj"},
	} {
		if opts, err := parseExportArgs(tc.args); err != nil || opts.Project != tc.want {
			t.Errorf("parseExportArgs(%v) = %+v, %v, want %q", tc.args, opts, err, tc.want)
		}
	}
}

// TestParseExportArgsRefusesAnEmptyProject: `--project` with no value is a
// refusal, never "every project" (an unset variable behind the flag must not
// export the whole store).
func TestParseExportArgsRefusesAnEmptyProject(t *testing.T) {
	for _, args := range [][]string{
		{"--project="}, {"--project", ""}, {"--out", "/tmp/x.jsonl", "--project="}, {"--out", "/tmp/x.jsonl", "--project", ""},
	} {
		opts, err := parseExportArgs(args)
		if err == nil || !strings.Contains(err.Error(), "--project") || opts.Project != "" {
			t.Errorf("parseExportArgs(%v) = %+v, %v, want a refusal naming --project and no scope", args, opts, err)
		}
	}
}

// TestParseExportArgsRefusesARepeatedProject: a second --project is a repeat
// whatever the first said (occurrences are counted, not values); two empty
// values stop at the first.
func TestParseExportArgsRefusesARepeatedProject(t *testing.T) {
	const dup, empty = "expected exactly one project", "--project requires a value"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--project", "ghost", "--project", "other"}, dup},
		{[]string{"--project=ghost", "--project=other"}, dup},
		{[]string{"--project", "ghost", "--project=other"}, dup},
		{[]string{"--project", "ghost", "--project="}, dup},
		{[]string{"--project=", "--project", "ghost"}, empty},
		{[]string{"--project=", "--project="}, empty},
	} {
		if _, err := parseExportArgs(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseExportArgs(%v) error = %v, want one containing %q", tc.args, err, tc.want)
		}
	}
}

// TestParseTransferRefusesAnEmptyOut: an explicit empty --out is the
// unset-variable spelling and is a refusal, never "no flag given" (the backup
// would land at the default path while the script looks in $DEST).
func TestParseTransferRefusesAnEmptyOut(t *testing.T) {
	parsers := map[string]func([]string) (string, error){
		"backup": func(args []string) (string, error) { opts, err := parseBackupArgs(args); return opts.Out, err },
		"export": func(args []string) (string, error) { opts, err := parseExportArgs(args); return opts.Out, err },
	}
	for name, parse := range parsers {
		for _, args := range [][]string{{"--out="}, {"--out", ""}} {
			if got, err := parse(args); err == nil || !strings.Contains(err.Error(), "--out") {
				t.Errorf("%s(%v) = %q, %v, want a refusal naming --out", name, args, got, err)
			}
		}
		// The control: a destination is used verbatim, in both spellings.
		for _, args := range [][]string{{"--out", "x.jsonl"}, {"--out=x.jsonl"}} {
			if got, err := parse(args); err != nil || got != "x.jsonl" {
				t.Errorf("%s(%v) = %q, %v, want the destination verbatim", name, args, got, err)
			}
		}
	}
}

func TestParseImportArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"--apply"}, {"--trust-provenance"}, {"a.jsonl", "b.jsonl"}} {
		if _, err := parseImportArgs(args); err == nil {
			t.Errorf("parseImportArgs(%v) was accepted: import reads exactly one artifact, and a flag is not a file", args)
		}
	}
	for _, tc := range []struct {
		args  []string
		apply bool
		trust bool
	}{
		{[]string{"x.jsonl"}, false, false}, // dry run and untrusted provenance by default
		{[]string{"--apply", "x.jsonl"}, true, false},
		{[]string{"--trust-provenance", "x.jsonl"}, false, true},
	} {
		opts, err := parseImportArgs(tc.args)
		if err != nil || opts.File != "x.jsonl" || opts.Apply != tc.apply || opts.TrustProvenance != tc.trust {
			t.Errorf("parseImportArgs(%v) = %+v, %v", tc.args, opts, err)
		}
	}
}

// TestDefaultPathsAreTimestampedBesideTheDatabase: the default backup and export
// names live in the data directory with a UTC timestamp and never collide with
// the live database; an explicit --out (or export's "-") is used verbatim.
func TestDefaultPathsAreTimestampedBesideTheDatabase(t *testing.T) {
	at := time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
	got, err := backupDefaultPath(filepath.Join("/data/ghost", "ghost.db"), "", at)
	if want := filepath.Join("/data/ghost", "ghost.db.backup-20260926T153207Z"); err != nil || got != want {
		t.Errorf("backup default path = %q, %v, want %q", got, err, want)
	}
	if explicit, err := backupDefaultPath("/data/ghost/ghost.db", "/elsewhere/snap.db", at); err != nil || explicit != "/elsewhere/snap.db" {
		t.Errorf("explicit --out = %q, %v, want it verbatim", explicit, err)
	}
	got, err = exportDefaultPath("/data/ghost", "", at)
	if want := filepath.Join("/data/ghost", "ghost-export-20260926T153207Z.jsonl"); err != nil || got != want {
		t.Errorf("export default path = %q, %v, want %q", got, err, want)
	}
	if got, err := exportDefaultPath("/data/ghost", "-", at); err != nil || got != "-" {
		t.Errorf(`--out - = %q, %v, want the stdout marker "-"`, got, err)
	}
}

// TestRunBackupCoreWritesAndReports: the report names the file and every row
// count a restore would check, and a second run into the same path fails without
// printing a report.
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
	for _, want := range []string{"backed up " + dest, "bytes", "projects:     1", "memories:     1", "memory_links: 0", "tasks:        0", "decisions:    0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	var second strings.Builder
	if err := runBackupCore(ctx, store, &second, dest); err == nil || second.Len() != 0 {
		t.Errorf("a second backup into the same path = %v with report %q, want a failure with no report", err, second.String())
	}
}

// TestRunExportCoreThenRunImportCoreRoundTrip: export writes an artifact, import
// reads it into a store that has never seen the data (dry run first, then
// applied, then a no-op re-run that says so).
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
	if err := runExportCore(ctx, src, &summary, &summary, artifact, ""); err != nil {
		t.Fatalf("runExportCore: %v", err)
	}
	for _, want := range []string{artifact, "1 project", "1 memory"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("export summary is missing %q:\n%s", want, summary.String())
		}
	}

	dst := transferTestStore(t)
	for _, step := range []struct {
		opts  portable.ImportOptions
		wants []string
		count int
	}{
		{portable.ImportOptions{}, []string{"would import", "--apply"}, 0},
		{portable.ImportOptions{Apply: true}, []string{"imported"}, 1},
		{portable.ImportOptions{Apply: true}, []string{"skipped"}, 1},
	} {
		var out strings.Builder
		if err := runImportCore(ctx, dst, artifact, step.opts, &out); err != nil {
			t.Fatalf("runImportCore(%+v): %v", step.opts, err)
		}
		for _, want := range step.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("import %+v did not say %q:\n%s", step.opts, want, out.String())
			}
		}
		if n := countMemories(t, dst); n != step.count {
			t.Errorf("import %+v left %d memories, want %d", step.opts, n, step.count)
		}
	}
}

// TestRunImportCoreRefusals: a rejected record, an unknown schema version and a
// missing file each leave an error naming what is wrong; a silent partial import
// reads as a complete one.
func TestRunImportCoreRefusals(t *testing.T) {
	rejected := `{"type":"header","schema_version":1}` + "\n" +
		`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
		`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"not-a-category","content":"x","source":"mcp"}}` + "\n"
	for _, tc := range []struct{ name, body, want string }{
		{"rejected.jsonl", rejected, "rejected"},
		{"future.jsonl", `{"type":"header","schema_version":7}` + "\n", "7"},
		{"gone.jsonl", "", "gone.jsonl"},
	} {
		path := filepath.Join(t.TempDir(), tc.name)
		if tc.body != "" {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write artifact: %v", err)
			}
		}
		var out strings.Builder
		err := runImportCore(context.Background(), transferTestStore(t), path, portable.ImportOptions{Apply: true}, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", tc.name, err, tc.want)
		}
		if tc.want == "rejected" && !strings.Contains(out.String(), "m1") {
			t.Errorf("the report does not name the rejected record:\n%s", out.String())
		}
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
	if err := runExportCore(context.Background(), store, &strings.Builder{}, &strings.Builder{}, dest, "nope"); err == nil {
		t.Fatal("an unmatched --project must fail")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a failed export left an artifact behind")
	}
}

// TestPrintImportReportNamesTheFileAndTheCounts: the summary names the file, the
// counts and skips, a singular noun for 1, and the provenance policy (it decides
// what every memory becomes, and the artifact's own value is gone once stored).
func TestPrintImportReportNamesTheFileAndTheCounts(t *testing.T) {
	report := portable.ImportReport{
		Applied: true,
		Created: map[string]int{"project": 1, "memory": 3},
		Skipped: map[string]int{"memory": 2},
	}
	var out strings.Builder
	if err := printImportReport(&out, "/tmp/x.jsonl", report, portable.ImportOptions{Apply: true}); err != nil {
		t.Fatalf("printImportReport: %v", err)
	}
	for _, want := range []string{"/tmp/x.jsonl", "1 project", "3 memories", "skipped 2",
		"provenance and retention downgraded", memory.RetentionProject, "--trust-provenance"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	trusted := portable.ImportReport{Applied: true, Created: map[string]int{"memory": 1, "task": 1}}
	if err := printImportReport(&out, "x.jsonl", trusted, portable.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
		t.Fatalf("printImportReport(trusted): %v", err)
	}
	if !strings.Contains(out.String(), "provenance and retention kept as the artifact states them") || strings.Contains(out.String(), "1 tasks") {
		t.Errorf("the trusted report misstates the policy or pluralises a single task:\n%s", out.String())
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
