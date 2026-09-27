// Command multiproc is the child-process half of the multi-process concurrency
// contract test in internal/memory (TestMultiProcessSharedDatabase).
//
// It exists because that test cannot re-exec its own test binary: under
// `go test` the test binary IS the suite, so a self-spawn re-runs every test in
// the package once per spawn, and a bug in the barrier logic becomes a fork
// bomb. So the test builds this program once with `go build` into a temp dir and
// starts one process per role. Each role is a real, separate process with its
// own *sql.DB, which is the unit the contract is written about: "Multiple Ghost
// processes may open the same database concurrently, and SQLite is the
// synchronization layer" (docs/architecture.md, "Concurrency contract").
//
// The roles, and the production shape each one stands for:
//
//	mcp      a live MCP server: the real mcpserver.Server on the real stdio
//	         transport, driven by a real MCP client session calling the real
//	         ghost_memory_save and ghost_memory_search tools.
//	cli      a CLI child: memory.OpenDB — what cmd/ghost's bootstrap hands every
//	         subcommand — plus the store writes a CLI makes. Upsert, then
//	         UpdateMemory, which reads a row and writes it back inside one
//	         transaction and is the case _txlock=immediate exists for.
//	ro       a read-only handle: memory.OpenReadDB, the shape the mcpinit
//	         hooks and `ghost obsidian` read through.
//	maint    a lifecycle writer: one ApplyReflection batch, the single
//	         transaction a reflect pass performs, plus a maintenance_runs row.
//	snapshot a long read transaction, opened the way ExplainSearchScoped opens
//	         its diagnostic snapshot, held across the batch commit and required
//	         to see the pre-batch state throughout.
//	poller   a fresh-snapshot sampler that reads the batch's row counts in a
//	         tight loop across the commit and records every distinct state it
//	         observes, so a batch that is not one transaction surfaces as an
//	         intermediate state instead of passing unnoticed.
//
// Every role reports what it measured as one JSON object on stdout. Failures go
// to stderr as text and set a non-zero exit status; the measurements are still
// reported, because a test that dies before printing its report tells the parent
// nothing about why.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
)

// report is the parent-facing result of one child run.
// internal/memory/multiproc_process_test.go declares the same shape for
// decoding; the two must agree on the JSON field names. This is a test protocol,
// not an API, so the agreement is by review rather than by a shared type — the
// alternative, a self-spawning test binary, is the fork-bomb risk the test
// exists to avoid.
//
// Values the parent asserts on travel in KV as strings, so a role can report a
// measurement the parent has no compile-time knowledge of, and a key the parent
// expects but the child never set fails as a missing key rather than as a
// silent zero.
type report struct {
	Role  string            `json:"role"`
	Label string            `json:"label"`
	KV    map[string]string `json:"kv"`
	// Observed holds the distinct states the poller role saw, in the order it
	// first saw them.
	Observed []string `json:"observed,omitempty"`
	// IDs holds every row id this process created and still holds a handle on.
	// The parent checks each one against the database afterwards, which is what
	// "no write is silently dropped" means across process boundaries: a child
	// that reported success and left no row has lost a memory.
	IDs []string `json:"ids,omitempty"`
	// Errs is every operation this process failed. Busy is the subset whose error
	// is SQLite lock contention, reported separately because "no SQLITE_BUSY
	// escaped" is the contract's central claim and deserves its own message
	// rather than being buried in a generic failure list. Busy is always a
	// subset of Errs, so a contention error cannot slip past by being
	// classified rather than reported.
	Errs []string `json:"errs,omitempty"`
	Busy []string `json:"busy,omitempty"`
	// Aborted is a failure this process inherited: it was waiting for a barrier
	// that another process's failure removed. It is kept out of Errs and Busy on
	// purpose — the contention belongs to the process that hit it, and ten
	// processes reporting the same inherited SQLITE_BUSY would bury the one that
	// actually caused it.
	Aborted string `json:"aborted,omitempty"`
}

func (r *report) put(key, value string) { r.KV[key] = value }

// count reads one measured counter, treating an unset key as zero: a role that
// never got far enough to set it reported nothing, and every caller either
// compares against a non-zero expectation or is counting.
func (r *report) count(key string) int {
	n, _ := strconv.Atoi(r.KV[key])
	return n
}

func (r *report) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.Errs = append(r.Errs, msg)
	if isBusy(msg) {
		r.Busy = append(r.Busy, msg)
	}
}

// isBusy reports whether a message is SQLite lock contention. Both the modernc
// driver's spelling and SQLite's own are matched because the error reaches this
// process through several layers — the store wraps it with fmt.Errorf — and the
// driver's text is not a contract.
func isBusy(msg string) bool {
	lower := strings.ToLower(msg)
	for _, needle := range []string{
		"sqlite_busy",
		"sqlite_locked",
		"database is locked",
		"database table is locked",
		"(517)",
		"(6)",
		"(5)",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// options are the flags the roles share. One flag set for all of them means a
// mistyped role name or path is a startup error with a usage line, not a silent
// no-op that lets the test pass for the wrong reason.
type options struct {
	role     string
	dbPath   string
	project  string
	label    string
	barrier  string
	seed     string
	replaced string
	load     string
	query    string
	probe    bool
	batch    int
	after    int
	writers  string
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.role, "role", "", "role to run (mcp, cli, ro, maint, snapshot, poller)")
	flag.StringVar(&o.dbPath, "db", "", "path to the shared database file")
	flag.StringVar(&o.project, "project", "", "project id this role reads and writes")
	flag.StringVar(&o.label, "label", "", "this process's label, used to name its barrier files")
	flag.StringVar(&o.barrier, "barrier", "", "directory holding the cross-process barrier files")
	flag.StringVar(&o.seed, "seed", "", "content prefix of the batch's pre-state rows")
	flag.StringVar(&o.replaced, "replaced", "", "content prefix of the batch's post-state rows")
	flag.StringVar(&o.load, "load", "", "content prefix for this role's steady-state writes")
	flag.StringVar(&o.query, "query", "", "FTS query the reader roles use")
	flag.BoolVar(&o.probe, "probe", false, "run the _txlock=immediate probe before joining the load")
	flag.IntVar(&o.batch, "batch", 0, "rows in the maint role's reflection batch")
	flag.IntVar(&o.after, "after", 0, "writes a load role completes before it reports itself in step")
	flag.StringVar(&o.writers, "writers", "", "comma-separated labels of the load's writers (maint role)")
	flag.Parse()
	return o
}

// barrierDeadline bounds every cross-process wait. A child that cannot reach the
// state it waits for reports the missing barrier and exits rather than blocking
// until the parent's context kills it, so a broken barrier is named in the
// failure instead of surfacing as a generic timeout. It is deliberately shorter
// than the parent's own barrier timeout, so the process that fails first is the
// one that reports it.
const barrierDeadline = 12 * time.Second

func main() {
	o := parseFlags()
	rep := &report{Role: o.role, Label: o.label, KV: map[string]string{}}
	if o.role == "" || o.dbPath == "" {
		rep.fail("missing required flag: role=%q db=%q", o.role, o.dbPath)
		emit(os.Stdout, rep)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The report is written to the stdout this process started with, captured
	// before any role runs: the mcp role stands a pipe in for os.Stdout while
	// its server session is up, and the report must not go down that pipe.
	stdout := os.Stdout

	b := barriers{dir: o.barrier}
	var err error
	switch o.role {
	case "mcp":
		err = runMCP(ctx, o, rep)
	case "cli":
		err = runCLI(ctx, o, rep, b)
	case "ro":
		err = runReadOnly(ctx, o, rep, b)
	case "maint":
		err = runMaintenance(ctx, o, rep, b)
	case "snapshot":
		err = runSnapshot(ctx, o, rep, b)
	case "poller":
		err = runPoller(ctx, o, rep, b)
	default:
		err = fmt.Errorf("unknown role %q", o.role)
	}
	if err != nil {
		// A barrier failure is this process giving up because another one gave
		// up, not an operation of its own that went wrong.
		var inherited barrierError
		if errors.As(err, &inherited) {
			rep.Aborted = err.Error()
		} else {
			rep.fail("%s: %v", o.role, err)
		}
		// Unblock every process still waiting on this one. A role that gives up
		// must say so where the others can see it, or a single failure costs the
		// whole barrier deadline before it is reported.
		_ = b.abort(err.Error())
		if o.role == "maint" {
			// The batch is what ends the steady-state load. A run that dies
			// before it commits must still release the writers.
			_ = b.signal(stopSignal)
		}
	}
	emit(stdout, rep)
	if len(rep.Errs) > 0 || rep.Aborted != "" {
		os.Exit(1)
	}
}

func emit(stdout *os.File, rep *report) {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintf(os.Stderr, "multiproc: encode report: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// barriers
// ---------------------------------------------------------------------------

// The barrier states, as file names. The ordering between processes is a
// property of these files, not of how long a machine happened to take: a reader
// that has pinned its snapshot announces it, and the writer does not commit
// until that announcement is on disk. A poll interval is a latency knob, not
// the synchronization.
const (
	// probeDone is written by the process that ran the _txlock probe. The parent
	// holds the rest of the fleet back until it appears, because the probe's
	// answer is only evidence about its own transaction while no other process
	// holds the write lock.
	probeDone = "probe-done"
	// loadReady is written by the parent once every load process has reported its
	// own wrote-<label> barrier, so the load is provably live before the batch
	// commits rather than assumed to be.
	loadReady = "load-ready"
	// polling is written by the poller once it is open and sampling on command.
	polling = "polling"
	// snapshotReady is written by the snapshot reader once its read
	// transaction has pinned a snapshot of the pre-batch state.
	snapshotReady = "snapshot-ready"
	// committing starts the poller's sampling window. Written immediately
	// before the batch, so the window brackets the commit instead of starting
	// after it.
	committing = "committing"
	// sampled is written by the poller after its first successful count. The
	// batch waits for it, so the pre-commit sample the atomicity assertion needs
	// is established by a file: without this the poller could be descheduled
	// through the whole batch and report only the post-state, failing the run for
	// having been slow rather than for having found anything.
	sampled = "sampled"
	// committed is written after the batch's transaction commits.
	committed = "committed"
	// snapshotDone is written after the snapshot reader has released its read
	// transaction, so the batch's process is not the one left holding the
	// database open against a snapshot somebody else still wants checkpointed.
	snapshotDone = "snapshot-done"
	// stopSignal ends every steady-state load loop.
	stopSignal = "stop"
	// abort is written by any role that fails, so the roles waiting on it fail
	// immediately and with the reason instead of running out the clock.
	abort = "abort"
)

type barriers struct{ dir string }

func (b barriers) path(name string) string { return filepath.Join(b.dir, name) }

func (b barriers) signal(name string) error {
	if b.dir == "" {
		return errors.New("no barrier directory configured (-barrier)")
	}
	return os.WriteFile(b.path(name), []byte(time.Now().Format(time.RFC3339Nano)), 0o600)
}

func (b barriers) signalled(name string) bool {
	_, err := os.Stat(b.path(name))
	return err == nil
}

func (b barriers) abort(reason string) error {
	if b.dir == "" {
		return errors.New("no barrier directory configured (-barrier)")
	}
	return os.WriteFile(b.path(abort), []byte(reason), 0o600)
}

// aborted reports the failure another process recorded, if there is one to
// report. An empty marker is not a report: os.WriteFile truncates before it
// writes, so a reader can catch the file in between and would otherwise fail a
// barrier wait with no reason at all — the one thing this protocol exists to
// avoid.
func (b barriers) aborted() (string, bool) {
	raw, err := os.ReadFile(b.path(abort))
	if err != nil {
		return "", false
	}
	reason := strings.TrimSpace(string(raw))
	if reason == "" {
		return "", false
	}
	return reason, true
}

// barrierError marks a wait that ended because another process failed or the
// state never arrived. It is distinct from an operation failure so the report
// can attribute the cause to the process that caused it.
type barrierError struct{ msg string }

func (e barrierError) Error() string { return e.msg }

// wait blocks until name exists, another process has aborted, or the deadline
// passes. The failure message names the barrier, because "timed out" without it
// is the least useful thing a multi-process test can report.
func (b barriers) wait(name string, deadline time.Time) error {
	for {
		if b.signalled(name) {
			return nil
		}
		if reason, ok := b.aborted(); ok {
			return barrierError{fmt.Sprintf("gave up waiting for barrier %q: another process failed: %s", name, reason)}
		}
		if time.Now().After(deadline) {
			return barrierError{fmt.Sprintf("barrier %q never appeared in %s", name, b.dir)}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// connection contract
// ---------------------------------------------------------------------------

// measureContract records the settings the concurrency contract in
// docs/architecture.md promises, read back from a live connection rather than
// from the DSN string that produced it. A DSN assertion passes while the string
// still contains what it always contained even if the driver has stopped
// honouring the parameter; a PRAGMA read cannot.
//
// journal_mode and foreign_keys are connection state, so they are read on the
// connection. max_open_conns is a property of the *sql.DB the process holds, so
// it is reported from the pool's own stats. The read-only check is behavioural
// — it attempts a write and requires the refusal — because "mode=ro" in a DSN
// is a claim and a failed INSERT is the fact.
func measureContract(ctx context.Context, rep *report, db *sql.DB, readOnly bool) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	var journal string
	var foreignKeys int
	var busyMS int
	scanErr := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal)
	if scanErr == nil {
		scanErr = conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys)
	}
	if scanErr == nil {
		scanErr = conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyMS)
	}
	// The connection is released before anything else touches db: the pool is
	// pinned to one connection, so holding this one while issuing a db.* call
	// waits for a connection only this code can release.
	_ = conn.Close()
	if scanErr != nil {
		return fmt.Errorf("read pragmas: %w", scanErr)
	}
	rep.put("journal_mode", strings.ToLower(journal))
	rep.put("foreign_keys", fmt.Sprint(foreignKeys))
	rep.put("busy_timeout_ms", fmt.Sprint(busyMS))
	rep.put("max_open_conns", fmt.Sprint(db.Stats().MaxOpenConnections))
	rep.put("read_only", fmt.Sprint(readOnly))
	return nil
}

// probeImmediateTxLock is the behavioural assertion for _txlock=immediate, which
// is a driver parameter rather than a pragma and so cannot be read back.
//
// A transaction opened with ReadOnly:false on this DSN issues BEGIN IMMEDIATE,
// which takes SQLite's write lock when the transaction starts rather than at its
// first write statement. So while such a transaction is open, a second
// connection's write must be refused. Under the default deferred BEGIN the same
// transaction would hold only a read snapshot, the second write would succeed,
// and the transaction's own read-to-write upgrade would later fail with
// SQLITE_BUSY_SNAPSHOT — the error busy_timeout does not retry, and the reason
// the setting exists.
//
// The answer is only evidence about our own transaction while no other process
// holds the write lock, so the parent runs this in a dedicated first process.
func probeImmediateTxLock(ctx context.Context, dbPath string) (bool, error) {
	// The challenger's table is created before the owner's transaction opens:
	// DDL needs the write lock too, so creating it afterwards would report
	// contention that has nothing to do with the setting under test.
	challenger, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(50)")
	if err != nil {
		return false, fmt.Errorf("open challenger handle: %w", err)
	}
	defer challenger.Close() //nolint:errcheck
	challenger.SetMaxOpenConns(1)
	if _, err := challenger.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS txlock_probe (id INTEGER PRIMARY KEY)`); err != nil {
		return false, fmt.Errorf("challenger create: %w", err)
	}

	owner, err := memory.OpenDB(dbPath)
	if err != nil {
		return false, fmt.Errorf("open owner handle: %w", err)
	}
	defer owner.Close() //nolint:errcheck

	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	// A read inside the transaction, so the probe covers a read-then-write
	// transaction rather than an empty one: that is the shape that fails with
	// SQLITE_BUSY_SNAPSHOT without the setting.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return false, fmt.Errorf("read inside transaction: %w", err)
	}

	// The challenger carries a short busy timeout so a held write lock surfaces
	// as a refusal in milliseconds instead of waiting out the 5s the contract's
	// writers allow. It is a probe, not a writer: the setting under test is the
	// one the owner's DSN carries, and the challenger's only job is to try to
	// write while the owner's transaction is open.
	if _, err := challenger.ExecContext(ctx, `INSERT OR REPLACE INTO txlock_probe (id) VALUES (1)`); err != nil {
		if isBusy(err.Error()) {
			// Refused while our transaction is open: the write lock was taken at
			// BEGIN, so the transaction is immediate.
			return true, nil
		}
		return false, fmt.Errorf("challenger insert: %w", err)
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// roles
// ---------------------------------------------------------------------------

// announceWrite is what a writer calls once, after a write it issued following
// the batch's announcement. The batch waits for this barrier from every writer, so
// "the load was live when the batch was announced" is a fact the run establishes
// with a file rather than one it infers from a timestamp.
//
// Nothing weaker works. The batch holds SQLite's write lock for its whole
// transaction, so no writer can commit *during* it; a row count taken either side
// measures the two gaps around the lock, and a writer checking the announcement
// flag on both sides of its own write cannot tell a write that straddled the
// instant from one that ran entirely after it. Waiting for a post-announcement
// write is the only ordering that is unambiguous, and it costs one write round.
func announceWrite(b barriers, rep *report, label string) error {
	if err := b.signal(announcedLabel(label)); err != nil {
		return err
	}
	rep.put("writes_after_announce", fmt.Sprint(rep.count("writes_after_announce")+1))
	return nil
}

// wroteLabel is the barrier a load process writes once it has completed its
// -after writes. Naming the barrier after the writer rather than a step count is
// what lets the batch wait for progress instead of for a clock.
func wroteLabel(label string) string { return "wrote-" + label }

// announcedLabel is the barrier a writer writes after a write it issued following
// the batch's announcement.
func announcedLabel(label string) string { return "announced-" + label }

func openStore(dbPath string) (*sql.DB, *memory.Store, error) {
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	return db, memory.NewStore(db, nil), nil
}

// savedID pulls the row id out of a ghost_memory_save response. A response that
// does not carry one is a failure rather than a fallback: the id is how the
// parent checks afterwards that a reported save left a row, so a save that
// reported no id would silently weaken the check. SQLite's hex() is uppercase,
// and the character class accepts either case.
var savedID = regexp.MustCompile(`\(id: ([0-9A-Fa-f]{32})\)`)

// A load role writes on every iteration until the batch releases it, which is
// what a live MCP server or CLI child does and what puts four writers against one
// write lock for the whole run.
//
// What keeps that from becoming a measurement of the corpus is the trigger, not a
// throttle: the batch waits for every writer to report -after writes and then ends
// the load within a write or two of committing, so the run is over before the
// corpus is large. The alternative — a writer looping while something *else*
// decided when the batch would come — is what grows the corpus without limit, and
// each write transaction then widens the near-duplicate scan inside it until one
// outlasts the very busy timeout whose sufficiency the test is checking, and the
// run fails on the environment rather than on the contract.
func runMCP(ctx context.Context, o options, rep *report) error {
	db, store, err := openStore(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	if err := measureContract(ctx, rep, db, false); err != nil {
		return err
	}

	session, cleanup, err := mcpSessionOverStdio(ctx, store)
	if err != nil {
		return err
	}
	defer cleanup()

	b := barriers{dir: o.barrier}
	writes := 0
	announced := false
	return loadLoop(ctx, rep, b, func(i int) error {
		content := fmt.Sprintf("%s mcp-%s-%03d about sqlite wal checkpointing", o.load, o.label, i)
		announcing := b.signalled(committing) && !announced
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ghost_memory_save",
			Arguments: map[string]any{"project_id": o.project, "content": content, "category": "fact"},
		})
		if err != nil {
			return fmt.Errorf("ghost_memory_save: %w", err)
		}
		if res.IsError {
			return fmt.Errorf("ghost_memory_save returned an error result: %s", textOf(res.Content))
		}
		match := savedID.FindStringSubmatch(textOf(res.Content))
		if match == nil {
			return fmt.Errorf("ghost_memory_save reported no memory id: %s", textOf(res.Content))
		}
		rep.IDs = append(rep.IDs, match[1])
		writes++
		rep.put("writes", fmt.Sprint(writes))
		if writes == o.after {
			if err := b.signal(wroteLabel(o.label)); err != nil {
				return err
			}
		}
		if announcing {
			announced = true
			if err := announceWrite(b, rep, o.label); err != nil {
				return err
			}
		}

		// Every third iteration is followed by a real search through the same
		// server, so this process reads its own writes as well as another
		// process's.
		if i%3 == 0 {
			search, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "ghost_memory_search",
				Arguments: map[string]any{"project_id": o.project, "query": o.query, "limit": 5},
			})
			if err != nil {
				return fmt.Errorf("ghost_memory_search: %w", err)
			}
			if search.IsError {
				return fmt.Errorf("ghost_memory_search returned an error result: %s", textOf(search.Content))
			}
			// No torn-row check here, and the omission is deliberate. The result
			// comes back as formatted text, so a row rewritten between the FTS
			// match and the read that hydrates the result is indistinguishable
			// from a stale index entry — and only the first is possible: a search
			// is two queries, and the second is free to see a newer version of
			// the row the first matched. Asserting on the text would report that
			// race as index rot. This role's reads therefore prove the tools
			// answer under contention, which is the only thing they can prove.
			rep.put("reads", fmt.Sprint(rep.count("reads")+1))
		}
		return nil
	})
}

// mcpSessionOverStdio runs the real MCP server on the real stdio transport and
// returns a client session talking to it.
//
// os.Stdin and os.Stdout are replaced with the read end of a pipe the client
// writes and the write end of a pipe the client reads, so the server's
// newline-delimited JSON-RPC frames cross a pipe exactly as they cross a
// client's stdio in production. The client then speaks to the other ends. Both
// ends run in this process on separate goroutines, and the SDK gives each side
// its own reader, so a request and a notification cannot deadlock each other —
// which is the one thing a synchronous pipe pair could otherwise do.
//
// The alternative, an in-memory transport pair, is not available from outside
// the mcpserver package: the field holding the SDK server is unexported and
// Run hardcodes StdioTransport. Hijacking stdio is how a test can reach the real
// transport without changing production code to accommodate one.
func mcpSessionOverStdio(ctx context.Context, store *memory.Store) (*mcp.ClientSession, func(), error) {
	// os.Pipe returns the read end first, so the server's input is the first
	// value here and the client's output the second.
	serverIn, toServer, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("pipe to server: %w", err)
	}
	fromServer, serverOut, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("pipe from server: %w", err)
	}

	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = serverIn, serverOut
	srv := mcpserver.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "multiproc-test")
	runCtx, cancelRun := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Run(runCtx) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "opencode", Version: "0"}, nil)
	// The client reports a name the server maps to a known harness, so a save's
	// provenance comes from the MCP session rather than from walking this
	// process's ancestors looking for one.
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: fromServer, Writer: toServer}, nil)
	if err != nil {
		cancelRun()
		return nil, nil, fmt.Errorf("client connect: %w", err)
	}
	cleanup := func() {
		_ = session.Close()
		cancelRun()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			fmt.Fprintln(os.Stderr, "multiproc: mcp server did not exit after the client closed")
		}
		_ = toServer.Close()
		_ = fromServer.Close()
		_ = serverIn.Close()
		_ = serverOut.Close()
		// The report still has to reach the real stdout, which the mcp role's
		// pipes stood in for the length of the session.
		os.Stdin, os.Stdout = origIn, origOut
	}
	return session, cleanup, nil
}

func runCLI(ctx context.Context, o options, rep *report, b barriers) error {
	db, store, err := openStore(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	if err := measureContract(ctx, rep, db, false); err != nil {
		return err
	}

	// The probe runs before this process joins the load, and the parent holds
	// the rest of the fleet back until it is done, so nothing else can hold the
	// write lock while the probe asks whether this transaction does.
	if o.probe {
		held, err := probeImmediateTxLock(ctx, o.dbPath)
		if err != nil {
			return err
		}
		rep.put("tx_lock_held", fmt.Sprint(held))
		if err := b.signal(probeDone); err != nil {
			return err
		}
	}

	// The ids this process has written, oldest first. The rewrite target is
	// always a row from earlier in this list rather than the one just written: a
	// row another transaction has already committed to is the one whose
	// read-then-write can collide with a commit in between.
	var created []string
	writes := 0
	announced := false
	return loadLoop(ctx, rep, b, func(i int) error {
		content := fmt.Sprintf("%s cli-%s-%03d about sqlite wal checkpointing", o.load, o.label, i)
		announcing := b.signalled(committing) && !announced
		id, duplicateOf, _, err := store.Upsert(ctx, o.project, "fact", content, "mcp", 0.5, []string{"concurrency"})
		if err != nil {
			return fmt.Errorf("Upsert: %w", err)
		}
		// A dedup hit is counted, not failed: Upsert inserts the incoming text as
		// its own row and links it to the near-duplicate it folded onto, so the
		// write is neither lost nor a second copy of one row. Reported so a
		// reader of the run's numbers can see how alike the fixture's rows are.
		if duplicateOf != "" {
			rep.put("duplicates", fmt.Sprint(rep.count("duplicates")+1))
		}
		rep.IDs = append(rep.IDs, id)
		writes++
		rep.put("writes", fmt.Sprint(writes))
		created = append(created, id)
		if writes == o.after {
			if err := b.signal(wroteLabel(o.label)); err != nil {
				return err
			}
		}
		if announcing {
			announced = true
			if err := announceWrite(b, rep, o.label); err != nil {
				return err
			}
		}

		if len(created) > 4 {
			// The rewritten text holds none of the query's terms, so an index
			// entry left pointing at it is a torn read the readers can see.
			updated := fmt.Sprintf("%s cli-%s-rewrite-%03d about kubernetes namespaces", o.load, o.label, i)
			if err := store.UpdateMemory(ctx, o.project, created[len(created)-5], &updated, nil, nil, nil); err != nil {
				return fmt.Errorf("UpdateMemory: %w", err)
			}
			rep.put("rewrites", fmt.Sprint(rep.count("rewrites")+1))
		}

		rows, err := store.SearchFTS(ctx, o.project, o.query, 5)
		if err != nil {
			return fmt.Errorf("SearchFTS: %w", err)
		}
		if err := checkNotTorn(rows, o.query); err != nil {
			return err
		}
		rep.put("reads", fmt.Sprint(rep.count("reads")+1))
		return nil
	})
}

func runReadOnly(ctx context.Context, o options, rep *report, b barriers) error {
	db, err := memory.OpenReadDB(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, nil)
	if err := measureContract(ctx, rep, db, true); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memories (project_id, content) VALUES (?, ?)`, o.project, "read-only probe write"); err == nil {
		rep.put("write_rejected", "false")
		return errors.New("a read-only handle accepted a write")
	}
	rep.put("write_rejected", "true")

	// A reader writes nothing, so it reports itself in step once it has completed
	// the same number of reads the writers count in writes. That keeps the
	// reader's searches inside the window the writers are colliding over, which
	// is the point of having them: they must not be stalled by it.
	reads := 0
	return loadLoop(ctx, rep, b, func(i int) error {
		rows, err := store.SearchFTS(ctx, o.project, o.query, 5)
		if err != nil {
			return fmt.Errorf("SearchFTS: %w", err)
		}
		if err := checkNotTorn(rows, o.query); err != nil {
			return err
		}
		reads++
		rep.put("reads", fmt.Sprint(reads))
		if reads == o.after {
			if err := b.signal(wroteLabel(o.label)); err != nil {
				return err
			}
		}
		return nil
	})
}

// runMaintenance is the lifecycle writer: one ApplyReflection batch — the single
// transaction a reflect pass performs — committed while every other process is
// writing the same file.
func runMaintenance(ctx context.Context, o options, rep *report, b barriers) error {
	// The steady-state load ends when this role finishes, batch or no batch.
	defer func() { _ = b.signal(stopSignal) }()

	db, store, err := openStore(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	if err := measureContract(ctx, rep, db, false); err != nil {
		return err
	}

	// Wait until the load is provably live and both observers are in position.
	// Each of these is a file another process wrote, so the batch cannot commit
	// before the state it must be observed from exists.
	for _, name := range []string{loadReady, polling, snapshotReady} {
		if err := b.wait(name, time.Now().Add(barrierDeadline)); err != nil {
			return err
		}
	}
	if err := b.signal("ready-" + o.label); err != nil {
		return err
	}
	// The poller starts sampling at this signal, so its window brackets the
	// commit rather than starting after it.
	if err := b.signal(committing); err != nil {
		return err
	}
	// …and does not start until the poller has actually taken a sample. Ten
	// processes on a small CI box means any of them can be descheduled across a
	// 60-row batch, and a sampler that only reads afterwards would report just
	// the post-state — indistinguishable, to the assertion that reads it, from a
	// batch that was never sampled at all.
	if err := b.wait(sampled, time.Now().Add(barrierDeadline)); err != nil {
		return err
	}
	// …and does not start until every writer has issued a write *after* that
	// announcement. This is what makes "the batch met a live load" a fact rather
	// than a hope: the batch cannot commit until each of them has come back
	// around its loop, so each one was writing across the announcement.
	for _, label := range strings.Split(o.writers, ",") {
		if label == "" {
			continue
		}
		if err := b.wait(announcedLabel(label), time.Now().Add(barrierDeadline)); err != nil {
			return err
		}
	}

	batch := make([]memory.Memory, 0, o.batch)
	for i := 0; i < o.batch; i++ {
		batch = append(batch, memory.Memory{
			Category:   "fact",
			Content:    fmt.Sprintf("%s %03d consolidated replacement for the batch", o.replaced, i),
			Importance: 0.5,
			Tags:       []string{"concurrency"},
			Source:     "reflection",
		})
	}
	consolidated, err := store.CurrentTimestamp(ctx)
	if err != nil {
		return fmt.Errorf("current timestamp: %w", err)
	}
	if _, _, _, err := store.ApplyReflection(ctx, o.project, batch, nil, consolidated, false); err != nil {
		return fmt.Errorf("ApplyReflection: %w", err)
	}
	rep.put("batch_rows", fmt.Sprint(len(batch)))

	// The maintenance table is written by the same process on the same handle,
	// so the "one maintenance writer" in the contract is this one and not the
	// batch alone.
	if err := memory.RecordMaintenanceRun(ctx, db, memory.MaintenanceRun{
		Kind: "multiproc-test", Note: "multi-process concurrency contract",
	}); err != nil {
		return fmt.Errorf("RecordMaintenanceRun: %w", err)
	}
	rep.put("maintenance_rows", "1")

	if err := b.signal(committed); err != nil {
		return err
	}
	// Hold the handle open until the snapshot reader has let go of its read
	// transaction, so this process's close is not the one that has to cope with
	// a WAL another connection still wants to checkpoint.
	return b.wait(snapshotDone, time.Now().Add(barrierDeadline))
}

// runSnapshot holds a read transaction across the batch commit and requires the
// pre-batch state to survive it.
//
// The transaction is opened exactly the way ExplainSearchScoped opens its
// diagnostic snapshot — BeginTx with ReadOnly — because that is the production
// shape: modernc.org/sqlite issues a plain BEGIN for a read-only transaction
// even under _txlock=immediate, so this is a WAL read snapshot that does not
// block the writer, which is the property under test. A write transaction here
// would take the write lock instead and stall the batch, proving nothing about
// snapshot isolation.
func runSnapshot(ctx context.Context, o options, rep *report, b barriers) error {
	db, err := memory.OpenReadDB(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	seed, replaced, err := batchCounts(ctx, tx, o.project, o.seed, o.replaced)
	if err != nil {
		return fmt.Errorf("pre-batch counts: %w", err)
	}
	rep.put("pre_seed", fmt.Sprint(seed))
	rep.put("pre_replaced", fmt.Sprint(replaced))

	// Only now is the snapshot pinned — the first read above acquired it — and
	// this file is what tells the batch it is safe to commit.
	if err := b.signal(snapshotReady); err != nil {
		return err
	}
	if err := b.wait(committed, time.Now().Add(barrierDeadline)); err != nil {
		return err
	}

	// The same transaction, so the same snapshot: the batch has committed and
	// this read must still describe the database as it was.
	inTxSeed, inTxReplaced, err := batchCounts(ctx, tx, o.project, o.seed, o.replaced)
	if err != nil {
		return fmt.Errorf("in-transaction counts: %w", err)
	}
	rep.put("in_tx_seed", fmt.Sprint(inTxSeed))
	rep.put("in_tx_replaced", fmt.Sprint(inTxReplaced))

	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("release snapshot: %w", err)
	}
	// A new snapshot, and the whole batch must be visible: nothing partial, and
	// nothing held back.
	postSeed, postReplaced, err := batchCounts(ctx, db, o.project, o.seed, o.replaced)
	if err != nil {
		return fmt.Errorf("post-batch counts: %w", err)
	}
	rep.put("post_seed", fmt.Sprint(postSeed))
	rep.put("post_replaced", fmt.Sprint(postReplaced))
	return b.signal(snapshotDone)
}

// runPoller samples the batch's row counts in a tight loop across the commit and
// records every distinct state it sees.
//
// A pinned snapshot cannot catch a non-atomic batch — it shows the pre-batch
// state whatever the writer did — so atomicity needs a reader that takes a fresh
// snapshot per sample. This role only records what it saw; the parent decides
// which states are legal, because only the parent knows the batch size.
func runPoller(ctx context.Context, o options, rep *report, b barriers) error {
	db, err := memory.OpenReadDB(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	if err := b.signal(polling); err != nil {
		return err
	}
	if err := b.wait(committing, time.Now().Add(barrierDeadline)); err != nil {
		return err
	}

	seen := map[string]bool{}
	var order []string
	sinceCommit := -1
	for round := 0; ; round++ {
		seed, replaced, err := batchCounts(ctx, db, o.project, o.seed, o.replaced)
		if err != nil {
			return fmt.Errorf("poll counts: %w", err)
		}
		state := fmt.Sprintf("seed=%d,replaced=%d", seed, replaced)
		if !seen[state] {
			seen[state] = true
			order = append(order, state)
			// Announce the first sample. The batch waits for this file, so the
			// pre-commit reading exists before the commit rather than by luck.
			if round == 0 {
				if err := b.signal(sampled); err != nil {
					return err
				}
			}
		}
		rep.put("samples", fmt.Sprint(rep.count("samples")+1))
		// The tail is counted from the commit signal, not from the first
		// sample. A read that begins while the writer is still finishing its
		// commit can legitimately describe the pre-batch snapshot, so a sampler
		// that stopped one read after the signal would be racing the commit
		// rather than measuring it — and, worse, would report a pre-batch state
		// as if it were the whole story.
		if b.signalled(committed) {
			if sinceCommit < 0 {
				sinceCommit = 0
			} else {
				sinceCommit++
			}
			// Sampling continues past the commit on purpose. A sampler that
			// stopped the instant it saw the post-state could have missed an
			// intermediate state just before it, and the tail is what makes the
			// pre-commit samples worth anything.
			if sinceCommit > postCommitSamples {
				break
			}
		}
		if round >= maxPollRounds {
			return fmt.Errorf("polled %d times without seeing the batch commit", maxPollRounds)
		}
		// The batch can give up without ever committing — a barrier it waits for
		// never arrives — and a sampler that keeps polling until its round cap
		// would turn that into minutes of nothing. The abort marker ends it, and
		// names why.
		if reason, ok := b.aborted(); ok {
			return barrierError{fmt.Sprintf("gave up polling after %d samples: %s", rep.count("samples"), reason)}
		}
	}
	rep.Observed = order
	return nil
}

const (
	// postCommitSamples is how many samples the poller takes after it has seen
	// the commit signal before it stops. It has to be more than one: the sample
	// that races the commit can still describe the pre-batch snapshot, so a
	// single post-signal read proves nothing about the post-batch state.
	postCommitSamples = 64
	// maxPollRounds bounds a poller that never sees the commit signal.
	maxPollRounds = 500000
)

// batchCounts counts the batch's pre-state and post-state rows in ONE statement,
// and that is load-bearing rather than tidy: two statements are two implicit
// transactions, so a reader that ran the seed count and then the replaced count
// could bracket the batch's commit and report a state no single snapshot ever
// held — an intermediate state manufactured by the reader, not observed in the
// database. instr(x, y) = 1 is an exact prefix test, with no LIKE metacharacter
// escaping to get wrong.
func batchCounts(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, project, seed, replaced string,
) (int, int, error) {
	var seedN, replacedN int
	if err := q.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM memories WHERE project_id = ? AND instr(content, ?) = 1),
		       (SELECT count(*) FROM memories WHERE project_id = ? AND instr(content, ?) = 1)`,
		project, seed, project, replaced).Scan(&seedN, &replacedN); err != nil {
		return 0, 0, err
	}
	return seedN, replacedN, nil
}

// ---------------------------------------------------------------------------
// shared load loop
// ---------------------------------------------------------------------------

// loadLoop runs body until the stop signal appears, so the steady-state writers
// provably overlap the lifecycle batch instead of finishing before it starts.
//
// Readiness is the caller's business, not the loop's: a role announces itself
// once it has completed -after writes, because that is the fact the batch needs —
// writers that are mid-stream against one another, not processes that have merely
// started. The batch therefore triggers on writer progress rather than on a
// timer, and the writers keep writing until the batch releases them, so they are
// live when it commits without the run having to hope they still are.
//
// The iteration cap is a backstop: the loop stays bounded even if the barrier
// protocol is broken, and it says so in the report rather than spinning.
func loadLoop(ctx context.Context, rep *report, b barriers, body func(int) error) error {
	const maxIterations = 20000
	for i := 0; i < maxIterations; i++ {
		if b.signalled(stopSignal) {
			rep.put("stopped_by", "signal")
			return nil
		}
		if err := body(i); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("load loop: %w", err)
		}
	}
	rep.put("stopped_by", "iteration-cap")
	return fmt.Errorf("load loop reached its %d-iteration cap without a stop signal", maxIterations)
}

// checkNotTorn requires that every row a search returned really holds the text
// its index entry claims to mirror.
//
// The FTS row matched the query, so the base-table content it points at must
// contain one of the query's terms. Checking only for a non-empty string would
// pass on a stale index entry left behind by a rewrite that never reached the
// index — which is the torn read this loop exists to catch, and which only
// appears once rows are being rewritten, not just inserted.
func checkNotTorn(rows []memory.Memory, query string) error {
	terms := strings.Fields(query)
	for _, r := range rows {
		if !containsAnyTerm(r.Content, terms) {
			return fmt.Errorf("torn read: id=%s matched %q but its content is %q", r.ID, query, r.Content)
		}
	}
	return nil
}

// containsAnyTerm reports whether content holds at least one term,
// case-insensitively. One match is enough: the index matched the query as a
// whole, and this only distinguishes the same fact from unrelated text that a
// stale index entry still points at.
func containsAnyTerm(content string, terms []string) bool {
	lower := strings.ToLower(content)
	for _, t := range terms {
		if strings.Contains(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

func textOf(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
