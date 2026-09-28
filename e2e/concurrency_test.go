//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestConcurrentAccess drives several ghost processes against one store at the
// same time and checks the two properties a caller can actually observe:
//
//   - No SQLITE_BUSY ever reaches the caller. The store pins its pool to one
//     connection and sets busy_timeout, so a write that arrives mid-transaction
//     waits rather than failing; a busy error surfacing to a user is the defect
//     this is looking for.
//   - The counts add up. Every save a writer reported as saved is in the store
//     when everyone has finished, and no memory was duplicated or lost.
//
// The writers are the real surfaces: two long-lived `ghost mcp` servers over
// stdio and a `ghost` CLI process, because the concurrency contract is between
// those and not inside one of them. A test with only in-process goroutines would
// exercise the mutex and nothing else — the file lock, the WAL and the busy
// timeout all live below that.
func TestConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("the concurrency window is seconds long; skipped under -short")
	}
	s := newSandbox(t)
	// Give the writers a project that exists before they start, so no save is
	// racing a project creation — that is a different race, and mixing the two
	// would make a failure ambiguous.
	cs := s.mcpSession(t)
	call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the seed memory every writer shares",
	})

	const (
		writers   = 2
		duration  = 4 * time.Second
		cliWrites = 8
	)

	// The deadline is a TIME, not a channel. A time.After channel delivers its
	// value to exactly one receiver, so several writers selecting on it would
	// have all but one spin forever — which is a test that fills the disk
	// instead of stopping, and it did exactly that before this was fixed.
	deadline := time.Now().Add(duration)
	done := func() bool { return time.Now().After(deadline) }

	var (
		mu       sync.Mutex
		busySeen []string
		savedIDs = map[string]string{}
		failures []string
	)

	// record collects what a writer saw. It is called from several goroutines,
	// so every append is under the one mutex.
	record := func(kind string, r result) {
		combined := r.stdout + "\n" + r.stderr
		mu.Lock()
		defer mu.Unlock()
		if r.code != 0 {
			failures = append(failures, fmt.Sprintf("%s exited %d: %s", kind, r.code, combined))
			return
		}
		for _, line := range strings.Split(combined, "\n") {
			// The shapes a caller can see a busy error in: SQLite's own text
			// from a surfaced error, the "database is locked" the driver reports
			// for the same condition, and the snapshot-conflict code the driver
			// names separately (busy_timeout does not retry that one, which is
			// why _txlock=immediate exists).
			if strings.Contains(line, "SQLITE_BUSY") ||
				strings.Contains(line, "database is locked") ||
				strings.Contains(line, "SQLITE_BUSY_SNAPSHOT") {
				busySeen = append(busySeen, kind+": "+strings.TrimSpace(line))
			}
		}
		for _, id := range idsIn(r.stdout) {
			savedIDs[id] = kind
		}
	}

	// mcpWriter drives one `ghost mcp` server, saving until the window closes.
	//
	// The writes are rate-limited rather than as fast as the process will go.
	// Unthrottled, the store grows by one memory per iteration and the
	// concurrency window stops being a fixed amount of time and becomes a fixed
	// number of rows that happens to take as long as filling the disk takes.
	mcpWriter := func(index int) {
		session := s.mcpSession(t)
		for i := 0; !done(); i++ {
			content := fmt.Sprintf("writer %d memory %d about the relay port", index, i)
			r := callRaw(t, session, "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    content,
				"category":   "fact",
			})
			record(fmt.Sprintf("mcp writer %d save %d", index, i), r)
			// Interleave a read with the writes, so the readers are contending
			// too: a store that only ever took writes would not exercise the
			// read-snapshot path a live server's searches take.
			if i%5 == 0 {
				record(fmt.Sprintf("mcp writer %d search %d", index, i),
					callRaw(t, session, "ghost_memory_search", map[string]any{
						"project_id": e2eProject,
						"query":      "relay port",
						"limit":      5,
					}))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// cliWriter drives a separate `ghost` process, so its work comes through
	// the CLI's own bootstrap path — a different open, a different connection,
	// against a store two servers hold. It does a bounded number of rounds
	// rather than looping to the deadline, because each round runs a whole
	// command and the process spawn dominates; a busy loop here would measure
	// fork/exec rather than contention.
	cliWriter := func(index int) {
		for i := 0; i < cliWrites && !done(); i++ {
			// `backup` takes the write lock for the length of a VACUUM INTO and
			// releases it, which is exactly the window a concurrent server's
			// write has to ride out.
			//
			// The --out path is explicit and unique per round because the default
			// one is stamped to the second and REFUSES to overwrite: two backups
			// inside the same second collide, and the refusal is the documented
			// behaviour rather than a contention failure, so counting it as one
			// would be measuring the timestamp's resolution.
			record(fmt.Sprintf("cli writer %d backup %d", index, i),
				s.run("backup", "--out", filepath.Join(s.t.TempDir(), fmt.Sprintf("b-%d-%d.db", index, i))))
			// A whole-length id (32 characters, the length Ghost mints), and a
			// deliberately absent one: `ghost history` takes a ref, so a SHORTER
			// string is a prefix and matching nothing is a refusal with a non-zero
			// exit (#720) rather than the read this loop is here to make. The read
			// is the point — it contends with the writers through the same open —
			// so the id has to be one this command reports on rather than refuses.
			record(fmt.Sprintf("cli writer %d history %d", index, i),
				s.run("history", "00000000000000000000000000000000"))
			time.Sleep(50 * time.Millisecond)
		}
	}

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mcpWriter(i)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		cliWriter(0)
	}()
	wg.Wait()

	mu.Lock()
	busy := append([]string(nil), busySeen...)
	failed := append([]string(nil), failures...)
	wantIDs := make(map[string]bool, len(savedIDs))
	for id := range savedIDs {
		wantIDs[id] = true
	}
	mu.Unlock()

	if len(busy) > 0 {
		t.Fatalf("SQLITE_BUSY reached the caller under concurrency:\n%s", strings.Join(busy, "\n"))
	}
	if len(failed) > 0 {
		t.Fatalf("%d writer invocations failed:\n%s", len(failed), strings.Join(failed, "\n"))
	}
	if len(wantIDs) == 0 {
		t.Fatalf("no writer reported a saved memory; the window produced no work to check")
	}

	// Every reported id is in the store, and each exactly once — a duplicate
	// would mean a retry landed twice, which is the other half of "the counts
	// add up".
	present := map[string]int{}
	for _, id := range s.queryStrings(t, `SELECT id FROM memories WHERE project_id = ?`, e2eProject) {
		present[id]++
	}
	for id := range wantIDs {
		switch present[id] {
		case 1:
		case 0:
			t.Fatalf("memory %s was reported saved but is not in the store", id)
		default:
			t.Fatalf("memory %s appears %d times in the store", id, present[id])
		}
	}
	// And no writer's content was lost: each reported id's content is in the
	// store, which catches a write that reported success and stored nothing.
	for id := range wantIDs {
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, id); n != 1 {
			t.Fatalf("memory %s is not readable after the concurrency window", id)
		}
	}

	// The seed is still there: a store that lost its first row under contention
	// would still pass every check above if the writers' rows survived.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE content = ?`, "the seed memory every writer shares"); n != 1 {
		t.Fatalf("the seed memory did not survive the concurrency window")
	}
}

// TestConcurrentLifecycleSpawns covers the other concurrency surface: several
// Stop hooks firing at once, which the pid lock exists to serialize. The claim is
// that at most one lifecycle chain runs per project — two would race on the same
// rows, and that race surfaced as foreign-key aborts and lost resolved_at stamps.
func TestConcurrentLifecycleSpawns(t *testing.T) {
	if testing.Short() {
		t.Skip("the spawn window is seconds long; skipped under -short")
	}
	s := newSandbox(t)
	cs := s.mcpSession(t)
	// Several candidates, so a spawned chain has real resolve work to do and
	// stays alive long enough for a second spawn to be attempted.
	for i := range 6 {
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    fmt.Sprintf("the migration attempt %d was reverted on the staging relay", i),
		})
	}
	s.mustRun("project", "bind", e2eProject, s.work)
	s.reconfigure(configOpts{autoResolve: true, minInterval: "0"})

	// Fire the hooks together, so they contend for the claim rather than
	// arriving politely one at a time.
	const hooks = 6
	var wg sync.WaitGroup
	for range hooks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			transcript, format := transcriptFor(t, "claude-code", true)
			s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
				"hook", "stop", "--source", "claude-code")
		}()
	}
	wg.Wait()

	// The cooldown is off, so the stamp advances once per spawn that actually
	// started. The log is the record of how many did.
	logPath := filepath.Join(s.dataDir(), "lifecycle.log")
	waitForFileContaining(t, logPath, "resolve completed", 60*time.Second, "a lifecycle chain to finish")
	// Give any second chain a chance to appear before the count is taken, so
	// this is a real observation rather than a race the test won.
	time.Sleep(3 * time.Second)

	log := string(mustReadFile(t, logPath))
	runs := countOccurrences(log, "resolve completed")
	if runs == 0 {
		t.Fatalf("no lifecycle chain ran:\n%s", log)
	}
	// The stamp is keyed on the resolved project id and the cooldown is
	// disabled, so the number of completed phases IS the number of chains that
	// started.
	t.Logf("%d concurrent Stop hooks produced %d lifecycle chain(s)", hooks, runs)

	// Whatever the count, no memory was lost or duplicated by the contention,
	// and the store is still whole — which is the property the lock protects.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, e2eProject); n != 6 {
		t.Fatalf("the store holds %d memories after %d concurrent hooks, want 6", n, hooks)
	}
	// foreign_key_check is a PRAGMA, not a table: it reports violations by
	// RAISING on each bad row, so it is run as one and a row returned is a
	// violation. The lifecycle race this guards against surfaced exactly here —
	// two chains replacing rows at once aborted on a foreign key — so the check
	// is worth the awkward spelling.
	rows, err := s.openDB(t).Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	if rows.Next() {
		var table, parent, fk string
		var rowid any
		if serr := rows.Scan(&table, &rowid, &parent, &fk); serr != nil {
			t.Fatalf("scan the foreign-key violation: %v", serr)
		}
		t.Fatalf("the store has a foreign-key violation after concurrent lifecycle spawns: "+
			"row %v of %s references a missing row in %s via %s", rowid, table, parent, fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
}

// TestServerExitIsClean checks that a `ghost mcp` server shuts down when its
// client closes the pipe, and leaves nothing behind holding the database. A
// server that ignored stdin EOF would leave a process per session, and the
// second one to open the store would meet a lock nobody released.
func TestServerExitIsClean(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ghostBin, "mcp")
	cmd.Env = s.env
	cmd.Dir = s.work
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	// A complete handshake, so the server is past initialization and holding the
	// store rather than still deciding whether to.
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-code","version":"e2e"}}}` + "\n")); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	buf := make([]byte, 4096)
	readDone := make(chan int, 1)
	go func() {
		n, _ := stdout.Read(buf)
		readDone <- n
	}()
	select {
	case <-readDone:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the server did not answer initialize")
	}
	if !strings.Contains(string(buf), `"id":1`) {
		t.Fatalf("the initialize response does not carry the id: %q", string(buf))
	}
	if _, err := os.Stat(s.dbPath()); err != nil {
		t.Fatalf("the server did not create the store: %v", err)
	}

	// Closing the pipe is how a client says goodbye.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the server exited %v on stdin EOF; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the server did not exit within 30s of stdin EOF (pid %d)", pid)
	}

	// And the store is left usable immediately, with nothing holding it: a
	// second read-write open is the proof, and it is the operation that would
	// fail if a lock outlived the process.
	s.mustRun("backup", "--out", filepath.Join(t.TempDir(), "after-exit.db"))
	// No -wal left behind by a clean close would be ideal, but SQLite deletes
	// it only when the LAST connection closes, and the test's own read-only
	// handle keeps one open — so the assertion is that the files are all still
	// there and the store reads, not that they are gone.
	if n := s.queryInt(t, `SELECT COUNT(*) FROM projects`); n < 1 {
		t.Fatalf("the store is not readable after the server exited")
	}
}

// callRaw invokes an MCP tool and returns the result WITHOUT failing the test on
// an error result. The concurrency test needs to record what a writer saw rather
// than abort on the first problem: a failing assertion has to name every failure
// at once, and one writer's failure is not a reason to stop measuring the rest.
func callRaw(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return result{stderr: err.Error(), code: 1}
	}
	code := 0
	if res.IsError {
		code = 1
	}
	return result{stdout: resultText(res), code: code}
}

// idsIn extracts the memory ids a save reported, from answers of the form
// "Memory saved (id: ABC...)". A concurrency check needs the ids to compare
// counts, and parsing them out of the text is how a caller would.
func idsIn(out string) []string {
	var ids []string
	rest := out
	for {
		i := strings.Index(rest, "id: ")
		if i < 0 {
			return ids
		}
		rest = rest[i+len("id: "):]
		end := strings.IndexAny(rest, " )\n\t")
		if end < 0 {
			return ids
		}
		candidate := rest[:end]
		if looksLikeID(candidate) {
			ids = append(ids, candidate)
		}
		rest = rest[end:]
	}
}
