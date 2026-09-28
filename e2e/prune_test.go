//go:build e2e

package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCLIPruneTiers walks the whole shape of the session tier through the real
// binary: a save that states a tier, a dry run that changes nothing, an apply that
// removes exactly the expired session row and leaves a tombstone behind, and a
// backup taken afterwards that still verifies.
//
// The ordering is the point. A prune is the only command in Ghost that deletes
// memories on its own authority, so the assertions are about what did NOT happen
// at every step: the durable and keep-forever rows survive a prune that removed
// a third, and the dry run that named that same row wrote nothing at all.
func TestCLIPruneTiers(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)

	// Deliberately unlike each other: two near-identical saves FOLD, and a fold
	// raises the surviving row's tier, so a fixture of paraphrases would leave
	// every row at the highest tier the loop reached.
	saves := []struct {
		content string
		tier    string
	}{
		{"the lab tunnel has mtu 1400 while we debug fragmentation", "session"},
		{"the deploy runbook says to drain the queue before rotating", ""},
		{"production ingress requires mtu 1500 for jumbo frames", "persistent"},
	}
	for _, save := range saves {
		args := map[string]any{"project_id": e2eProject, "content": save.content}
		if save.tier != "" {
			args["retention"] = save.tier
		}
		out := call(t, cs, "ghost_memory_save", args)
		if save.tier != "" && !strings.Contains(out, save.tier) {
			t.Errorf("save(%s) did not report the tier: %s", save.tier, out)
		}
	}

	// The tiers are in the database as saved, read back per project so the builtin
	// global seed cannot be mistaken for one of them.
	byTier := map[string]string{}
	for _, tier := range []string{"session", "project", "persistent"} {
		ids := s.queryStrings(t,
			"SELECT id FROM memories WHERE project_id = ? AND retention = ?", e2eProject, tier)
		if len(ids) != 1 {
			t.Fatalf("%d memories in the project at tier %q, want exactly 1", len(ids), tier)
		}
		byTier[tier] = ids[0]
	}

	// A refused tier is refused in the caller's words, and writes nothing.
	refused := callExpectingError(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a save whose tier is a typo",
		"retention":  "keep-forever",
	})
	for _, want := range []string{"session", "project", "persistent"} {
		if !strings.Contains(refused, want) {
			t.Errorf("the refusal does not name %q: %s", want, refused)
		}
	}

	// Age the session row the way time does: expired, and untouched since long
	// before the grace. Written straight to the store because the binary has no
	// way to move a clock, and this is the one thing a fixture has to simulate.
	ageSessionRow(t, s, byTier["session"], "-720 hours")

	// The dry run names the row and writes nothing.
	dry := s.mustRun("prune", "--grace", "1h")
	mustContain(t, "prune dry run", dry.stdout, "1 session memory would be removed")
	mustContain(t, "prune dry run", dry.stdout, "dry run, nothing was written")
	mustContain(t, "prune dry run", dry.stdout, byTier["session"])
	mustContain(t, "prune dry run", dry.stdout, "--apply")
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); n != 3 {
		t.Fatalf("%d project memories after the dry run, want 3: the default run deleted something", n)
	}
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'delete'", byTier["session"]); n != 0 {
		t.Errorf("the dry run left %d delete record(s) behind", n)
	}

	// The apply removes exactly that row.
	applied := s.mustRun("prune", "--apply", "--grace", "1h")
	mustContain(t, "prune apply", applied.stdout, "1 session memory removed")
	mustContain(t, "prune apply", applied.stdout, "phase=delete")
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); n != 2 {
		t.Fatalf("%d project memories after the apply, want 2", n)
	}
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memories WHERE id = ?", byTier["persistent"]); n != 1 {
		t.Error("the persistent row did not survive the prune")
	}
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memories WHERE id = ?", byTier["project"]); n != 1 {
		t.Error("the durable row did not survive the prune")
	}

	// The removal is auditable from the tool a user would reach for.
	history := s.mustRun("history", byTier["session"])
	mustContain(t, "history of a pruned memory", history.stdout, "delete")
	mustContain(t, "history of a pruned memory", history.stdout, "the lab tunnel has mtu 1400")

	// And a backup of the pruned store still verifies: the prune moved the memory
	// count, so the manifest a backup writes afterwards has to describe the store
	// as it now is rather than as it was.
	dest := filepath.Join(s.t.TempDir(), "post-prune.db")
	s.mustRun("backup", "--out", dest)
	verify := s.mustRun("backup", "verify", dest)
	for _, want := range []string{"sha256", "integrity check", "schema version", "row counts", "ok"} {
		mustContain(t, "verify after a prune", verify.stdout, want)
	}
	if got := countInFileWhere(t, dest, "SELECT COUNT(*) FROM memories WHERE project_id = ?", e2eProject); got != 2 {
		t.Errorf("the backup holds %d project memories, want 2: the manifest did not follow the prune", got)
	}
}

// TestCLIPruneIsNeverRunForYou is the negative of the command above, and it is the
// property the whole tier design exists to protect: no lifecycle pass, no hook and
// no scheduled task calls the prune. A store that is left alone keeps its expired
// session rows, which is what a user has to be able to rely on.
func TestCLIPruneIsNeverRunForYou(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "a session note an unattended pass must leave alone",
		"retention":  "session",
	})
	ids := s.queryStrings(t, "SELECT id FROM memories WHERE project_id = ? AND retention = 'session'", e2eProject)
	if len(ids) != 1 {
		t.Fatalf("%d session memories, want 1", len(ids))
	}
	ageSessionRow(t, s, ids[0], "-720 hours")

	// A Stop hook with every auto-consolidation phase enabled is the most work any
	// unattended path will do. It must not prune.
	s.reconfigure(configOpts{autoReflect: true, autoResolve: true, autoSupersede: true})
	stop := s.mustRun("hook", "stop", "--source", "claude-code")
	mustNotContain(t, "stop hook", stop.stdout+stop.stderr, "prune")
	if n := s.queryInt(t, "SELECT COUNT(*) FROM memories WHERE id = ?", ids[0]); n != 1 {
		t.Error("an unattended pass removed an expired session memory")
	}
}

// ageSessionRow backdates a session row's expiry and last write, which is the
// state a session memory reaches on its own after a day of not being used. It
// opens the store read-write because the binary has no clock argument, and the
// e2e process is the only writer while it holds the handle.
func ageSessionRow(t *testing.T, s *sandbox, id, offset string) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open the sandbox store read-write: %v", err)
	}
	defer db.Close() //nolint:errcheck
	db.SetMaxOpenConns(1)
	// The offset is a test constant, not input: it names how far back to pretend
	// the row's clock ran, in SQLite's spelled-out modifier ("-720 hours" — the
	// abbreviated "h" is not a modifier it knows, and an unknown one makes
	// datetime() return NULL rather than the row's own timestamp). The id is
	// bound, because an id is data.
	if _, err := db.Exec(`
		UPDATE memories
		SET expires_at = datetime('now', '`+offset+`'),
		    updated_at = datetime('now', '`+offset+`'),
		    created_at = datetime('now', '`+offset+`')
		WHERE id = ?`, id); err != nil {
		t.Fatalf("age the session row: %v", err)
	}
	// Read it back rather than trusting the write: a fixture that silently aged
	// nothing would make the prune assertions pass for the wrong reason.
	var expires string
	if err := db.QueryRow("SELECT expires_at FROM memories WHERE id = ?", id).Scan(&expires); err != nil {
		t.Fatalf("read expires_at back: %v", err)
	}
	at, err := time.Parse("2006-01-02 15:04:05", expires)
	if err != nil {
		t.Fatalf("the aged expires_at %q does not parse: %v", expires, err)
	}
	if !at.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("expires_at = %s, want at least an hour in the past", expires)
	}
}
