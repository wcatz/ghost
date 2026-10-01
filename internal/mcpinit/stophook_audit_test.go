package mcpinit

// The stop hook's half of the retrieval audit: it reads the transcript, reduces
// the agent's own words to fingerprints, and hands the file to the detached
// lifecycle child on a command line.
//
// What these tests pin is the WIRING and its two boundaries. The hook must not
// open a database for this — the comparison is the child's job, and a
// synchronous path that wrote a verdict would put a store write between the
// agent and the end of its turn. And it must hand over NOTHING when the scan
// found nothing, because the one reading that would make the report confidently
// wrong is a comparison against an empty transcript: every kept memory filed as
// "ignored" on the strength of a scan that read nothing.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/memory"
)

// The id the transcript's assistant text names. Any id-shaped token will do: the
// scanner lifts ids out of prose by shape, and the point is that the name
// survives the trip rather than the words around it.
const auditTranscriptMemoryID = "D20E133860CC4AFE38B485AD5371BA59"

func auditProseLine(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text)
}

// captureLifecycleChild replaces the spawn with a recorder for the duration of
// one test and returns the argv it was handed.
//
// The executable is this test binary, so a real Start would run the whole suite
// again — and a suite that spawns a suite. Replacing the one call that starts
// the child is what makes the argv assertable at all.
func captureLifecycleChild(t *testing.T) *[][]string {
	t.Helper()
	real := startLifecycleChild
	t.Cleanup(func() { startLifecycleChild = real })

	var seen [][]string
	startLifecycleChild = func(cmd *exec.Cmd) error {
		seen = append(seen, cmd.Args)
		return nil
	}
	return &seen
}

// sidecarPathFrom returns the path the argv passes to --signals.
func sidecarPathFrom(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "--signals" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// auditHookRun is one prepared stop-hook run: an isolated home with a store, a
// project the session's cwd resolves to, and a config that lets the lifecycle
// spawn. Returns the project directory to use as the payload's cwd.
func auditHookRun(t *testing.T) (dataHome, projDir string) {
	t.Helper()
	dataHome = isolatedHome(t)
	// auto_resolve reaches the spawn without the no-LLM guard, which only gates
	// a reflect-only chain: the audit rides whatever the lifecycle already runs.
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
	t.Setenv("PATH", t.TempDir())

	projDir = filepath.Join(t.TempDir(), "audited")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	seedProject(t, dataHome, "p1", canonical, "audited")
	return dataHome, canonical
}

// TestTheStopHookHandsTheLifecycleAnAuditSidecar: the turn's evidence crosses
// into the detached child as a file, and the child is told where it is. The
// transcript is materialized per-invocation for three of the four hosts and swept
// as soon as this hook returns, so a child that had to go back for it would find
// nothing.
func TestTheStopHookHandsTheLifecycleAnAuditSidecar(t *testing.T) {
	_, projDir := auditHookRun(t)
	spawned := captureLifecycleChild(t)

	transcript := writeTranscript(t,
		lineUser,
		auditProseLine("the transcript under mkdtemp holds "+auditTranscriptMemoryID),
		lineGhostSave,
	)
	var out bytes.Buffer
	RunHostEvent("stop", "claude-code",
		strings.NewReader(stopInputIn(t, projDir, transcript, "claude-jsonl")),
		&out, io.Discard)

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1", len(*spawned))
	}
	argv := (*spawned)[0]
	path := sidecarPathFrom(t, argv)
	if path == "" {
		t.Fatalf("the child argv carries no --signals: %v", argv)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the sidecar the child was told to read does not exist: %v", err)
	}
	if !strings.HasPrefix(string(raw), audit.SidecarHeader) {
		t.Errorf("the sidecar does not carry the header ReadSidecar demands:\n%s", raw)
	}
	// The id crossed; the WORDS did not. The transcript text is what the whole
	// design keeps out of every durable artefact, and the sidecar outlives the
	// turn on disk.
	if !strings.Contains(string(raw), auditTranscriptMemoryID) {
		t.Errorf("the sidecar does not carry the id the agent named:\n%s", raw)
	}
	for _, word := range []string{"transcript", "mkdtemp"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("the sidecar carries the transcript word %q", word)
		}
	}
}

// TestTheStopHookFilesNoVerdictItself: the hook's path is synchronous and the
// agent is waiting on it, so the comparison and the write belong to the child.
// A row here would mean a store write between the agent and the end of its turn.
func TestTheStopHookFilesNoVerdictItself(t *testing.T) {
	dataHome, projDir := auditHookRun(t)
	captureLifecycleChild(t)

	transcript := writeTranscript(t, lineUser,
		auditProseLine("the transcript under mkdtemp holds "+auditTranscriptMemoryID))
	var out bytes.Buffer
	RunHostEvent("stop", "claude-code",
		strings.NewReader(stopInputIn(t, projDir, transcript, "claude-jsonl")),
		&out, io.Discard)

	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM retrieval_audit`).Scan(&n); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if n != 0 {
		t.Errorf("the hook filed %d verdict(s) on its synchronous path, want 0", n)
	}
}

// TestTheStopHookHandsOverNothingWithoutEvidence: three shapes that must produce
// no sidecar at all, because comparing against them would report a confident
// "the agent used nothing" that no transcript supports.
func TestTheStopHookHandsOverNothingWithoutEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string
		lines  []string
	}{
		{
			name:   "a transcript with no assistant-authored text",
			format: "claude-jsonl",
			// A user turn is where the injected block and every tool result
			// arrive, so a session that only listened has said nothing itself.
			lines: []string{lineUser, `{"type":"user","message":{"content":[{"type":"tool_result","content":"the injected memory text"}]}}`},
		},
		{
			name:   "a format this build has no audit scanner for",
			format: "none",
			lines:  []string{lineUser, lineToolBash},
		},
		{
			name:   "a transcript path that does not exist",
			format: "claude-jsonl",
			lines:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, projDir := auditHookRun(t)
			spawned := captureLifecycleChild(t)

			path := "/nonexistent/transcript.jsonl"
			if tc.lines != nil {
				path = writeTranscript(t, tc.lines...)
			}
			var out bytes.Buffer
			RunHostEvent("stop", "claude-code",
				strings.NewReader(stopInputIn(t, projDir, path, tc.format)),
				&out, io.Discard)

			if len(*spawned) != 1 {
				t.Fatalf("the hook started %d child(ren), want exactly 1", len(*spawned))
			}
			if got := sidecarPathFrom(t, (*spawned)[0]); got != "" {
				t.Errorf("the child was handed a sidecar for a scan with no evidence: %q", got)
			}
		})
	}
}

// TestASidecarGoesToTheTempDirAndNotTheRepository: it is a temp file handed to a
// detached child, so its directory is a parameter of the write rather than a
// property of wherever the session happened to be running.
func TestASidecarGoesToTheTempDirAndNotTheRepository(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { auditSidecarDir = func() string { return os.TempDir() } })
	auditSidecarDir = func() string { return dir }

	s := &audit.Signals{}
	s.AddProse("the transcript under mkdtemp holds " + auditTranscriptMemoryID)
	path, err := audit.WriteSidecar(auditSidecarDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("the sidecar landed in %q, want the temp dir %q", filepath.Dir(path), dir)
	}
}

// stopInputIn is stopInput with the cwd and transcript format selectable, because
// the spawn resolves the project from the cwd and the scanner is chosen by
// format.
func stopInputIn(t *testing.T, cwd, transcriptPath, format string) string {
	t.Helper()
	return contractInputFor(t, "Stop", "claude-code", format,
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":%q,"stop_hook_active":false}`,
			transcriptPath, cwd))
}
