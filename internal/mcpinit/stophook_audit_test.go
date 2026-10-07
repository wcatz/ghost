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
	"errors"
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
	projDir, dataHome = isolatedProjectDir(t)
	// The install key. A live one always exists — the MCP server resolves it at
	// startup — and the hook READS it and never creates it (memory.ReadRetrievalKey
	// refuses to), so without a key in this sandbox the audit is off and every
	// test below would be vacuously green.
	seedRetrievalKey(t, dataHome)
	return dataHome, projDir
}

// seedRetrievalKey writes an install key where a real install has one.
//
// The FILE is written directly rather than through memory.WarmQueryKey, and the
// reason is the key cache: it is a package-level one-key-per-process cache, so in
// a suite where each test gets its own sandbox the first test to warm it poisons
// every later one — WarmQueryKey would report the first sandbox's key and write
// no file here at all. The hook's contract is to read this file, so writing it is
// the fixture that matches the contract. The format is the real one (32 bytes,
// lower-case hex, 0600) because the reader classifies it.
func seedRetrievalKey(t *testing.T, dataHome string) {
	t.Helper()
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir the data directory: %v", err)
	}
	// Fixed rather than random: a test that reads the key back (auditTestKey) and a
	// test that signs a sidecar have to agree, and a random key per call would make
	// the second of those fail for a reason that has nothing to do with the code.
	const keyHex = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	path := filepath.Join(dir, "retrieval-record.key")
	if err := os.WriteFile(path, []byte(keyHex), 0o600); err != nil {
		t.Fatalf("write the install key: %v", err)
	}
}

// auditTestKey is the sandbox's install key, which is the key the hook signs the
// sidecar with — so a test that signs with a literal of its own is signing with
// something the child could never reproduce.
func auditTestKey(t *testing.T) []byte {
	t.Helper()
	key, err := memory.ReadRetrievalKey()
	if err != nil {
		t.Fatalf("ReadRetrievalKey: %v", err)
	}
	return key
}

// isolatedProjectDir is auditHookRun without its install key, for the one test
// whose subject IS the absent key. Everything else about the sandbox — the
// auto_resolve config that reaches the spawn, the project the cwd resolves to —
// has to match, or the chain would not run for a second and unrelated reason and
// the test would prove nothing.
func isolatedProjectDir(t *testing.T) (projDir, dataHome string) {
	t.Helper()
	dataHome = isolatedHome(t)
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
	return canonical, dataHome
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

// TestASidecarWrittenForAChildThatNeverStartedIsRemoved: the sidecar is written
// BEFORE the spawn, because writing it after would mean the file outlives the
// turn whenever the spawn is refused — and the spawn is refused for ordinary
// reasons (no executable, a full disk, a fork limit). A file nobody reads and
// nobody owns is what the sweep exists for, but waiting a day for it on every
// failed spawn is the sweep doing housekeeping that the hook could have done at
// the moment it knew.
func TestASidecarWrittenForAChildThatNeverStartedIsRemoved(t *testing.T) {
	_, projDir := auditHookRun(t)

	real := startLifecycleChild
	t.Cleanup(func() { startLifecycleChild = real })
	var handed [][]string
	startLifecycleChild = func(cmd *exec.Cmd) error {
		handed = append(handed, cmd.Args)
		return errors.New("cannot fork")
	}

	transcript := writeTranscript(t,
		lineUser,
		auditProseLine("per "+auditTranscriptMemoryID+" the hook sweeps on close"),
		lineGhostSave,
	)
	var out bytes.Buffer
	RunHostEvent("stop", "claude-code",
		strings.NewReader(stopInputIn(t, projDir, transcript, "claude-jsonl")),
		&out, io.Discard)

	if len(handed) != 1 {
		t.Fatalf("the hook tried to start %d child(ren), want exactly 1", len(handed))
	}
	path := sidecarPathFrom(t, handed[0])
	if path == "" {
		t.Fatal("no sidecar was written, so this case proves nothing about removing one")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a sidecar written for a child that never started is still there: %v", err)
	}
}

// TestNoKeyMeansNoSidecarAndStillALifecycle: the degradation the key forces, and
// it is the one case in this file where the answer is "nothing happened".
//
// The hook resolves the install key READ-ONLY, so a store that has never recorded
// a retrieval — and so has no key file — produces no sidecar. The alternative was
// available and was the one this design refuses: hash the words anyway, in the
// clear, into a file in the shared temp directory. An absent sidecar costs this
// turn's audit; an unkeyed one costs the property the sidecar is for, on a turn
// nobody was watching.
//
// The other half is that the lifecycle chain is UNAFFECTED. The audit rides
// whatever the lifecycle already runs, so a missing key must not cost the phases
// that maintain the store — those are worth running whether or not this turn
// produced evidence.
func TestNoKeyMeansNoSidecarAndStillALifecycle(t *testing.T) {
	projDir, dataHome := isolatedProjectDir(t)
	// Deliberately NO seedRetrievalKey here: the key file is absent, which is the
	// whole condition under test.
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", "retrieval-record.key")); !os.IsNotExist(err) {
		t.Fatalf("this test needs NO install key, but one exists: %v", err)
	}
	spawned := captureLifecycleChild(t)

	transcript := writeTranscript(t,
		lineUser,
		auditProseLine("the transcript under mkdtemp holds "+auditTranscriptMemoryID),
		lineGhostSave,
	)
	var out, errOut bytes.Buffer
	RunHostEvent("stop", "claude-code",
		strings.NewReader(stopInputIn(t, projDir, transcript, "claude-jsonl")),
		&out, &errOut)

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1: a missing key must not "+
			"cost the lifecycle chain", len(*spawned))
	}
	if got := sidecarPathFrom(t, (*spawned)[0]); got != "" {
		t.Errorf("the child was handed a sidecar signed with no key: %q", got)
	}
	// And the operator is told, on the channel the hook already fails open on,
	// because a silently absent audit is indistinguishable from an agent that used
	// nothing.
	if !strings.Contains(errOut.String(), "retrieval key") {
		t.Errorf("nothing on stderr said why the audit was skipped:\n%s", errOut.String())
	}
}

// TestASidecarGoesToTheTempDirAndNotTheRepository: it is a temp file handed to a
// detached child, so its directory is a parameter of the write rather than a
// property of wherever the session happened to be running.
func TestASidecarGoesToTheTempDirAndNotTheRepository(t *testing.T) {
	// The sandbox and its install key, because a sidecar can only be written
	// with one and the hook's key is the sandbox's.
	auditHookRun(t)
	dir := t.TempDir()
	t.Cleanup(func() { auditSidecarDir = func() string { return os.TempDir() } })
	auditSidecarDir = func() string { return dir }

	hasher, err := audit.NewHasher(auditTestKey(t))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	s := audit.NewWithHasher(hasher)
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

// TestTheStopHookNamesTheSessionInTheSidecar: the verdicts a run files are about one
// session's text, so the file the child reads must say which session that is, taken from
// the hook payload's own session_id. Without it the child can only judge the project's
// recent calls whatever session made them.
func TestTheStopHookNamesTheSessionInTheSidecar(t *testing.T) {
	_, projDir := auditHookRun(t)
	spawned := captureLifecycleChild(t)

	transcript := writeTranscript(t, lineUser,
		auditProseLine("the transcript under mkdtemp holds "+auditTranscriptMemoryID), lineGhostSave)
	RunHostEvent("stop", "claude-code",
		strings.NewReader(stopInputIn(t, projDir, transcript, "claude-jsonl")),
		&bytes.Buffer{}, io.Discard)

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want 1", len(*spawned))
	}
	path := sidecarPathFrom(t, (*spawned)[0])
	sig, err := audit.ReadSidecar(path, mustAuditHasher(t))
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if sig.SessionID() != "s1" {
		t.Errorf("sidecar session = %q, want the payload's session_id %q", sig.SessionID(), "s1")
	}
}

func mustAuditHasher(t *testing.T) audit.Hasher {
	t.Helper()
	h, err := audit.NewHasher(auditTestKey(t))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}
