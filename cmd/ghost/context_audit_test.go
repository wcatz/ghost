package main

// `ghost context --audit` (#646 part 3): the report surface, at the CLI boundary.
//
// Three things only this layer can establish. The argument rules, because the
// parser is what decides whether `--since` is a window or a mistyped flag. The
// READ-ONLY property, because the report must not migrate or write a store — and
// the only way to prove that is to open the same file before and after and find it
// byte-identical in the columns that matter. And the fact that a report is reached
// through `ghost context` at all rather than as a command of its own, which is a
// decision about this CLI's surface rather than about the report.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	auditCLIMemory  = "D20E133860CC4AFE38B485AD5371BA59"
	auditCLIContent = "the scratch directory is reaped before each lifecycle run begins"
)

// TestParseContextAuditArgs is the argument contract, table-driven because the
// cases are the same shape and one of them is the reason this parser exists.
//
// The load-bearing case is the last: `--as-of` is a real flag of `ghost context`,
// and `ghost context --audit --as-of 2026-01-01T00:00:00Z` is a request for two
// different things at once. Answering it with the session-start block at that
// instant would be the more useful-looking wrong answer, and answering it with an
// audit report would silently drop the instant.
func TestParseContextAuditArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     contextAuditOptions
		wantFail string
	}{
		{
			name: "bare --audit reports the whole store",
			args: []string{"--audit"},
			want: contextAuditOptions{Audit: true},
		},
		{
			name: "project by bare flag",
			args: []string{"--audit", "--project", "ghost"},
			want: contextAuditOptions{Audit: true, Project: "ghost"},
		},
		{
			name: "project by attached form",
			args: []string{"--audit", "--project=ghost"},
			want: contextAuditOptions{Audit: true, Project: "ghost"},
		},
		{
			name: "since by bare flag",
			args: []string{"--audit", "--since", "24h"},
			want: contextAuditOptions{Audit: true, Since: 24 * time.Hour},
		},
		{
			name: "since by attached form",
			args: []string{"--audit", "--since=168h"},
			want: contextAuditOptions{Audit: true, Since: 168 * time.Hour},
		},
		{
			name: "both, in either order",
			args: []string{"--audit", "--since", "1h", "--project", "ghost"},
			want: contextAuditOptions{Audit: true, Project: "ghost", Since: time.Hour},
		},
		{
			// A project named "-h" travels through the verbatim --project form,
			// which is the same contract help.go's value-flag scan rests on.
			name: "a project named -h",
			args: []string{"--audit", "--project=-h"},
			want: contextAuditOptions{Audit: true, Project: "-h"},
		},
		{
			name:     "--project needs a value",
			args:     []string{"--audit", "--project"},
			wantFail: "needs a value",
		},
		{
			name:     "--since needs a value",
			args:     []string{"--audit", "--since"},
			wantFail: "needs a value",
		},
		{
			name:     "a value that is not a duration",
			args:     []string{"--audit", "--since", "7d"},
			wantFail: "not a duration",
		},
		{
			name:     "a negative window is refused, not clamped",
			args:     []string{"--audit", "--since", "-1h"},
			wantFail: "negative",
		},
		{
			// A zero window is a real request — "everything recorded right now" —
			// and is NOT the same as omitting the flag only if the default were a
			// window. It is not, so this is accepted and means "no window". The
			// report says which of the two it is either way.
			name: "a zero window is accepted and means no window",
			args: []string{"--audit", "--since", "0s"},
			want: contextAuditOptions{Audit: true},
		},
		{
			name:     "--project twice",
			args:     []string{"--audit", "--project", "a", "--project", "b"},
			wantFail: "twice",
		},
		{
			// An empty value is not "no value": `--project=` is what a script with
			// an unset variable produces, and the reader's scope would silently
			// become the directory's project — a confident report about a project
			// they did not name.
			name:     "an empty attached --project is refused, not read as no scope",
			args:     []string{"--audit", "--project="},
			wantFail: "empty",
		},
		{
			name:     "an empty --project value is refused",
			args:     []string{"--audit", "--project", ""},
			wantFail: "empty",
		},
		{
			name:     "an empty attached --cwd is refused, not read as this directory",
			args:     []string{"--audit", "--cwd="},
			wantFail: "empty",
		},
		{
			name:     "an empty --cwd value is refused",
			args:     []string{"--audit", "--cwd", ""},
			wantFail: "empty",
		},
		{
			name:     "a named scope followed by an empty one is still twice",
			args:     []string{"--audit", "--project", "ghost", "--project="},
			wantFail: "twice",
		},
		{
			// Two empties stop at the FIRST one, and that is the better of the two
			// answers: there is no scope in the command line at all, so the thing to
			// tell the reader is the empty value rather than a duplicate they cannot
			// see either. The duplicate guard is for the case where there IS a first
			// value — the one above.
			name:     "two empty scopes stop at the first, which is empty",
			args:     []string{"--audit", "--project=", "--project="},
			wantFail: "empty",
		},
		{
			name:     "a directory followed by an empty one is still twice",
			args:     []string{"--audit", "--cwd", "/tmp", "--cwd="},
			wantFail: "twice",
		},
		{
			name:     "an unknown flag is an error, not something ignored",
			args:     []string{"--audit", "--json"},
			wantFail: "unknown flag",
		},
		{
			name:     "a stray operand is an error",
			args:     []string{"--audit", "search"},
			wantFail: "flags only",
		},
		{
			name:     "--as-of cannot be combined with --audit",
			args:     []string{"--audit", "--as-of", "2026-01-01T00:00:00Z"},
			wantFail: "--as-of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContextAuditArgs(tc.args)
			if tc.wantFail != "" {
				if err == nil {
					t.Fatalf("parseContextAuditArgs(%v) = %+v, want an error mentioning %q", tc.args, got, tc.wantFail)
				}
				if !strings.Contains(err.Error(), tc.wantFail) {
					t.Errorf("error %q does not mention %q", err, tc.wantFail)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseContextAuditArgs(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseContextAuditArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseContextAuditArgsNeedsNoAuditFlag: the parser is about the FLAGS of the
// audit mode, not about deciding the mode. runContext decides that, and a parser
// that also decided it would mean two functions answering "is this an audit run".
func TestParseContextAuditArgsNeedsNoAuditFlag(t *testing.T) {
	got, err := parseContextAuditArgs([]string{"--since", "1h"})
	if err != nil {
		t.Fatalf("parseContextAuditArgs: %v", err)
	}
	if !got.Audit {
		t.Error("the parser did not mark the invocation as an audit run")
	}
}

// TestParseContextAuditArgsRejectsAWrongOrderValue catches the flag scan's
// counterpart: `--since` immediately followed by another flag would otherwise be
// consumed as the duration and fail with a parse error naming the FLAG rather than
// the missing value.
func TestParseContextAuditArgsRejectsAWrongOrderValue(t *testing.T) {
	_, err := parseContextAuditArgs([]string{"--audit", "--since", "--project", "ghost"})
	if err == nil {
		t.Fatal("a --since followed by another flag was accepted")
	}
	if !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("error %q does not name the problem; a reader is left thinking --project is malformed", err)
	}
}

// ghostDataDir is the directory a Ghost install keeps its database in, derived
// from the XDG data home a test sandbox sets — the same value dataDirPath resolves
// in production, and the one the dev-directory refusal is written against.
func ghostDataDir(t *testing.T, dataHome string) string {
	t.Helper()
	dir, err := config.DataDirPath()
	if err != nil {
		t.Fatalf("DataDirPath: %v", err)
	}
	if want := filepath.Join(dataHome, "ghost"); dir != want {
		t.Fatalf("DataDirPath = %q, want %q", dir, want)
	}
	return dir
}

// seedAuditCLIStore writes a store holding one project, one memory, one recorded
// search that kept it and one recorded search that kept nothing — enough for the
// report to have both a figure and a missed count.
//
// It returns the project's root directory, which is a name ("projx") the report
// could be addressed by and a PATH the report could be resolved from. The two are
// different resolutions and a test that only exercised the name would leave the
// default path — resolve the directory the reader is standing in — unproven.
func seedAuditCLIStore(t *testing.T, dataHome string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	store := memory.NewStore(db, nil)
	// A directory whose basename is NOT the project name, so a resolution test
	// reaching the path-prefix branch cannot quietly succeed on the name branch.
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(root, "cmd", "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := store.EnsureProject(ctx, "projx", root, "projx"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.CreateWithID(ctx, "projx", auditCLIMemory, memory.Memory{
		Content:  auditCLIContent,
		Category: "gotcha",
		Source:   "manual",
	}); err != nil {
		t.Fatalf("seed the memory: %v", err)
	}
	if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
		ProjectID: "projx",
		SessionID: "s1",
		Source:    "search",
		Outcome:   "answerable",
		Verdicts: []memory.RowVerdict{
			{ID: auditCLIMemory, Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
		ProjectID: "projx",
		SessionID: "s1",
		Source:    "search",
		Outcome:   "empty",
	}); err != nil {
		t.Fatalf("RecordRetrieval (empty call): %v", err)
	}
	// One judged verdict, so the report has a scored figure as well as the calls.
	s := audit.NewWithHasher(mustHasher(t))
	s.AddProse("the scratch directory is reaped before each lifecycle run begins")
	if _, err := audit.Run(ctx, store, "projx", s); err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	return root
}

// mustHasher builds the hasher the lifecycle fixture uses, from a fixed key so the
// fingerprint set is the same on every run.
func mustHasher(t *testing.T) audit.Hasher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	h, err := audit.NewHasher(key)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

// databaseFingerprint is what the read-only test compares: the schema version and
// a row count from each of the tables this report reads.
//
// user_version because a migrating open is the failure this guards — it stamps the
// version and creates tables. Row counts because a seeding open inserts rows, which
// would report a different number for the same store. Both, because either alone
// could be satisfied by a wrong implementation: stamping nothing while still
// inserting, or inserting nothing into a store the report then misread.
func databaseFingerprint(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s read-only: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	var sb strings.Builder
	version, err := memory.DBUserVersion(db)
	if err != nil {
		t.Fatalf("DBUserVersion: %v", err)
	}
	fmt.Fprintf(&sb, "user_version=%d\n", version)
	for _, table := range []string{"memories", "retrieval_record", "retrieval_audit", "projects"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		fmt.Fprintf(&sb, "%s=%d\n", table, n)
	}
	return sb.String()
}

// TestBuildContextAuditReportDoesNotWrite: the load-bearing property of this
// surface.
//
// `ghost context --audit` is a report, and a report that migrates a store whose
// schema is behind, seeds builtin rows into it, or bumps its version has changed
// the thing it reported on. The evidence is a fingerprint of the file taken before
// and after the run through the SAME seam the command uses.
func TestBuildContextAuditReportDoesNotWrite(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditCLIStore(t, dataHome)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")

	before := databaseFingerprint(t, dbPath)

	rep, err := buildContextAudit(context.Background(), contextAuditOptions{Project: "projx"}, ghostDataDir(t, dataHome))
	if err != nil {
		t.Fatalf("buildContextAudit: %v", err)
	}
	// The report actually got built, so the fingerprint comparison below is
	// between two populated states rather than between a failure and a fixture.
	if rep.ProjectID != "projx" {
		t.Fatalf("report scope = %q, want the resolved project id %q", rep.ProjectID, "projx")
	}
	if src := rep.Source("search"); src == nil || src.Kept != 1 {
		t.Fatalf("the report has no search figure over the seeded call: %+v", src)
	}

	after := databaseFingerprint(t, dbPath)
	if after != before {
		t.Errorf("the report changed the store it read:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestBuildContextAuditReportRefusesAnUnmigratedStore: the visible half of
// read-only, and the reason this path does not reach for bootstrap.
//
// A store stamped behind would be migrated by a read-write open, so the report
// would work — having silently rewritten the user's database to produce it. The
// read-only open refuses it and names the remedy instead, which is the same answer
// `ghost export` gives. A report that migrated the store would be indistinguishable
// from one that did not, right up until the user needed their database back.
func TestBuildContextAuditReportRefusesAnUnmigratedStore(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditCLIStore(t, dataHome)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")

	// Stamp it behind, from outside the store.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open to stamp: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("stamp behind: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = buildContextAudit(context.Background(), contextAuditOptions{Project: "projx"}, ghostDataDir(t, dataHome))
	if err == nil {
		t.Fatal("the report read a store stamped behind its schema; a read-write open would have migrated it")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
}

// TestBuildContextAuditReportNamesAnUnknownProject: a --project that resolves to
// nothing is an ERROR, never a store-wide report.
//
// The two readings of "no such project" are a bug and a measurement of the whole
// corpus, and the second one is the dangerous one: an operator who mistyped a
// project name would get every project's figures pooled and no indication of it.
func TestBuildContextAuditReportNamesAnUnknownProject(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditCLIStore(t, dataHome)

	_, err := buildContextAudit(context.Background(), contextAuditOptions{Project: "no-such-project"}, ghostDataDir(t, dataHome))
	if err == nil {
		t.Fatal("an unknown project produced a report rather than an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the error does not say the project was not found: %v", err)
	}
}

// TestBuildContextAuditReportDefaultsToTheProjectsDirectory: with no --project the
// scope is the project the DIRECTORY resolves to — the scope `ghost context`
// already works in, and the one the reader is standing in.
//
// The fixture's project directory is a temporary checkout whose basename is not the
// project name, so this reaches the path-prefix resolution rather than satisfying
// itself on the name branch the --project test already covers. It points --cwd at a
// SUBDIRECTORY rather than the root, because that is the shape a reader's shell is
// actually in, and because a store that only matched its own root would resolve for
// the wrong reason.
func TestBuildContextAuditReportDefaultsToTheProjectsDirectory(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	root := seedAuditCLIStore(t, dataHome)

	rep, err := buildContextAudit(context.Background(),
		contextAuditOptions{Cwd: filepath.Join(root, "cmd", "ghost")}, ghostDataDir(t, dataHome))
	if err != nil {
		t.Fatalf("buildContextAudit: %v", err)
	}
	if rep.ProjectID != "projx" {
		t.Errorf("report scope = %q, want the project the directory resolved to (%q)", rep.ProjectID, "projx")
	}
	if src := rep.Source("search"); src == nil || src.Calls != 2 {
		t.Errorf("the resolved report is not the fixture's figures: %+v", src)
	}
}

// TestBuildContextAuditReportRefusesAnUnresolvableDirectory: the other half of
// always-one-project, and the reading that would be worse than an error.
//
// A report over every project is the one figure set this command can produce
// without the reader naming a scope, and it is always an answer to a different
// question: it would pool sources from projects the reader never asked about, which
// is precisely the pooling Report.Source and the per-source lines exist to prevent.
// So a directory Ghost does not know is refused, and the refusal names the flag that
// would have worked — otherwise the reader is left to guess at a fix.
func TestBuildContextAuditReportRefusesAnUnresolvableDirectory(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditCLIStore(t, dataHome)

	stranger := filepath.Join(t.TempDir(), "not-a-project")
	if err := os.MkdirAll(stranger, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rep, err := buildContextAudit(context.Background(),
		contextAuditOptions{Cwd: stranger}, ghostDataDir(t, dataHome))
	if err == nil {
		t.Fatalf("a directory outside every project produced a report (scope %q) rather than an error", rep.ProjectID)
	}
	for _, want := range []string{"not found", "--project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestPrintContextAuditStatesItsLimits: the prose reaches the terminal. Asserted
// here as well as on the type because this is the surface an operator pastes into
// an issue, and a report whose caveats live only in a godoc comment has none.
func TestPrintContextAuditStatesItsLimits(t *testing.T) {
	var sb strings.Builder
	if err := printContextAudit(&sb, audit.Report{
		ProjectID: "projx",
		Sources: []audit.SourceReport{
			{Source: "search", Calls: 2, Kept: 3, Scored: 3, Used: 1, Ignored: 2, KeptNothing: 1},
			{Source: "session_start"},
			{Source: "project_context"},
		},
	}); err != nil {
		t.Fatalf("printContextAudit: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"projx",
		"never pooled",
		"not a relevance or usefulness score",
		"not measured",
		"session_start: no rows",
		"project_context: no rows",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the printed report does not contain %q:\n%s", want, out)
		}
	}
}

// TestPrintContextAuditCarriesTheWindow: a report saved into an issue has to say
// what it measured, or its figures read as the store's standing state.
func TestPrintContextAuditCarriesTheWindow(t *testing.T) {
	var sb strings.Builder
	if err := printContextAudit(&sb, audit.Report{
		ProjectID: "projx",
		Since:     24 * time.Hour,
		Sources:   []audit.SourceReport{{Source: "search", Calls: 1, Kept: 1, Scored: 1, Used: 1}},
	}); err != nil {
		t.Fatalf("printContextAudit: %v", err)
	}
	if !strings.Contains(sb.String(), "24h") {
		t.Errorf("the printed report does not echo its window:\n%s", sb.String())
	}
}

// TestContextUsageDocumentsTheAuditFlags: the help is a promise about what a
// command accepts, and this one grew a mode and two flags.
func TestContextUsageDocumentsTheAuditFlags(t *testing.T) {
	for _, want := range []string{"--audit", "--project", "--since"} {
		if !strings.Contains(contextUsage, want) {
			t.Errorf("ghost context's help does not document %s", want)
		}
	}
}

// TestContextAuditIsRegisteredAsAValueFlagCommand: the registration and the help
// scan it feeds, asserted at this command because this is the one that would
// misbehave without it.
//
// The direction is the one the CLI-wide contract chose, and it is worth being
// explicit about because the intuitive reading is backwards: `--project -h` is a
// request to report on a project NAMED "-h", not a help request. A project name is
// caller-supplied data, and treating a data token as the help flag is how
// `ghost reflect --project -h` came to print usage for a command whose parser
// accepts the name. So the scan skips the token after a registered value flag, and
// the parser — which reports no such project — is what answers.
//
// The counterpart is that a bare `-h` still reaches usage, because `--audit` takes
// no value: nothing is being consumed, so the token is a question again. If that
// stopped working, `ghost context --audit -h` would run a report instead of
// documenting one.
func TestContextAuditIsRegisteredAsAValueFlagCommand(t *testing.T) {
	valueFlags, ok := helpValueFlagsByCommand["context"]
	if !ok {
		t.Fatal(`"context" is not registered in helpValueFlagsByCommand`)
	}
	for _, flag := range []string{"--project", "--since"} {
		if !valueFlags[flag] {
			t.Errorf("%s is not registered as a value flag of `ghost context`; `--audit %s -h` would print usage instead of reporting on the project named -h", flag, flag)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"a project named -h is a value, not help", []string{"--audit", "--project", "-h"}, false},
		{"a duration-looking value does not reopen the scan", []string{"--audit", "--since", "24h", "--project", "-h"}, false},
		{"a directory named -h is a value too", []string{"--audit", "--cwd", "-h"}, false},
		{"a bare -h after a valueless flag is still help", []string{"--audit", "-h"}, true},
		{"help after a real value is still help", []string{"--audit", "--since", "24h", "--help"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsHelp("context", tc.args); got != tc.want {
				t.Errorf("wantsHelp(%q, %v) = %v, want %v", "context", tc.args, got, tc.want)
			}
		})
	}
}

// TestRunContextDispatchesToTheAudit: the routing itself. Without --audit, the
// command prints the session-start block — the thing opencode's plugin depends on
// — and this asserts the audit path does not swallow the ordinary one.
func TestContextAuditRequestedOnlyWithTheFlag(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{}, false},
		{[]string{"--as-of", "2026-01-01T00:00:00Z"}, false},
		{[]string{"--cwd", "/tmp"}, false},
		{[]string{"--project", "ghost"}, false},
		{[]string{"--audit"}, true},
		{[]string{"--audit", "--since", "24h"}, true},
		{[]string{"--since", "24h"}, false},
	}
	for _, tc := range cases {
		if got := contextAuditRequested(tc.args); got != tc.want {
			t.Errorf("contextAuditRequested(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
