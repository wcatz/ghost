package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// alertBudgetProjectID is the project the budget fixture's session resolves
// to: an id of 300 bytes, which is the size the marker's own project field can
// hold — nothing upstream bounds the operand a marker records, so a marker
// written by hand can name a project this long, and the alert renders it
// TWICE on the one line every session opens with.
var alertBudgetProjectID = strings.Repeat("p", 300)

// alertBudgetSummaryBytes is the learned summary the fixture stores: the
// field's own cap (sessionLearnedBytes), so the framing carries what the
// rows cannot.
const alertBudgetSummaryBytes = 1000

// alertBudgetName is the project's NAME in the budget fixture: long enough
// that the block reaches the byte cap. The name is the framing's fine lever —
// the block prints it twice, so two bytes at a time — and it is what puts the
// block within a couple of bytes of the cap, which is the state the issue's
// reproduction is about.
var alertBudgetName = strings.Repeat("n", 700)

// seedAlertBudgetStore builds the store the session-start budget fixture
// needs: a project the session resolves to, and a block that REACHES the byte
// cap.
//
// The block is over the cap on purpose — the framing (the learned summary,
// the tasks, the decisions) plus the rows is more than the cap allows — so
// the fit pass gives up rows until the block fits, which is the state the
// issue's reproduction is about: a store whose block reaches the cap, with an
// alert printed ahead of it on the same stdout.
//
// The rows are kept small on purpose. The fit pass gives up WHOLE rows, so a
// row of 200 bytes would leave the block anywhere in the last 200 bytes below
// the cap, and the fixture would not reliably be the case under test. A small
// row leaves the block within a few bytes of the cap, and the project name —
// printed twice in the framing, so two bytes at a time — is the fine lever
// that puts it there.
func seedAlertBudgetStore(t *testing.T, xdgHome string) (projectPath string) {
	t.Helper()
	projectPath = filepath.Join(t.TempDir(), "alertproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	projectPath = canonical

	if err := os.MkdirAll(filepath.Join(xdgHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	dbPath := filepath.Join(xdgHome, "ghost", "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, alertBudgetProjectID, projectPath, alertBudgetName)
	exec(`INSERT INTO ghost_state (project_id, learned_context) VALUES (?, ?)`,
		alertBudgetProjectID, strings.Repeat("Learned summary sentence. ", 40)[:alertBudgetSummaryBytes])

	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	insert := func(id, project, category, content string) {
		exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'manual', 0.5, 0, ?, ?)`, id, project, category, content, ts, ts)
	}
	for i := range 15 {
		insert(fmt.Sprintf("pmem%02d", i), alertBudgetProjectID, "fact", fmt.Sprintf("row %02d", i))
	}
	for i := range 8 {
		insert(fmt.Sprintf("gmem%02d", i), "_global", "fact", fmt.Sprintf("g %02d", i))
	}
	// The tasks and the decisions carry the rest of the framing: the block's
	// own fields, each at its own cap.
	for i := range 10 {
		exec(`INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, ?, ?, ?, 'pending', 1)`,
			fmt.Sprintf("task%02d", i), alertBudgetProjectID, strings.Repeat("Task title words ", 10), strings.Repeat("Task description. ", 14))
	}
	for i := range 5 {
		exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status) VALUES (?, ?, ?, ?, 'why', 'active')`,
			fmt.Sprintf("dec%02d", i), alertBudgetProjectID, strings.Repeat("Decision title words ", 8), strings.Repeat("Decision body. ", 14))
	}
	return projectPath
}

// alertBudgetBlock renders the fixture's session-start block the way the hook
// does, so a test can state the block's size before it says anything about
// the output that carries it.
func alertBudgetBlock(t *testing.T, dbPath, projectPath string) string {
	t.Helper()
	cfg := mustHookConfig(t)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	projectID, project, memories, globals, learned, tasks, decisions, interactionCount, tally :=
		loadSessionContextFrom(dbPath, projectPath, cfg, func() time.Time { return now }, "")
	return formatSessionContext(projectID, project, nil, memories, learned, tasks, decisions, interactionCount, globals, tally)
}

// TestSessionStart_AlertAndBlockStayUnderTheHostLimit is the issue's
// reproduction.
//
// The session-start block is capped at sessionStartByteCap, and the margin
// under the host's 10,000-character limit is meant to cover the alert line
// printed ahead of the block on the same stdout. The alert was not measured
// and not bounded: it renders the marker's project twice (300 bytes each) and
// the error beside it (300 bytes), so the alert alone reached about 1,200
// bytes — more than the margin — and the block's own cap did not know the
// alert was there.
//
// So a store whose block reaches the cap, plus a marker naming a 300-byte
// project with a long error, produced a session-start output over the host's
// limit, and the host replaced the whole block with a preview.
func TestSessionStart_AlertAndBlockStayUnderTheHostLimit(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	projectPath := seedAlertBudgetStore(t, xdgHome)

	// The fixture, not the behaviour: a block well under the cap cannot reach
	// the limit with any alert, so the case under test would not be reached.
	block := alertBudgetBlock(t, filepath.Join(xdgHome, "ghost", "ghost.db"), projectPath)
	if len(block) < sessionStartByteCap-200 {
		t.Fatalf("fixture block is %d bytes, want within 200 of the %d cap: the block has to reach the cap for the "+
			"alert to have anything to push over the host's limit", len(block), sessionStartByteCap)
	}

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project:      alertBudgetProjectID,
		PhasesFailed: []string{"reflect"},
		Error:        "error: " + strings.Repeat("the harness said no ", 30),
		At:           at,
		Version:      1,
	})

	_, out := alertFirstLine(t, projectPath)

	if len(out) > sessionHostLimit {
		t.Errorf("the session-start output is %d bytes, over the host's %d-character limit: the alert is printed "+
			"ahead of the block on the same stdout, so the two share the limit\n%s", len(out), sessionHostLimit, out)
	}
}

// TestSessionStart_AlertLineIsBounded pins the alert's own cap: the line is
// printed at the top of every session for the marker's lifetime, and the
// fields on it are read from a file any local process can write, so the line
// carries a byte cap of its own rather than the sum of its parts.
func TestSessionStart_AlertLineIsBounded(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project:      alertBudgetProjectID,
		PhasesFailed: []string{"reflect"},
		Error:        "error: " + strings.Repeat("the harness said no ", 30),
		At:           at,
		Version:      1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the marker must still alert, got:\n%s", out)
	}
	if len(firstLine) > sessionHostLimit-sessionStartByteCap {
		t.Errorf("the alert line is %d bytes, over the %d-byte margin the block's cap leaves under the host's limit: "+
			"the line is printed ahead of the block on the same stdout and the host's limit covers the two together\n%s",
			len(firstLine), sessionHostLimit-sessionStartByteCap, firstLine)
	}
}

// TestSessionStart_AlertNamesOnlyThePhasesItCanCarry pins the phase list's
// bound: the list is read from the marker file verbatim, and a marker written
// by hand can hold any number of entries, so the alert names the ones it can
// carry and counts the ones it left out.
func TestSessionStart_AlertNamesOnlyThePhasesItCanCarry(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	phases := make([]string, 0, 40)
	for i := range 40 {
		phases = append(phases, fmt.Sprintf("phase-%02d", i))
	}
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project:      "p1",
		PhasesFailed: phases,
		Error:        "boom",
		At:           at,
		Version:      1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the marker must still alert, got:\n%s", out)
	}
	// The entries the alert can carry are named, and the ones it left out are
	// counted: a phase the reader cannot see is a phase the reader knows is
	// there.
	named := 0
	for _, p := range phases {
		if !strings.Contains(firstLine, p) {
			break
		}
		named++
	}
	if named == 0 || named == len(phases) {
		t.Fatalf("the alert must name the phases it can carry and leave the rest out, named %d of %d: %s",
			named, len(phases), firstLine)
	}
	if !strings.Contains(firstLine, fmt.Sprintf("+%d more", len(phases)-named)) {
		t.Errorf("the alert must count the %d phases it left out: %s", len(phases)-named, firstLine)
	}
}

// TestSessionStart_AlertKeepsTheReason pins the order the alert's parts are
// cut in: the error line says WHY the consolidation failed, so it is the last
// thing the line's budget takes, and a marker with a long project beside it
// still shows the reason.
func TestSessionStart_AlertKeepsTheReason(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project:      alertBudgetProjectID,
		PhasesFailed: []string{"reflect"},
		Error:        "error: the harness said no because the model refused",
		At:           at,
		Version:      1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the marker must still alert, got:\n%s", out)
	}
	if !strings.Contains(firstLine, "the harness said no because the model refused") {
		t.Errorf("the alert must keep the reason the phase failed: %s", firstLine)
	}
}

// alertErrorRenderBytes is the bound the alert's error field renders inside.
// It is the production cap (lifecycleMarkerErrorRenderMaxBytes) written out
// here, so the test states the bound rather than reading it back out of the
// code it is checking.
const alertErrorRenderBytes = 320

// TestSessionStart_AlertKeepsAnEscapedErrorInsideItsBound pins the error
// field's bound as it RENDERS: the quoting escapes a control rune to up to
// six bytes, so a stored error carrying them renders longer than the content
// it was cut to — a phase's stderr carries colour codes often enough for
// that to be the ordinary case rather than a corner.
func TestSessionStart_AlertKeepsAnEscapedErrorInsideItsBound(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		Project:      "p1",
		PhasesFailed: []string{"reflect"},
		Error:        "error: " + strings.Repeat("\x1b[31mred\x1b[0m ", 40),
		At:           at,
		Version:      1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the marker must still alert, got:\n%s", out)
	}
	_, field, ok := strings.Cut(firstLine, "«")
	if !ok {
		t.Fatalf("the alert must frame the error as data, got:\n%s", out)
	}
	field, _, ok = strings.Cut(field, "»")
	if !ok {
		t.Fatalf("the error frame must be closed, got:\n%s", out)
	}
	if len(field) > alertErrorRenderBytes {
		t.Errorf("the error field renders %d bytes, over the %d-byte bound: the quoting escapes a control rune to up "+
			"to six bytes, so the field is capped as it renders and not as it is stored\n%s",
			len(field), alertErrorRenderBytes, firstLine)
	}
}

// TestSessionStart_ProjectCutNeverSplitsAnEscape pins the project's cut.
//
// The value is cut on the RAW side and the ESCAPED form is what the share is
// measured against, so the cut is always a whole escape sequence. Cutting the
// escaped form instead left "\x" where the value held "\x1b", and it could cut
// a backtick's escape in half — which is the escape that keeps the retry
// command's own backtick from closing the span early.
//
// The value carries a control rune and a pair of backticks, which are the two
// escapes the renderer writes, and the shares run from the smallest that can
// hold a character to the one the alert's own budget hands out.
func TestSessionStart_ProjectCutNeverSplitsAnEscape(t *testing.T) {
	raw := "proj\x1b`name`tail"
	label := assemble.Label(raw)
	for _, share := range []int{1, 2, 3, 4, 5, 8, 12, 16, 24, 32, 48, 64, 89, 128, 300} {
		got := renderMarkerProject(raw, share)
		if !wholeEscape(got) {
			t.Errorf("share %d: the rendered project carries a half-written escape: %q", share, got)
		}
		// The cut removes a SUFFIX of the value and nothing else: what is
		// left is a prefix of the whole rendering, so no escape sequence is
		// cut in half.
		if trimmed := strings.TrimSuffix(got, "…"); !strings.HasPrefix(label, trimmed) {
			t.Errorf("share %d: the rendered project is not a prefix of the whole rendering: %q", share, got)
		}
		if len(got) > share {
			t.Errorf("share %d: the rendered project is %d bytes: %q", share, len(got), got)
		}
	}
}

// TestSessionStart_ProjectCutLeavesTheRetryCommandSpanned pins the same
// property on the line the reader is told to run: the alert prints the
// project inside a backtick span, so a half-written escape in the project
// would swallow the command's own closing backtick.
func TestSessionStart_ProjectCutLeavesTheRetryCommandSpanned(t *testing.T) {
	isolatedHome(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeMarkerFile(t, xdgHome, testMarker{
		// The prefix is three bytes so the cut lands INSIDE one of the
		// escapes rather than between two of them, which is where a cut
		// that splits one shows.
		Project:      "pro" + strings.Repeat("\x1b`", 100),
		PhasesFailed: []string{"reflect"},
		Error:        "error: " + strings.Repeat("the harness said no ", 30),
		At:           at,
		Version:      1,
	})

	firstLine, out := alertFirstLine(t, filepath.Join(t.TempDir(), "no-such-project"))

	if !strings.HasPrefix(firstLine, "**Ghost maintenance alert:**") {
		t.Fatalf("the marker must still alert, got:\n%s", out)
	}
	if !wholeEscape(firstLine) {
		t.Errorf("the alert line carries a half-written escape, which swallows the retry command's own backtick: %q",
			firstLine)
	}
	if !strings.Contains(firstLine, "` to retry") {
		t.Errorf("the retry command's span must stay closed: %q", firstLine)
	}
}

// TestSessionStart_PhaseEntryKeepsItsQuoting pins the phase entry's cut: the
// entry is cut on the raw value and the rendered entry is what the bound is
// checked against, so the quoting it carries is never left open and its
// escapes are never half-written.
//
// A space puts the entry inside quotes and a control rune escapes it, and
// the names carry the escape at a different offset each time, so the cut
// lands inside one of them somewhere in the list.
func TestSessionStart_PhaseEntryKeepsItsQuoting(t *testing.T) {
	for i := range 10 {
		raw := fmt.Sprintf("phase-%d \x1b[31m", i) + strings.Repeat("x", i*7)
		list := renderMarkerPhases([]string{raw})
		if !wholeEscape(list) {
			t.Errorf("the phase list carries a half-written escape: %q", list)
		}
		if strings.Count(list, `"`)%2 != 0 {
			t.Errorf("the phase list carries an odd number of quotes, so an entry's quoting is left open: %q", list)
		}
	}

	// The entry's own bound, from the smallest that can hold a character
	// to the one the list hands out.
	raw := "reflect phase\x1b[31m"
	for _, bound := range []int{1, 2, 3, 4, 8, 16, 32, 64} {
		got, ok := markerPhaseEntry(raw, bound)
		if !ok {
			// The other answer: an entry that cannot be shown at all is
			// dropped and counted, never shown half.
			continue
		}
		if !wholeEscape(got) {
			t.Errorf("bound %d: the rendered entry carries a half-written escape: %q", bound, got)
		}
		if strings.Count(got, `"`)%2 != 0 {
			t.Errorf("bound %d: the rendered entry carries an odd number of quotes, so its quoting is left open: %q",
				bound, got)
		}
		if len(got) > bound {
			t.Errorf("bound %d: the rendered entry is %d bytes: %q", bound, len(got), got)
		}
	}
}

// wholeEscape reports whether every backslash in s begins a whole escape
// sequence. The renderers write Go escapes (a control rune) and the backtick
// escape assemble.Label writes, so a cut that lands inside one leaves a
// dangling backslash or a half-written "\x1".
func wholeEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		i++
		if i >= len(s) {
			return false
		}
		switch c := s[i]; {
		case c == '`' || c == '\\' || c == '"' || c == '\'':
		case c == 'x':
			if i+2 >= len(s) || !isHexDigit(s[i+1]) || !isHexDigit(s[i+2]) {
				return false
			}
			i += 2
		case c == 'u':
			if i+4 >= len(s) {
				return false
			}
			for k := 1; k <= 4; k++ {
				if !isHexDigit(s[i+k]) {
					return false
				}
			}
			i += 4
		case c == 'U':
			if i+8 >= len(s) {
				return false
			}
			for k := 1; k <= 8; k++ {
				if !isHexDigit(s[i+k]) {
					return false
				}
			}
			i += 8
		case c == 'a' || c == 'b' || c == 'f' || c == 'n' || c == 'r' || c == 't' || c == 'v':
		default:
			return false
		}
	}
	return true
}
