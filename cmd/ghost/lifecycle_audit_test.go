package main

// The lifecycle child's half of the retrieval audit: it reads the sidecar the
// stop hook left, compares the session's retrievals against it, persists the
// verdicts, and deletes the file.
//
// The property these tests care about most is the fail-open one. This process is
// detached, nobody reads its exit status, and the phases behind it are the ones
// that maintain the store — so a sidecar that cannot be read, a store that
// cannot be opened, a project that resolves to nothing: each must cost the audit
// and nothing else. The reflect marker is the witness that the chain ran on.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	// auditLifecycleProject is the id and name the run below resolves.
	auditLifecycleProject = "projx"
	// auditLifecycleMemory is the memory the recorded call kept.
	auditLifecycleMemory = "D20E133860CC4AFE38B485AD5371BA59"
	// auditLifecycleContent is its wording, and the agent's prose below repeats
	// enough of it for the token arm to clear — five of the eight distinctive
	// words, where the arm asks for a third and never fewer than three.
	auditLifecycleContent = "the scratch directory is reaped before each lifecycle run begins"
)

// seedAuditedCall writes a store holding one project, one memory and one
// recorded call that kept it. That is the smallest shape audit.Run judges: a
// (call, memory) pair and a transcript to judge it against.
func seedAuditedCall(t *testing.T, dataHome string) {
	t.Helper()
	// The audit phase opens the same file through config.DataDir; the seed store
	// gets there first, so the directory has to exist already.
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
	if err := store.EnsureProject(ctx, auditLifecycleProject, filepath.Join(t.TempDir(), auditLifecycleProject), auditLifecycleProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.CreateWithID(ctx, auditLifecycleProject, auditLifecycleMemory, memory.Memory{
		Content:  auditLifecycleContent,
		Category: "gotcha",
		Source:   "manual",
	}); err != nil {
		t.Fatalf("seed the memory: %v", err)
	}
	if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
		ProjectID: auditLifecycleProject,
		SessionID: "s1",
		Source:    "search",
		Outcome:   "answerable",
		Verdicts: []memory.RowVerdict{
			{ID: auditLifecycleMemory, Kept: true, Stage: "fit", Reason: "fit_response"},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
}

// writeAuditSidecar writes the signals a stop hook would have left for this
// session and returns the path.
func writeAuditSidecar(t *testing.T, dir string) string {
	t.Helper()
	s := &audit.Signals{}
	s.AddProse("the scratch directory is reaped before the lifecycle run begins")
	path, err := audit.WriteSidecar(dir, s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	return path
}

// runLifecycleWithArgs points runLifecycle at argv for one test.
func runLifecycleWithArgs(t *testing.T, argv ...string) {
	t.Helper()
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = append([]string{orig[0], "lifecycle"}, argv...)
	runLifecycle()
}

// storedVerdicts reads back every verdict filed for the seeded project.
func storedVerdicts(t *testing.T, dataHome string) []memory.RetrievalAuditRow {
	t.Helper()
	db, err := memory.OpenReadDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	rows, err := memory.NewStore(db, nil).RetrievalAudits(context.Background(), auditLifecycleProject, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	return rows
}

// TestRunLifecycleJudgesTheSessionFromItsSidecar is the whole wiring in one run:
// the argv reaches the phase, the phase compares what the call kept against what
// the agent wrote, and the verdict is persisted for a report to read.
func TestRunLifecycleJudgesTheSessionFromItsSidecar(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditedCall(t, dataHome)
	sidecar := writeAuditSidecar(t, t.TempDir())

	runLifecycleWithArgs(t, "--project", auditLifecycleProject, "--signals", sidecar)

	rows := storedVerdicts(t, dataHome)
	if len(rows) != 1 {
		t.Fatalf("got %d stored verdict(s), want 1", len(rows))
	}
	if rows[0].MemoryID != auditLifecycleMemory {
		t.Errorf("verdict names memory %s, want %s", rows[0].MemoryID, auditLifecycleMemory)
	}
	if rows[0].Outcome != string(audit.OutcomeUsed) {
		t.Errorf("outcome = %q, want %q — the agent repeated the memory's own words", rows[0].Outcome, audit.OutcomeUsed)
	}
	if rows[0].RecordRowID <= 0 {
		t.Errorf("verdict is not attributable to a call (record_rowid = %d)", rows[0].RecordRowID)
	}
	if rows[0].Source != "search" {
		t.Errorf("source = %q, want the recorded call's own source", rows[0].Source)
	}
	// The child owns the file it was handed: it is read once, and the sweep only
	// exists for the runs that never got this far.
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("the sidecar outlived the run that read it: %v", err)
	}
}

// TestRunLifecycleSurvivesAnUnreadableSidecar: a path that is not one this build
// wrote is a refusal, not a comparison. The audit is lost; the chain is not.
func TestRunLifecycleSurvivesAnUnreadableSidecar(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditedCall(t, dataHome)
	foreign := filepath.Join(t.TempDir(), "ghost-audit-foreign.signals")
	if err := os.WriteFile(foreign, []byte("this is not a sidecar\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	runLifecycleWithArgs(t, "--project", auditLifecycleProject, "--signals", foreign)

	if rows := storedVerdicts(t, dataHome); len(rows) != 0 {
		t.Errorf("a refused sidecar filed %d verdict(s), want 0", len(rows))
	}
	// The witness that the chain ran on: reflect is skipped for want of an LLM
	// backend in this environment, and the marker for that skip is the last
	// thing runLifecycle writes.
	marker := filepath.Join(dataHome, "ghost", "lifecycle-last-failure-"+auditLifecycleProject+".json")
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the chain did not reach its end after a refused sidecar: %v", err)
	}
	// …and the refusal stayed OUT of it. The marker and the session-start alert
	// it raises are about consolidation — "Memory consolidation is paused" —
	// so a phase named here that never ran is a false claim in a surface every
	// session reads, and a hole in one report is the cheaper of the two.
	var filed struct {
		PhasesFailed []string `json:"phases_failed"`
	}
	if err := json.Unmarshal(raw, &filed); err != nil {
		t.Fatalf("the lifecycle marker is not readable: %v", err)
	}
	for _, ph := range filed.PhasesFailed {
		if ph != "reflect" {
			t.Errorf("the refused audit was recorded as a failed %q phase; the marker is about consolidation", ph)
		}
	}
	if _, err := os.Stat(foreign); !os.IsNotExist(err) {
		t.Errorf("a sidecar the child could not read must still be cleaned up by it: %v", err)
	}
}

// TestRunLifecycleSurvivesAMissingSidecar: the file can be gone before the child
// gets to it (a swept temp dir, a host that cleaned up early), and a turn's
// audit is not worth a lifecycle that stops.
func TestRunLifecycleSurvivesAMissingSidecar(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditedCall(t, dataHome)

	runLifecycleWithArgs(t, "--project", auditLifecycleProject,
		"--signals", filepath.Join(t.TempDir(), "never-written.signals"))

	if rows := storedVerdicts(t, dataHome); len(rows) != 0 {
		t.Errorf("a missing sidecar filed %d verdict(s), want 0", len(rows))
	}
	marker := filepath.Join(dataHome, "ghost", "lifecycle-last-failure-"+auditLifecycleProject+".json")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the chain did not reach its end after a missing sidecar: %v", err)
	}
}

// disableAutoPhases turns every automatic consolidation phase off, so a run
// here is a clean no-op for consolidation: phasesRan is 0 and failedPhases is
// empty, which is the one state FinishLifecycleRun deliberately leaves alone.
// That is what makes it the right control for a claim about what the marker
// does NOT get.
func disableAutoPhases(t *testing.T) {
	t.Helper()
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	const yaml = "reflection:\n  auto_reflect: false\n  auto_resolve: false\n  auto_supersede: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestAnAuditFailureIsNotAConsolidationFailure: the marker claim from the other
// direction. Nothing in this run writes one, so a refused sidecar must leave it
// unwritten — a marker naming a phase that never ran is a false claim in the
// session-start alert every session reads.
func TestAnAuditFailureIsNotAConsolidationFailure(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	disableAutoPhases(t)
	seedAuditedCall(t, dataHome)
	foreign := filepath.Join(t.TempDir(), "ghost-audit-foreign.signals")
	if err := os.WriteFile(foreign, []byte("this is not a sidecar\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	runLifecycleWithArgs(t, "--project", auditLifecycleProject, "--signals", foreign)

	marker := filepath.Join(dataHome, "ghost", "lifecycle-last-failure-"+auditLifecycleProject+".json")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(marker)
		t.Errorf("a refused sidecar wrote a consolidation failure marker: %s", raw)
	}
}

// TestRunLifecycleWithoutSignalsJudgesNothing: the flag is what turns the audit
// on, so a manual `ghost lifecycle` — the retry an operator runs from the
// maintenance alert — must not judge a session against nothing.
func TestRunLifecycleWithoutSignalsJudgesNothing(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditedCall(t, dataHome)

	runLifecycleWithArgs(t, "--project", auditLifecycleProject)

	if rows := storedVerdicts(t, dataHome); len(rows) != 0 {
		t.Errorf("a run with no --signals filed %d verdict(s), want 0", len(rows))
	}
}

// TestTheAuditReportStandsOnWhatItReads: the report is what an operator reads
// after the fact, so it must not carry the transcript, the memory's content or
// the id the comparison read. The summary is written to the child's stderr, which
// for a detached run is lifecycle.log.
func TestTheAuditReportStandsOnWhatItReads(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	seedAuditedCall(t, dataHome)
	sidecar := writeAuditSidecar(t, t.TempDir())

	printed := captureStderr(t, func() {
		runLifecycleWithArgs(t, "--project", auditLifecycleProject, "--signals", sidecar)
	})

	// The report may not carry the memory's id, its content, or a phrase only the
	// comparison read. The words are chosen to be absent from the chain's own
	// lifecycle lines — "scratch", for instance, is not: the scratch reap prints
	// one, and a test that forbade it would be testing the reap.
	for _, word := range []string{auditLifecycleMemory, "reaped", "lifecycle run"} {
		if strings.Contains(printed, word) {
			t.Errorf("the audit report carries %q: it is ids and counts", word)
		}
	}
	// …and it does say what it measured, including the limit that no heuristic can
	// find a re-derived fact.
	if !strings.Contains(printed, "retrieval audit for "+auditLifecycleProject) {
		t.Errorf("the report does not name the project it audited:\n%s", printed)
	}
	if !strings.Contains(printed, "re-derived") {
		t.Errorf("the report states no limit on what \"missed\" can mean:\n%s", printed)
	}
}
