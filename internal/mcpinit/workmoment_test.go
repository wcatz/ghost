package mcpinit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	momentGotcha = "Gotcha: the quartz_migrate step deadlocks when sqlitebackup runs at the same time; always stop sqlitebackup before it."
	momentFile   = "Editing stophook.go needs the lifecycle cooldown stamp cleared first, otherwise the spawn is silently skipped."
	// momentFiller shares ordinary words ("cloud", "build") with the unrelated
	// message, so only the floor keeps it out.
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
		{"mglobal", memory.GlobalProjectID, "preference", "Global preference: quartz_migrate deadlock reports are always verbose."},
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
	out := f.submit("sess-a", "why does the quartz_migrate step deadlock on ci")
	text, _ := momentContext(t, out)
	if !strings.Contains(text, "sqlitebackup") || !strings.Contains(text, "`mgotcha`") {
		t.Fatalf("the saved gotcha was not delivered:\n%s", text)
	}
	if strings.Contains(text, "mglobal") {
		t.Errorf("a `_global` row was delivered; the channel is the project's own memories:\n%s", text)
	}
	if again := f.submit("sess-a", "remind me about quartz_migrate and sqlitebackup"); again != "" {
		t.Fatalf("a second matching message repeated a delivered row:\n%s", again)
	}
	if other := f.submit("sess-b", "why does the quartz_migrate step deadlock on ci"); other == "" {
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
	if out := f.submit("sess-s", "why does the quartz_migrate step deadlock on ci"); out != "" {
		t.Fatalf("a row the session-start block already delivered was repeated:\n%s", out)
	}
}

func TestAnUnrelatedMessageEmitsNothing(t *testing.T) {
	f := newMomentFixture(t)
	// Shares one ordinary word ("cloud") with a stored row: kept out by the floor.
	if out := f.submit("sess-u", "what is the weather in the cloud today"); out != "" {
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
	// hook.go shares only an ordinary word with a hooks memory: below the floor.
	if out := f.edit("sess-e4", "Edit", filepath.Join(f.projDir, "hook.go"), "x := 1"); out != "" {
		t.Errorf("an edit of hook.go delivered a memory that only shares the word hook:\n%s", out)
	}
}

func TestTheDeliveryIsRecordedWithItsOwnSource(t *testing.T) {
	f := newMomentFixture(t)
	if out := f.submit("sess-r", "why does the quartz_migrate step deadlock on ci"); out == "" {
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
	if c.queryHash == "" || strings.Contains(c.verdicts, "quartz_migrate") {
		t.Errorf("the record must carry a digest and no text: hash=%q verdicts=%s", c.queryHash, c.verdicts)
	}
}

func TestTheSharedBudgetIsRespected(t *testing.T) {
	f := newMomentFixture(t)
	msg := "why does the quartz_migrate step deadlock on ci"

	recordSessionStartDelivery("sess-b1", nil, workMomentBudget-100)
	if out := f.submit("sess-b1", msg); out != "" {
		t.Errorf("delivered with 100 bytes left:\n%s", out)
	}

	recordSessionStartDelivery("sess-b2", nil, workMomentBudget-300)
	text, _ := momentContext(t, f.submit("sess-b2", "quartz_migrate sqlitebackup stophook.go cooldown"))
	if len(text) == 0 || len(text) > 300 || strings.Count(text, "\n- [") != 1 {
		t.Errorf("block is %d bytes with 300 left, want exactly one row:\n%s", len(text), text)
	}

	text, _ = momentContext(t, f.submit("sess-b3", msg))
	if len(text) > workMomentBudget-sessionStartByteCap {
		t.Errorf("block is %d bytes, but an unknown session start leaves only %d", len(text), workMomentBudget-sessionStartByteCap)
	}

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
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if strings.Contains(f.submit("sess-p", "why does the quartz_migrate step deadlock on ci"), "mgotcha") {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got != 1 {
		t.Fatalf("%d parallel hooks delivered the row, want exactly 1", got)
	}
}

func TestWorkingMomentErrorPathsEmitNothing(t *testing.T) {
	f := newMomentFixture(t)
	good := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "sess-x", "cwd": f.projDir,
		"prompt": "why does the quartz_migrate step deadlock on ci"}
	n := 0
	with := func(k string, v any) string { // a session of its own per case
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
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
		t.Fatal("control: the unmodified payload must deliver")
	}
	cases := map[string]struct{ event, source, payload string }{
		"malformed json":  {"message-submit", "claude-code", "{not json"},
		"no session id":   {"message-submit", "claude-code", with("session_id", nil)},
		"subagent":        {"message-submit", "claude-code", with("agent_id", "agent-1")},
		"unknown project": {"message-submit", "claude-code", with("cwd", t.TempDir())},
		"only stopwords":  {"message-submit", "claude-code", with("prompt", "what is this and that")},
		"event mismatch":  {"message-submit", "claude-code", with("hook_event_name", "Stop")},
		"codex":           {"message-submit", "codex", with("cwd", f.projDir)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if out := fireHook(c.event, c.source, c.payload); out != "" {
				t.Errorf("emitted:\n%s", out)
			}
		})
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestARefusedDataDirEmitsNothingAndWritesNothing(t *testing.T) {
	withBuildVersion(t, devBuild)
	f := newMomentFixture(t)
	t.Setenv(config.DevForbidDataDirEnv, realPath(t, f.dataDir))
	before := dirEntries(t, f.dataDir)
	if out := f.submit("sess-f", "why does the quartz_migrate step deadlock on ci"); out != "" {
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
}

// The marker is named by a digest of the free-text session id, so two ids can
// never share a ledger.
func TestTheSessionIdIsDigestedIntoTheMarkerName(t *testing.T) {
	newMomentFixture(t)
	for _, id := range []string{"abc-123", "../../evil", "a/b\\c:d e", strings.Repeat("z", 200)} {
		p := momentMarkerPath(id)
		base := filepath.Base(p)
		if filepath.Base(filepath.Dir(p)) != "ghost" {
			t.Errorf("marker for %q = %q, want a file directly in the data dir", id, p)
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(base, workMomentMarkerPrefix), workMomentMarkerSuffix)
		raw, err := hex.DecodeString(stem)
		if stem == base || err != nil || len(raw) != 16 {
			t.Errorf("marker for %q = %q, want %s<16 hex-encoded bytes>%s", id, base, workMomentMarkerPrefix, workMomentMarkerSuffix)
		}
	}
	// Two ids that fold onto one sanitized name must not share a file.
	for _, pair := range [][2]string{{"a b", "a_b"}, {"sess a", "sess_a"}} {
		if p, q := momentMarkerPath(pair[0]), momentMarkerPath(pair[1]); p == q {
			t.Errorf("%q and %q share the marker %q", pair[0], pair[1], p)
		}
	}
}

// A marker an older build wrote under the sanitized stem is never read (that
// name is not injective) and the age sweep removes it.
func TestAMarkerUnderTheOldNameIsNeverReadAndIsSwept(t *testing.T) {
	f := newMomentFixture(t)
	legacy := filepath.Join(f.dataDir, "working-moment-sess_a.log")
	if err := os.WriteFile(legacy, []byte("D start 9000 mgotcha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := readMomentState(momentMarkerPath("sess a"))
	if err != nil {
		t.Fatal(err)
	}
	if st.delivered["mgotcha"] {
		t.Errorf("the old name's marker was read: %v", st.delivered)
	}
	aged := time.Now().Add(-workMomentMarkerMaxAge - time.Hour)
	if err := os.Chtimes(legacy, aged, aged); err != nil {
		t.Fatal(err)
	}
	recordSessionStartDelivery("sess-sweep", []string{"x"}, 10)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("a marker under the old name survived the sweep: %v", err)
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
		if err := ensureWorkingMomentHooks(&out, sf, "/opt/ghost", false); err != nil {
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
}

func TestWorkingMomentCapabilityIsClaudeCodeOnly(t *testing.T) {
	for _, s := range []hostevent.Source{hostevent.SourceClaudeCode, hostevent.SourceCodex, hostevent.SourceGoose, hostevent.SourceOpencode} {
		c, ok := hostevent.CapabilityFor(s)
		if !ok || c.WorkingMoment != (s == hostevent.SourceClaudeCode) {
			t.Errorf("%s WorkingMoment = %v", s, c.WorkingMoment)
		}
	}
}

// With the relevance cutoff off the keyword leg returns the `_global` row too;
// only the project filter keeps it out.
func TestAGlobalRowIsNeverDelivered(t *testing.T) {
	f := newMomentFixture(t)
	t.Setenv("GHOST_CONTEXT_RELEVANCE_CUTOFF", "0")
	text, _ := momentContext(t, f.submit("sess-g", "why does the quartz_migrate step deadlock on ci"))
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
	if out := f.submit("sess-n", "why does the quartz_migrate step deadlock on ci"); out == "" {
		t.Fatal("no first delivery")
	}
	text, _ := momentContext(t, f.submit("sess-n", "quartz_migrate sqlitebackup stophook.go cooldown"))
	if !strings.Contains(text, "`mfile`") || strings.Contains(text, "`mgotcha`") {
		t.Errorf("want only the new row (mfile):\n%s", text)
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
		{"xhook.go", "hook.go", false},
		{"hook.gox hook.go", "hook.go", true},
	} {
		if got := containsBounded(c.text, c.term); got != c.want {
			t.Errorf("containsBounded(%q, %q) = %v, want %v", c.text, c.term, got, c.want)
		}
	}
}

func TestATruncatedPreviewEndsInOneEllipsis(t *testing.T) {
	got := momentPreview(strings.Repeat("word ", 100))
	if strings.Count(got, "…") != 1 || !strings.HasSuffix(got, "…") {
		t.Errorf("preview %q must end in exactly one ellipsis", got[len(got)-12:])
	}
}

// A symlink at the working-moment marker path must not be written through:
// appendMomentEntry refuses it and leaves the link target untouched.
func TestWorkingMomentMarkerAppendDoesNotWriteThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not available on this platform")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "working-moment-link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := appendMomentEntry(link, momentEntry{token: "t", bytes: 1, ids: []string{"a"}}); err == nil {
		t.Error("appendMomentEntry through a symlink: want an error, got nil")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Errorf("symlink target changed: got %q, want %q", string(got), "keep")
	}
}

// Two session ids that fold onto one sanitized name — one holding a space
// where the other holds an underscore, or two sharing their first 80
// characters — must not share a ledger: the second session would find the row
// already delivered and its bytes already spent.
func TestTwoSessionIdsThatFoldOntoOneNameKeepSeparateLedgers(t *testing.T) {
	f := newMomentFixture(t)
	const msg = "why does the quartz_migrate step deadlock on ci"
	for _, c := range []struct{ name, first, second string }{
		{"a space against an underscore", "sess a", "sess_a"},
		{"a shared 80-character prefix", strings.Repeat("a", 80) + "1", strings.Repeat("a", 80) + "2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := f.submit(c.first, msg); out == "" {
				t.Fatalf("control: the first session delivered nothing")
			}
			if out := f.submit(c.second, msg); out == "" {
				t.Errorf("%q and %q share one ledger, so the second session was silenced by the first", c.first, c.second)
			}
		})
	}
}
