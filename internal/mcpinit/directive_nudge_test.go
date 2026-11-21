package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func TestIsDirectivePrompt(t *testing.T) {
	trueCases := []string{
		"never use tabs for indentation",
		"Don't commit generated files.",
		"Always run tests before pushing.",
		"Stop using fmt.Println in handlers.",
		"You must sign commits.",
		"no, that's wrong",
		"that's not how we do it",
		"thats not right",
		"That is not correct",
		"we use gofumpt in this repo",
		"we don't use globals",
		"We do not commit binaries.",
		"Please fix this. Never edit vendor files.",
	}
	for _, c := range trueCases {
		if !IsDirectivePrompt(c) {
			t.Errorf("IsDirectivePrompt(%q) = false, want true", c)
		}
	}
	falseCases := []string{
		"",
		"   ",
		"what should I do next?",
		"can you run the tests?",
		"please fix the bug in parser",
		"explain how this works",
		"there is a stop sign in the diagram",
		"this must be the issue we saw yesterday",
		strings.Repeat("a", directiveMaxLen+10),
	}
	for _, c := range falseCases {
		if IsDirectivePrompt(c) {
			t.Errorf("IsDirectivePrompt(%q) = true, want false", c)
		}
	}
}

func TestIsGenericWordIsTheSharedList(t *testing.T) {
	for _, w := range []string{"fix", "fixes", "test", "tests", "docs", "error", "go", "make", "file", "files"} {
		if !IsGenericWord(w) {
			t.Errorf("IsGenericWord(%q) = false, want true", w)
		}
	}
	for _, w := range []string{"tabs", "indentation", "migration", "quartz_widget"} {
		if IsGenericWord(w) {
			t.Errorf("IsGenericWord(%q) = true, want false", w)
		}
	}
}

func directiveContext(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		H struct {
			Ctx string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	return v.H.Ctx
}

func TestDirectivePromptEmitsNudge(t *testing.T) {
	f := newMomentFixture(t)
	out := f.submit("sess-dir", "never use tabs for indentation in this project")
	if out == "" {
		t.Fatal("no output for directive prompt")
	}
	ctx := directiveContext(t, out)
	if !strings.Contains(ctx, directiveNudge) {
		t.Fatalf("nudge missing:\n%s", ctx)
	}
}

func TestDirectivePromptNudgeIncludedWithMemoryDelivery(t *testing.T) {
	f := newMomentFixture(t)
	out := f.submit("sess-dir-mem", "never let quartz_migrate deadlock, stop sqlitebackup first")
	if out == "" {
		t.Fatal("no output")
	}
	ctx := directiveContext(t, out)
	if !strings.Contains(ctx, directiveNudge) || !strings.Contains(ctx, "mgotcha") {
		t.Fatalf("expected nudge and memory delivery:\n%s", ctx)
	}
}

func TestNonDirectivePromptNoNudge(t *testing.T) {
	f := newMomentFixture(t)
	out := f.submit("sess-nondir", "why does the quartz_migrate step deadlock on ci")
	if out == "" {
		t.Fatal("control delivery missing")
	}
	ctx := directiveContext(t, out)
	if strings.Contains(ctx, directiveNudge) {
		t.Fatalf("unexpected nudge in non-directive prompt:\n%s", ctx)
	}
}

func TestDirectiveErrorPathsEmitNothing(t *testing.T) {
	f := newMomentFixture(t)
	const prompt = "never let the quartz_migrate step deadlock"
	good := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "sess-dgood", "cwd": f.projDir, "prompt": prompt}
	if out := fireHook("message-submit", "claude-code", string(mustJSON(good))); !strings.Contains(directiveContext(t, out), directiveNudge) {
		t.Fatalf("control: the unmodified directive must nudge:\n%s", out)
	}
	n := 0
	with := func(k string, v any) string {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		n++
		m["session_id"] = fmt.Sprintf("sess-dcase-%d", n)
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return string(mustJSON(m))
	}
	cases := map[string]string{
		"no session id":   with("session_id", nil),
		"subagent":        with("agent_id", "agent-1"),
		"unknown project": with("cwd", t.TempDir()),
		"malformed json":  "{not json",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if out := fireHook("message-submit", "claude-code", payload); out != "" {
				t.Errorf("emitted:\n%s", out)
			}
		})
	}
	t.Run("codex", func(t *testing.T) {
		if out := fireHook("message-submit", "codex", with("cwd", f.projDir)); out != "" {
			t.Errorf("emitted:\n%s", out)
		}
	})
}

func TestDirectiveUnopenableDataDirEmitsNothing(t *testing.T) {
	f := newMomentFixture(t)
	if err := os.Remove(f.dbPath); err != nil {
		t.Fatal(err)
	}
	if out := f.submit("sess-dnodb", "never let the quartz_migrate step deadlock"); out != "" {
		t.Fatalf("emitted with no store to open:\n%s", out)
	}
}

func TestDirectiveRefusedDataDirEmitsNothing(t *testing.T) {
	withBuildVersion(t, devBuild)
	f := newMomentFixture(t)
	t.Setenv(config.DevForbidDataDirEnv, realPath(t, f.dataDir))
	before := dirEntries(t, f.dataDir)
	if out := f.submit("sess-dref", "never let the quartz_migrate step deadlock"); out != "" {
		t.Fatalf("emitted from a forbidden data dir:\n%s", out)
	}
	if after := dirEntries(t, f.dataDir); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("a refused data dir was written to: %v -> %v", before, after)
	}
}

func TestDirectiveWithNoTermsNudgesAlone(t *testing.T) {
	f := newMomentFixture(t)
	out := f.submit("sess-dnoterms", "never do that")
	if out == "" {
		t.Fatal("no output")
	}
	if ctx := directiveContext(t, out); ctx != directiveNudge {
		t.Fatalf("want the nudge alone, got:\n%s", ctx)
	}
}

func TestDirectiveNudgeFitsTheBlockCapAndIsCounted(t *testing.T) {
	f := newMomentFixture(t)
	db, err := memory.OpenDB(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("zebrafish_widget handling needs care. ", 12)
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance)
			VALUES (?, 'p1', 'gotcha', ?, 'manual', 0.8)`, fmt.Sprintf("mlong%d", i), long); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out := f.submit("sess-dcap", "never touch zebrafish_widget without a review")
	ctx := directiveContext(t, out)
	if !strings.HasSuffix(ctx, directiveNudge) {
		t.Fatalf("nudge missing:\n%s", ctx)
	}
	if !strings.Contains(ctx, "mlong") {
		t.Fatalf("expected a memory delivery:\n%s", ctx)
	}
	if len(ctx) > workMomentBlockBytes {
		t.Errorf("block is %d bytes, cap %d", len(ctx), workMomentBlockBytes)
	}
	st, err := readMomentState(momentMarkerPath("sess-dcap"))
	if err != nil {
		t.Fatal(err)
	}
	if st.spent < len(ctx) {
		t.Errorf("ledger spent %d, want at least the %d bytes delivered", st.spent, len(ctx))
	}
}

func TestDirectiveNudgesAloneWhenTheBudgetIsSpent(t *testing.T) {
	f := newMomentFixture(t)
	recordSessionStartDelivery("sess-dspent", nil, workMomentBudget-100)
	before, err := readMomentState(momentMarkerPath("sess-dspent"))
	if err != nil {
		t.Fatal(err)
	}
	out := f.submit("sess-dspent", "never touch quartz_migrate without stopping sqlitebackup")
	if ctx := directiveContext(t, out); ctx != directiveNudge {
		t.Fatalf("want the nudge alone with 100 bytes left, got:\n%s", ctx)
	}
	after, err := readMomentState(momentMarkerPath("sess-dspent"))
	if err != nil {
		t.Fatal(err)
	}
	if after.spent != before.spent {
		t.Errorf("spent %d -> %d, a lone nudge must not be counted", before.spent, after.spent)
	}
	if out := f.submit("sess-dspent", "why does the quartz_migrate step deadlock on ci"); out != "" {
		t.Errorf("a non-directive prompt delivered with the budget spent:\n%s", out)
	}
}
