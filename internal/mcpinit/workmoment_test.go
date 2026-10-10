package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// The working-moment tests drive RunHostEvent, the real entry point, over a real
// on-disk store under sandboxed XDG and HOME directories, and read what a host
// would read: stdout.

type momentFixture struct {
	projDir, dataDir, dbPath string
}

const (
	momentGotcha = "Gotcha: the quartzmigrate step deadlocks when sqlitebackup runs at the same time; always stop sqlitebackup before it."
	momentFile   = "Editing stophook.go needs the lifecycle cooldown stamp cleared first, otherwise the spawn is silently skipped."
	// momentFiller shares ordinary words with the unrelated message used below
	// ("cloud", "build"), so the keyword leg RETRIEVES it and only the floor
	// keeps it out.
	momentFiller = "The nightly build runs in the cloud and the sync step follows every build."
)

func newMomentFixture(t *testing.T) momentFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	setHome(t, filepath.Join(root, "home"))
	t.Setenv(config.DevForbidDataDirEnv, "")
	dataDir := mkdirAll(t, filepath.Join(root, "data", "ghost"))
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	projDir := mkdirAll(t, filepath.Join(root, "proj"))
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'momentproj')`, canonical); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, project, category, content string }{
		{"mgotcha", "p1", "gotcha", momentGotcha},
		{"mfile", "p1", "gotcha", momentFile},
		{"mfiller", "p1", "fact", momentFiller},
		{"mhooks", "p1", "gotcha", "Helm hooks run before the migrate job, and hook weights decide the order."},
		{"mglobal", memory.GlobalProjectID, "preference", "Global preference: quartzmigrate deadlock reports are always verbose."},
	} {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance)
			VALUES (?, ?, ?, ?, 'manual', 0.8)`, r.id, r.project, r.category, r.content); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return momentFixture{projDir: canonical, dataDir: dataDir, dbPath: dbPath}
}

func (f momentFixture) submit(session, prompt string) string {
	p, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": session, "cwd": f.projDir, "prompt": prompt})
	return fireHook("message-submit", "claude-code", string(p))
}

func (f momentFixture) edit(session, tool, file, newString string) string {
	p, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": session, "cwd": f.projDir,
		"tool_name": tool, "tool_input": map[string]any{"file_path": file, "new_string": newString}})
	return fireHook("edit", "claude-code", string(p))
}

// fireHook runs the real entry point and returns stdout; stderr must be empty on
// every path these tests take, because a hook that prints on a normal turn is
// noise in the host's log.
func fireHook(event, source, payload string) string {
	var out, errb bytes.Buffer
	RunHostEvent(event, source, strings.NewReader(payload), &out, &errb)
	if errb.Len() > 0 && !strings.Contains(errb.String(), "fail-open") {
		return out.String() + "\nSTDERR:" + errb.String()
	}
	return out.String()
}

// context decodes a delivery and returns its additionalContext and event name.
func momentContext(t *testing.T, out string) (text, event string) {
	t.Helper()
	var v struct {
		H struct {
			Event string `json:"hookEventName"`
			Ctx   string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("stdout is not the host's output shape: %v\n%s", err, out)
	}
	return v.H.Ctx, v.H.Event
}

func TestAMessageNamingASavedGotchaDeliversItOnce(t *testing.T) {
	f := newMomentFixture(t)
	out := f.submit("sess-a", "why does the quartzmigrate step deadlock on ci")
	text, event := momentContext(t, out)
	if event != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", event)
	}
	if !strings.Contains(text, "sqlitebackup") || !strings.Contains(text, "`mgotcha`") {
		t.Fatalf("the saved gotcha was not delivered:\n%s", text)
	}
	if strings.Contains(text, "mglobal") {
		t.Errorf("a `_global` row was delivered; the channel is the project's own memories:\n%s", text)
	}
	if len(text) > workMomentBlockBytes {
		t.Errorf("block is %d bytes, cap is %d", len(text), workMomentBlockBytes)
	}
	if again := f.submit("sess-a", "remind me about quartzmigrate and sqlitebackup"); again != "" {
		t.Fatalf("a second matching message repeated a delivered row:\n%s", again)
	}
	// Another session has its own marker and is shown it again.
	if other := f.submit("sess-b", "why does the quartzmigrate step deadlock on ci"); other == "" {
		t.Error("a different session was denied the row; the marker is per session")
	}
}

func TestARowFromTheSessionStartBlockIsNotRepeated(t *testing.T) {
	f := newMomentFixture(t)
	start, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "sess-s", "cwd": f.projDir, "source": "startup"})
	block := fireHook("session-start", "claude-code", string(start))
	if !strings.Contains(block, "`mgotcha`") {
		t.Fatalf("the fixture's session start did not render the gotcha, so this test would prove nothing:\n%s", block)
	}
	if out := f.submit("sess-s", "why does the quartzmigrate step deadlock on ci"); out != "" {
		t.Fatalf("a row the session-start block already delivered was repeated:\n%s", out)
	}
}

func TestAnUnrelatedMessageEmitsNothing(t *testing.T) {
	f := newMomentFixture(t)
	// Shares one ordinary word ("cloud") with a stored row: retrieved by the
	// keyword leg, kept out by the floor.
	msg := "what is the weather in the cloud today"
	terms := memory.DistinctiveTerms(msg, workMomentTerms)
	if s := keywordScore(assemble.Item{Content: momentFiller}, terms); s == 0 {
		t.Fatalf("the filler row shares no term with the message (score 0), so the floor is not what silences it")
	}
	if out := f.submit("sess-u", msg); out != "" {
		t.Fatalf("an unrelated message emitted:\n%s", out)
	}
	if calls := recordedCalls(t, f.dbPath); len(calls) != 0 {
		t.Errorf("a silent turn wrote %d retrieval record(s); only a delivery is recorded", len(calls))
	}
}

func TestAnEditOfAFileNamedInAMemoryDeliversIt(t *testing.T) {
	f := newMomentFixture(t)
	out := f.edit("sess-e", "Edit", filepath.Join(f.projDir, "internal", "mcpinit", "stophook.go"), "x := 1")
	text, event := momentContext(t, out)
	if event != "PostToolUse" {
		t.Errorf("hookEventName = %q, want PostToolUse", event)
	}
	if !strings.Contains(text, "`mfile`") {
		t.Fatalf("the memory naming the file was not delivered:\n%s", text)
	}
	if again := f.edit("sess-e", "Write", filepath.Join(f.projDir, "stophook.go"), ""); again != "" {
		t.Errorf("a second edit of the same file repeated the row:\n%s", again)
	}
	if out := f.edit("sess-e2", "Bash", "stophook.go", ""); out != "" {
		t.Errorf("a non-edit tool delivered:\n%s", out)
	}
	// hook.go shares its stem with a memory about hooks, which is not a memory
	// about hook.go: the stem is an ordinary word and one word is below the floor.
	if out := f.edit("sess-e4", "Edit", filepath.Join(f.projDir, "hook.go"), "x := 1"); out != "" {
		t.Errorf("an edit of hook.go delivered a memory that only shares the word hook:\n%s", out)
	}
	if out := f.edit("sess-e3", "Edit", filepath.Join(f.projDir, "unrelated.txt"), "plain words only"); out != "" {
		t.Errorf("an edit of an unmentioned file delivered:\n%s", out)
	}
}

func TestTheDeliveryIsRecordedWithItsOwnSource(t *testing.T) {
	f := newMomentFixture(t)
	if out := f.submit("sess-r", "why does the quartzmigrate step deadlock on ci"); out == "" {
		t.Fatal("no delivery")
	}
	calls := recordedCalls(t, f.dbPath)
	if len(calls) != 1 {
		t.Fatalf("%d records, want 1", len(calls))
	}
	c := calls[0]
	if c.source != string(assemble.SourceWorkingMoment) || c.projectID != "p1" || c.sessionID != "sess-r" {
		t.Errorf("record = %+v, want source working_moment, project p1, session sess-r", c)
	}
	if c.queryHash == "" || strings.Contains(c.verdicts, "quartzmigrate") {
		t.Errorf("the record must carry a digest and no text: hash=%q verdicts=%s", c.queryHash, c.verdicts)
	}
	var vs []memory.RowVerdict
	if err := json.Unmarshal([]byte(c.verdicts), &vs); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, v := range vs {
		if v.Kept {
			kept[v.ID] = true
		}
	}
	if !kept["mgotcha"] || len(kept) != 1 {
		t.Errorf("kept = %v, want exactly the delivered row", kept)
	}
}

func TestTheSharedBudgetIsRespected(t *testing.T) {
	f := newMomentFixture(t)
	msg := "why does the quartzmigrate step deadlock on ci"

	// Session start spent nearly everything: nothing fits, nothing is sent.
	recordSessionStartDelivery("sess-b1", nil, workMomentBudget-100)
	if out := f.submit("sess-b1", msg); out != "" {
		t.Errorf("delivered with 100 bytes left:\n%s", out)
	}

	// Two rows match; 300 bytes left holds one of them and not both.
	both := "quartzmigrate sqlitebackup stophook.go cooldown"
	full, _ := momentContext(t, f.submit("sess-b0", both))
	if !strings.Contains(full, "`mgotcha`") || !strings.Contains(full, "`mfile`") || len(full) <= 300 {
		t.Fatalf("control: with room, both rows must be delivered in more than 300 bytes (%d):\n%s", len(full), full)
	}
	recordSessionStartDelivery("sess-b2", nil, workMomentBudget-300)
	text, _ := momentContext(t, f.submit("sess-b2", both))
	if len(text) == 0 || len(text) > 300 || strings.Count(text, "\n- [") != 1 {
		t.Errorf("block is %d bytes with 300 left, want exactly one row:\n%s", len(text), text)
	}

	// No start entry at all: the start is assumed to have spent its whole cap.
	text, _ = momentContext(t, f.submit("sess-b3", msg))
	if len(text) > workMomentBudget-sessionStartByteCap {
		t.Errorf("block is %d bytes, but an unknown session start leaves only %d", len(text), workMomentBudget-sessionStartByteCap)
	}

	// Deliveries add to the same ledger as the start block.
	st, err := readMomentState(momentMarkerPath("sess-b3"))
	if err != nil {
		t.Fatal(err)
	}
	if st.spent != sessionStartByteCap+len(text) {
		t.Errorf("spent = %d, want the assumed start cap %d plus the delivery %d", st.spent, sessionStartByteCap, len(text))
	}
}

func TestParallelHooksDeliverARowExactlyOnce(t *testing.T) {
	f := newMomentFixture(t)
	const n = 8
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = f.submit("sess-p", "why does the quartzmigrate step deadlock on ci")
		}(i)
	}
	wg.Wait()
	got := 0
	for _, o := range outs {
		if strings.Contains(o, "mgotcha") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d parallel hooks delivered the row, want exactly 1", got)
	}
}

func TestWorkingMomentErrorPathsEmitNothing(t *testing.T) {
	f := newMomentFixture(t)
	good := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "sess-x", "cwd": f.projDir,
		"prompt": "why does the quartzmigrate step deadlock on ci"}
	n := 0
	with := func(k string, v any) string {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		// A session of its own per case, so the control's marker is never why a
		// case is silent.
		n++
		m["session_id"] = fmt.Sprintf("sess-case-%d", n)
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	if out := fireHook("message-submit", "claude-code", string(mustJSON(good))); out == "" {
		t.Fatal("control: the unmodified payload must deliver, or the cases below prove nothing")
	}
	cases := map[string]struct{ event, source, payload string }{
		"malformed json":   {"message-submit", "claude-code", "{not json"},
		"empty stdin":      {"message-submit", "claude-code", ""},
		"no session id":    {"message-submit", "claude-code", with("session_id", nil)},
		"subagent":         {"message-submit", "claude-code", with("agent_id", "agent-1")},
		"unknown project":  {"message-submit", "claude-code", with("cwd", t.TempDir())},
		"no prompt":        {"message-submit", "claude-code", with("prompt", "")},
		"only stopwords":   {"message-submit", "claude-code", with("prompt", "what is this and that")},
		"event mismatch":   {"message-submit", "claude-code", with("hook_event_name", "Stop")},
		"codex":            {"message-submit", "codex", with("cwd", f.projDir)},
		"opencode":         {"message-submit", "opencode", with("cwd", f.projDir)},
		"goose":            {"message-submit", "goose", with("cwd", f.projDir)},
		"edit with prompt": {"edit", "claude-code", with("hook_event_name", "PostToolUse")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// A fresh session id so the control's marker cannot be the reason.
			if out := fireHook(c.event, c.source, c.payload); out != "" {
				t.Errorf("emitted:\n%s", out)
			}
		})
	}
	t.Run("no store", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		if out := fireHook("message-submit", "claude-code", with("cwd", f.projDir)); out != "" {
			t.Errorf("emitted:\n%s", out)
		}
	})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestARefusedDataDirEmitsNothingAndWritesNothing(t *testing.T) {
	withBuildVersion(t, devBuild)
	f := newMomentFixture(t)
	t.Setenv(config.DevForbidDataDirEnv, realPath(t, f.dataDir))
	before := dirEntries(t, f.dataDir)
	if out := f.submit("sess-f", "why does the quartzmigrate step deadlock on ci"); out != "" {
		t.Fatalf("emitted from a forbidden data dir:\n%s", out)
	}
	recordSessionStartDelivery("sess-f", []string{"mgotcha"}, 100)
	if after := dirEntries(t, f.dataDir); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("a refused data dir was written to: %v -> %v", before, after)
	}
	if p := momentMarkerPath("sess-f"); p != "" {
		t.Errorf("a refused resolve produced a marker path %q; it must mean do nothing", p)
	}
}

func TestMarkerValidityIsDecidedByFileOrder(t *testing.T) {
	f := newMomentFixture(t)
	path := filepath.Join(f.dataDir, "m.log")
	for _, e := range []momentEntry{
		{token: startToken, bytes: 1000, ids: []string{"a"}},
		{token: "t1", bytes: 500, ids: []string{"b"}},
		{token: "t2", bytes: 500, ids: []string{"b", "c"}},         // clashes with t1
		{token: "t3", bytes: workMomentBudget, ids: []string{"d"}}, // over budget
		{token: "t4", bytes: 100, ids: []string{"e"}},
	} {
		if err := appendMomentEntry(path, e); err != nil {
			t.Fatal(err)
		}
	}
	st, err := readMomentState(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{startToken: true, "t1": true, "t2": false, "t3": false, "t4": true}
	for tok, w := range want {
		if st.valid[tok] != w {
			t.Errorf("valid[%s] = %v, want %v", tok, st.valid[tok], w)
		}
	}
	if st.spent != 1600 || st.delivered["c"] || !st.delivered["e"] {
		t.Errorf("spent=%d delivered=%v: invalid entries must neither spend nor claim", st.spent, st.delivered)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("marker mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSessionIdIsSanitisedIntoTheMarkerName(t *testing.T) {
	newMomentFixture(t)
	for id, want := range map[string]string{
		"abc-123":    "working-moment-abc-123.log",
		"../../evil": "working-moment-.._.._evil.log",
		"a/b\\c:d e": "working-moment-a_b_c_d_e.log",
	} {
		p := momentMarkerPath(id)
		if filepath.Base(p) != want || filepath.Base(filepath.Dir(p)) != "ghost" {
			t.Errorf("marker for %q = %q, want a file %q directly in the data dir", id, p, want)
		}
	}
	if p := momentMarkerPath("  "); p != "" {
		t.Errorf("blank session id produced %q", p)
	}
}

func TestSessionStartSweepsOnlyOldMarkers(t *testing.T) {
	f := newMomentFixture(t)
	old := filepath.Join(f.dataDir, "working-moment-old.log")
	fresh := filepath.Join(f.dataDir, "working-moment-fresh.log")
	other := filepath.Join(f.dataDir, "lifecycle-p1.last")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("D start 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aged := time.Now().Add(-workMomentMarkerMaxAge - time.Hour)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	recordSessionStartDelivery("sess-sweep", []string{"x"}, 10)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an old marker survived the sweep")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was swept but must not be", filepath.Base(p))
		}
	}
}

func TestEnsureWorkingMomentHooksIsIdempotentAndNonDestructive(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, ".claude", "settings.json")
	mkdirAll(t, filepath.Dir(path))
	existing := `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/bin/other-tool"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/usr/bin/mine"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := ensureWorkingMomentHooks(&out, sf, "/opt/ghost"); err != nil {
			t.Fatal(err)
		}
	}
	if err := sf.save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var doc struct {
		Hooks map[string][]hookEntry `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	count := func(event, sub string) (n int, matcher string) {
		for _, e := range doc.Hooks[event] {
			for _, h := range e.Hooks {
				if strings.Contains(h.Command, sub) {
					n++
					matcher = e.Matcher
				}
			}
		}
		return
	}
	if n, _ := count("UserPromptSubmit", "hook message-submit --source claude-code"); n != 1 {
		t.Errorf("message-submit hook registered %d times after 3 runs, want 1", n)
	}
	if n, m := count("PostToolUse", "hook edit --source claude-code"); n != 1 || m != "Edit|Write|MultiEdit" {
		t.Errorf("edit hook registered %d times with matcher %q, want once with Edit|Write|MultiEdit", n, m)
	}
	if n, _ := count("PostToolUse", "/usr/bin/other-tool"); n != 1 {
		t.Error("an existing hook for the same event was removed")
	}
	if n, _ := count("UserPromptSubmit", "/usr/bin/mine"); n != 1 {
		t.Error("an existing message hook was removed")
	}
	if got := strings.Count(out.String(), "added"); got != 2 {
		t.Errorf("reported %d additions across 3 runs, want 2 (first run only):\n%s", got, out.String())
	}
	if !contractHookWired(sf, "UserPromptSubmit", "message-submit", "claude-code") || !contractHookWired(sf, "PostToolUse", "edit", "claude-code") {
		t.Error("status would report the hooks as unwired")
	}
}

func TestWorkingMomentCapabilityIsClaudeCodeOnly(t *testing.T) {
	for _, s := range []hostevent.Source{hostevent.SourceClaudeCode, hostevent.SourceCodex, hostevent.SourceGoose, hostevent.SourceOpencode} {
		c, ok := hostevent.CapabilityFor(s)
		if !ok || c.WorkingMoment != (s == hostevent.SourceClaudeCode) {
			t.Errorf("%s WorkingMoment = %v", s, c.WorkingMoment)
		}
	}
}

// With the relevance cutoff off, the keyword leg returns the `_global` row too
// (it matches two of the message's words), so only the channel's own project
// filter keeps it out.
func TestAGlobalRowIsNeverDelivered(t *testing.T) {
	f := newMomentFixture(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "0")
	text, _ := momentContext(t, f.submit("sess-g", "why does the quartzmigrate step deadlock on ci"))
	if !strings.Contains(text, "`mgotcha`") {
		t.Fatalf("control: the project row must be delivered:\n%s", text)
	}
	if strings.Contains(text, "mglobal") {
		t.Errorf("a `_global` row was delivered:\n%s", text)
	}
	calls := recordedCalls(t, f.dbPath)
	if len(calls) != 1 || !strings.Contains(calls[0].verdicts, reasonWorkingMomentGlobal) {
		t.Errorf("the withheld global row must be recorded as a dropped verdict (%s): %+v", reasonWorkingMomentGlobal, calls)
	}
}

// A message that matches one row already delivered and one new row delivers the
// new one: the delivered row is skipped at selection, so it neither blocks the
// delivery nor is repeated.
func TestADeliveredRowDoesNotHideANewMatch(t *testing.T) {
	f := newMomentFixture(t)
	if out := f.submit("sess-n", "why does the quartzmigrate step deadlock on ci"); out == "" {
		t.Fatal("no first delivery")
	}
	text, _ := momentContext(t, f.submit("sess-n", "quartzmigrate sqlitebackup stophook.go cooldown"))
	if !strings.Contains(text, "`mfile`") || strings.Contains(text, "`mgotcha`") {
		t.Errorf("want only the new row (mfile):\n%s", text)
	}
}

func TestAMissingMarkerAssumesTheSessionStartSpentItsCap(t *testing.T) {
	f := newMomentFixture(t)
	st, err := readMomentState(filepath.Join(f.dataDir, "working-moment-none.log"))
	if err != nil || st.spent != sessionStartByteCap {
		t.Errorf("missing marker: spent=%d err=%v, want the session-start cap %d", st.spent, err, sessionStartByteCap)
	}
	// And a marker with deliveries but no start entry counts the cap once, not never.
	path := filepath.Join(f.dataDir, "working-moment-nostart.log")
	if err := appendMomentEntry(path, momentEntry{token: "t1", bytes: 200, ids: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := readMomentState(path); st.spent != sessionStartByteCap+200 {
		t.Errorf("spent = %d, want %d", st.spent, sessionStartByteCap+200)
	}
}

func TestContainsBounded(t *testing.T) {
	for _, c := range []struct {
		text, term string
		want       bool
	}{
		{"edit hook.go now", "hook.go", true},
		{"hook.gone", "hook.go", false},
		{"see v10", "v1", false},
		{"see v1.", "v1", true},
		{"xhook.go", "hook.go", false},
		{"hook.gox hook.go", "hook.go", true},
		{"", "a", false},
	} {
		if got := containsBounded(c.text, c.term); got != c.want {
			t.Errorf("containsBounded(%q, %q) = %v, want %v", c.text, c.term, got, c.want)
		}
	}
}

// Entries are judged against the total the caller is charged with, the assumed
// session-start spend included: with no start entry, two ~600-byte deliveries
// cannot both fit under the allowance (9,000 + 600 + 600 > 10,000).
func TestTheAssumedStartSpendCountsInTheValidityCheck(t *testing.T) {
	f := newMomentFixture(t)
	path := filepath.Join(f.dataDir, "working-moment-assumed.log")
	for _, e := range []momentEntry{
		{token: "t1", bytes: 600, ids: []string{"a"}},
		{token: "t2", bytes: 600, ids: []string{"b"}},
	} {
		if err := appendMomentEntry(path, e); err != nil {
			t.Fatal(err)
		}
	}
	st, err := readMomentState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.valid["t1"] || st.valid["t2"] || st.spent != sessionStartByteCap+600 {
		t.Errorf("valid=%v spent=%d: the second delivery must lose against the assumed start spend", st.valid, st.spent)
	}
}

// A start hook fired again for the same session (a clear) re-prints a block: its
// ids repeat, its bytes are still spent.
func TestARepeatedSessionStartStillChargesItsBytes(t *testing.T) {
	f := newMomentFixture(t)
	path := filepath.Join(f.dataDir, "working-moment-clear.log")
	for _, e := range []momentEntry{
		{token: startToken, bytes: 5000, ids: []string{"a", "b"}},
		{token: startToken, bytes: 3000, ids: []string{"a", "b"}},
	} {
		if err := appendMomentEntry(path, e); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := readMomentState(path)
	if st.spent != 8000 || !st.delivered["a"] {
		t.Errorf("spent=%d delivered=%v, want both blocks charged (8000) and the ids kept", st.spent, st.delivered)
	}
}

func TestATruncatedPreviewEndsInOneEllipsis(t *testing.T) {
	got := momentPreview(strings.Repeat("word ", 100))
	if strings.Count(got, "…") != 1 || !strings.HasSuffix(got, "…") {
		t.Errorf("preview %q must end in exactly one ellipsis", got[len(got)-12:])
	}
	if short := "short row"; momentPreview(short) != short {
		t.Error("a row within the budget must be unchanged")
	}
}
