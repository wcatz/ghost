package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

type fakeAsOfStore struct {
	rows           []memory.AsOfRow
	unknown        []memory.AsOfRow
	projectRows    map[string][]memory.AsOfRow
	projectUnknown map[string][]memory.AsOfRow
	err            error
}

func (f fakeAsOfStore) MemoriesAsOfAll(context.Context, time.Time) (*memory.AsOfSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &memory.AsOfSet{Rows: f.rows, Unknown: f.unknown}, nil
}

func (f fakeAsOfStore) MemoriesAsOf(_ context.Context, projectID string, _ time.Time) (*memory.AsOfSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &memory.AsOfSet{
		Rows:    f.projectRows[projectID],
		Unknown: f.projectUnknown[projectID],
	}, nil
}

func memoryRow(content string, resolved bool) memory.AsOfRow {
	m := memory.Memory{Content: content}
	if resolved {
		stamp := "2026-01-02 10:00:00"
		m.ResolvedAt = &stamp
	}
	return memory.AsOfRow{Memory: m}
}

func writeLog(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func line(fields map[string]any) string {
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func userLine(session, ts, text string) string {
	return line(map[string]any{
		"type": "user", "sessionId": session, "timestamp": ts,
		"message": map[string]any{"content": text},
	})
}

func TestReadTurnsExtractsUserTextOnly(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "b.jsonl", userLine("s2", "2026-01-02T00:00:00Z", "we use gofumpt here"))
	writeLog(t, dir, "a.jsonl",
		userLine("s1", "2026-01-01T00:00:00Z", "never use tabs for indentation"),
		line(map[string]any{"type": "assistant", "sessionId": "s1", "timestamp": "2026-01-01T00:00:01Z", "message": map[string]any{"content": "ok"}}),
		line(map[string]any{
			"type": "user", "sessionId": "s1", "timestamp": "2026-01-01T00:00:02Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "always run gofmt"},
				map[string]any{"type": "tool_result", "content": "noise"},
			}},
		}),
		line(map[string]any{"type": "user", "sessionId": "s1", "timestamp": "2026-01-01T00:00:03Z", "isMeta": true, "message": map[string]any{"content": "meta"}}),
		userLine("s1", "2026-01-01T00:00:04Z", "<system-reminder>ignore me</system-reminder>\nwhy does the build fail"),
		line(map[string]any{"type": "summary", "summary": "a session"}),
	)
	turns, err := readTurns(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"never use tabs for indentation", "always run gofmt", "why does the build fail", "we use gofumpt here"}
	got := make([]string, 0, len(turns))
	for _, turn := range turns {
		got = append(got, turn.text)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("turns = %q, want %q", got, want)
	}
	if turns[0].session != "s1" || turns[3].session != "s2" {
		t.Fatalf("sessions not carried: %+v", turns)
	}
	if turns[0].at.IsZero() {
		t.Fatal("timestamp not parsed")
	}
}

func TestFlagRateUsesTheShippedDetector(t *testing.T) {
	texts := []string{
		"never use tabs for indentation",
		"we use gofumpt in this repo",
		"why does the build fail",
		"please fix the parser bug",
		"no, that's not how we do it",
	}
	var turns []userTurn
	for i, text := range texts {
		turns = append(turns, userTurn{session: "s", text: text, order: i, at: time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)})
	}
	got, err := analyze(turns, fakeAsOfStore{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	wantFlagged := 0
	for _, text := range texts {
		if mcpinit.IsDirectivePrompt(text) {
			wantFlagged++
		}
	}
	if got.UserTurns != len(texts) || got.Flagged != wantFlagged {
		t.Fatalf("report turns=%d flagged=%d, want %d/%d", got.UserTurns, got.Flagged, len(texts), wantFlagged)
	}
	if got.Flagged != 3 {
		t.Fatalf("flagged = %d, want 3", got.Flagged)
	}
}

func TestRepeatWithoutARecordedRuleIsCounted(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	turns := []userTurn{
		{session: "s", at: t1, text: "Never use tabs for indentation in this repo", order: 0},
		{session: "s", at: t2, text: "I told you never to use tabs for indentation", order: 1},
	}
	missing, err := analyze(turns, fakeAsOfStore{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Repeats != 1 || missing.Missing != 1 {
		t.Fatalf("empty store: repeats=%d missing=%d", missing.Repeats, missing.Missing)
	}
	if len(missing.MissingRows) != 1 || !strings.Contains(missing.MissingRows[0].Rule, "tabs") {
		t.Fatalf("missing rows = %+v", missing.MissingRows)
	}
	if !missing.MissingRows[0].At.Equal(t2) || missing.MissingRows[0].Session != "s" {
		t.Fatalf("missing row identity = %+v", missing.MissingRows[0])
	}

	present, err := analyze(turns, fakeAsOfStore{
		rows: []memory.AsOfRow{memoryRow("Never use tabs for indentation in this repo", false)},
	}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if present.Repeats != 1 || present.Missing != 0 {
		t.Fatalf("stored rule: repeats=%d missing=%d", present.Repeats, present.Missing)
	}
}

func TestRecordedRuleWinsOverUnrecordedRows(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	turns := []userTurn{
		{session: "s", at: t1, text: "Never use tabs for indentation in this repo", order: 0},
		{session: "s", at: t2, text: "I told you never to use tabs for indentation", order: 1},
	}
	const saved = "Never use tabs for indentation in this repo"
	cases := []struct {
		name        string
		store       fakeAsOfStore
		wantMissing int
		wantUnknown int
		wantPresent int
	}{
		{
			name: "rule recorded plus an unrelated unrecorded row",
			store: fakeAsOfStore{
				rows:    []memory.AsOfRow{memoryRow(saved, false)},
				unknown: []memory.AsOfRow{{Memory: memory.Memory{ID: "m-unrelated"}}},
			},
			wantPresent: 1,
		},
		{
			name: "rule not recorded with an unrecorded row",
			store: fakeAsOfStore{
				unknown: []memory.AsOfRow{{Memory: memory.Memory{ID: "m1"}}},
			},
			wantUnknown: 1,
		},
		{
			name:        "no recorded or unrecorded rows",
			store:       fakeAsOfStore{},
			wantMissing: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := analyze(turns, tc.store, "", false)
			if err != nil {
				t.Fatal(err)
			}
			present := got.Repeats - got.Missing - got.Undated - got.Unknown
			if got.Repeats != 1 || got.Missing != tc.wantMissing || got.Unknown != tc.wantUnknown || present != tc.wantPresent {
				t.Fatalf("repeats=%d missing=%d unknown=%d present=%d, want 1/%d/%d/%d",
					got.Repeats, got.Missing, got.Unknown, present,
					tc.wantMissing, tc.wantUnknown, tc.wantPresent)
			}
		})
	}
}

func TestResolvedRowStillAnswersThatTheRuleWasSaved(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)
	turns := []userTurn{
		{session: "s", at: t1, text: "Always run the migration before the tests", order: 0},
		{session: "s", at: t2, text: "We went over that yesterday: always run the migration", order: 1},
	}
	got, err := analyze(turns, fakeAsOfStore{rows: []memory.AsOfRow{
		memoryRow("Always run the migration before the tests", true),
	}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repeats != 1 || got.Missing != 0 {
		t.Fatalf("resolved row: repeats=%d missing=%d", got.Repeats, got.Missing)
	}
}

func TestReportWritesBothMeasures(t *testing.T) {
	r := report{
		UserTurns: 100, Flagged: 4, Repeats: 2, Missing: 1,
		MissingRows: []missingRow{{
			At:      time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
			Session: "sess-1",
			Repeat:  "I told you never to use tabs",
			Rule:    "never use tabs for indentation",
		}},
	}
	var b strings.Builder
	if err := r.writeTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"User turns: 100",
		"Directive-flagged turns: 4",
		"Flag rate: 4.00%",
		"Repeat corrections: 2",
		"Rule presence scope: all projects",
		"sharing two words with the rule",
		"rule absent before repeat: 1",
		"rule present before repeat: 1",
		"repeats with rule state unknown at the repeat: 0",
		"sess-1",
		"never use tabs for indentation",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestStoreErrorIsReported(t *testing.T) {
	turns := []userTurn{
		{session: "s", at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), text: "never use tabs", order: 0},
		{session: "s", at: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), text: "I told you never to use tabs", order: 1},
	}
	if _, err := analyze(turns, fakeAsOfStore{err: os.ErrPermission}, "", false); err == nil {
		t.Fatal("a store read failure was swallowed")
	}
}

// realStoreFixture writes one saved rule and one repeat of it into a temporary
// store and a temporary session-log directory. Nothing here touches a real
// data directory.
func realStoreFixture(t *testing.T) (logs, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	logs = filepath.Join(dir, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'p1')`, filepath.Join(dir, "p1")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
		VALUES (?, 'p1', 'preference', ?, 'manual', 0.8, '2026-01-01 09:00:00', '2026-01-01 09:00:00')`,
		"mrule", "Never use tabs for indentation in this repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_history (memory_id, project_id, phase, recorded_at, content, category, importance, source)
		VALUES ('mrule', 'p1', 'save', '2026-01-01 09:00:00', ?, 'preference', 0.8, 'manual')`,
		"Never use tabs for indentation in this repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeLog(t, logs, "s.jsonl",
		userLine("s", "2026-01-01T10:00:00Z", "Never use tabs for indentation in this repo"),
		userLine("s", "2026-01-02T10:00:00Z", "I told you never to use tabs for indentation"),
	)
	return logs, dbPath
}

func TestRunOnARealStore(t *testing.T) {
	logs, dbPath := realStoreFixture(t)
	var out strings.Builder
	if err := run(logs, dbPath, "", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rule absent before repeat: 0") {
		t.Fatalf("the stored rule was missed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Repeat corrections: 1") {
		t.Fatalf("the repeat was not found:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Rule presence scope: all projects") {
		t.Fatalf("the unfiltered scope was not disclosed:\n%s", out.String())
	}
}

func TestAPostRepeatEditCannotMakeTheRuleReadAsSaved(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'p1')`, filepath.Join(dir, "p1")); err != nil {
		t.Fatal(err)
	}
	const newText = "Never use tabs for indentation in this repo"
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, created_at, updated_at)
		VALUES ('m1', 'p1', 'preference', ?, 'manual', 0.8, '2026-01-01 09:00:00', '2026-01-10 09:00:00')`, newText); err != nil {
		t.Fatal(err)
	}
	// The only version is the edit after the repeat, carrying the edited text.
	// At the repeat there is no recorded text for this row, only its id.
	if _, err := db.Exec(`INSERT INTO memory_history (memory_id, project_id, phase, recorded_at, content, category, importance, source)
		VALUES ('m1', 'p1', 'update', '2026-01-10 09:00:00', ?, 'preference', 0.8, 'manual')`, newText); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeLog(t, logs, "s.jsonl",
		userLine("s", "2026-01-01T10:00:00Z", "Never use tabs for indentation in this repo"),
		userLine("s", "2026-01-02T10:00:00Z", "I told you never to use tabs for indentation"),
	)
	var out strings.Builder
	if err := run(logs, dbPath, "", false, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"rule present before repeat: 0",
		"rule absent before repeat: 0",
		"repeats with rule state unknown at the repeat: 1",
		"Repeats with the rule state unknown at the repeat:",
		"Never use tabs for indentation in this repo",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

func TestWordFilterUsesTheSharedGenericList(t *testing.T) {
	got := words("fixes tests tabs indentation quartz_widget")
	for _, want := range []string{"tabs", "indentation", "quartz_widget"} {
		if !got[want] {
			t.Errorf("words dropped %q: %v", want, got)
		}
	}
	for _, drop := range []string{"fixes", "tests"} {
		if got[drop] {
			t.Errorf("words kept generic %q: %v", drop, got)
		}
	}
}

func TestProjectFilterRejectsAnUnrelatedCrossProjectMemory(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	turns := []userTurn{
		{session: "s", at: t1, text: "Never use tabs for indentation in this repo", order: 0},
		{session: "s", at: t2, text: "I told you never to use tabs for indentation", order: 1},
	}
	// This memory is in another project and is about a different subject, but
	// shares two non-generic words with the rule: tabs and indentation.
	unrelated := "The tabs and indentation policy for widgets"
	store := fakeAsOfStore{
		rows:        []memory.AsOfRow{memoryRow(unrelated, false)},
		projectRows: map[string][]memory.AsOfRow{"p1": {}},
	}

	all, err := analyze(turns, store, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if all.Missing != 0 {
		t.Fatalf("unfiltered: missing=%d, want the two-word coincidence to read as present", all.Missing)
	}

	scoped, err := analyze(turns, store, "p1", false)
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Repeats != 1 || scoped.Missing != 1 {
		t.Fatalf("project p1: repeats=%d missing=%d, want 1/1", scoped.Repeats, scoped.Missing)
	}
	var b strings.Builder
	if err := scoped.writeTo(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Rule presence scope: project p1 (and _global)") {
		t.Fatalf("scoped report did not name the project:\n%s", b.String())
	}
}

func TestListFlagPrintsFlaggedTurns(t *testing.T) {
	logs, dbPath := realStoreFixture(t)
	writeLog(t, logs, "plain.jsonl",
		userLine("s2", "2026-01-04T10:00:00Z", "why does the build fail on ci"),
	)
	var out strings.Builder
	if err := run(logs, dbPath, "", true, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	const heading = "Flagged user turns:\n"
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("-list printed no flagged section:\n%s", text)
	}
	section := text[start+len(heading):]
	if end := strings.Index(section, "\nRepeat corrections:"); end >= 0 {
		section = section[:end]
	}
	for _, candidate := range []string{
		"Never use tabs for indentation in this repo",
		"I told you never to use tabs for indentation",
		"why does the build fail on ci",
	} {
		want := mcpinit.IsDirectivePrompt(candidate)
		if got := strings.Contains(section, candidate); got != want {
			t.Errorf("listed %q = %v, want %v:\n%s", candidate, got, want, section)
		}
	}
}

func TestParseArgsWiresFlags(t *testing.T) {
	logs, storePath, project, list, err := parseArgs(
		[]string{"-list", "-project", "p1", "logsdir", "store.db"}, &strings.Builder{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if logs != "logsdir" || storePath != "store.db" || project != "p1" || !list {
		t.Fatalf("parseArgs = %q, %q, %q, %v", logs, storePath, project, list)
	}
	if _, _, _, _, err := parseArgs([]string{"only-one"}, &strings.Builder{}); err == nil {
		t.Fatal("one operand was accepted")
	}
}

func TestIsRepeatCorrection(t *testing.T) {
	yes := []string{
		"I told you not to do that",
		"We went over that yesterday",
		"like I said, use gofumpt",
		"how many times do I have to say this",
	}
	no := []string{
		"never use tabs for indentation",
		"why does the build fail",
		"the stop sign is in the diagram",
	}
	for _, s := range yes {
		if !isRepeatCorrection(s) {
			t.Errorf("isRepeatCorrection(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isRepeatCorrection(s) {
			t.Errorf("isRepeatCorrection(%q) = true, want false", s)
		}
	}
}
