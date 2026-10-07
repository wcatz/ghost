package mcpserver

// #646 part 3: the COST of the retrieval block in ghost_health.
//
// OpenDB pins the pool at one connection, so every statement a tool issues is time a
// concurrent write cannot have the store at all. The retrieval block used to be
// ListProjects, then BuildReport per project, then MergeProjects — a health check
// whose cost was proportional to the number of checkouts on the machine, on the tool an
// agent calls precisely when something feels wrong.
//
// So this asserts the COST and not the arithmetic: the same number of statements
// whether the store holds one project or thirty. Counting at the driver is deliberate —
// a seam in the block would let the loop hide behind it, and the concern is real SQL.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/audit"
	"github.com/wcatz/ghost/internal/memory"

	sqlite "modernc.org/sqlite"
)

// healthQueryCounter tallies statements whose text names one of the two retrieval
// tables, which is the whole cost this block can spend: everything else ghost_health
// prints is bounded by the project's own memory count, not by the number of projects.
type healthQueryCounter struct {
	inner driver.Connector
	mu    sync.Mutex
	n     int
}

func (c *healthQueryCounter) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &healthCountingConn{Conn: conn, owner: c}
	if beginner, ok := conn.(driver.ConnBeginTx); ok {
		wrapped.ConnBeginTx = beginner
	}
	if execer, ok := conn.(driver.ExecerContext); ok {
		wrapped.ExecerContext = execer
	}
	if queryer, ok := conn.(driver.QueryerContext); ok {
		wrapped.QueryerContext = queryer
	}
	return wrapped, nil
}

func (c *healthQueryCounter) Driver() driver.Driver { return c.inner.Driver() }

func (c *healthQueryCounter) record(query string) {
	if !strings.Contains(query, "retrieval_record") && !strings.Contains(query, "retrieval_audit") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *healthQueryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n = 0
}

func (c *healthQueryCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// healthCountingConn embeds the driver's optional interfaces so the context-aware ones
// are promoted and intercepted. See internal/memory/explain_legcount_test.go, which
// wraps the same driver for the same reason.
type healthCountingConn struct {
	driver.Conn
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *healthQueryCounter
}

func (c *healthCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.owner.record(query)
	return c.QueryerContext.QueryContext(ctx, query, args)
}

func (c *healthCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.owner.record(query)
	return c.ExecerContext.ExecContext(ctx, query, args)
}

var (
	_ driver.Conn           = (*healthCountingConn)(nil)
	_ driver.QueryerContext = (*healthCountingConn)(nil)
	_ driver.ExecerContext  = (*healthCountingConn)(nil)
)

// countedHealthStore opens a store whose retrieval statements are counted, after the
// migrations have already run through OpenDB — so what is counted is the health block's
// own work and not the schema's.
func countedHealthStore(t *testing.T, projects int) (*memory.Store, *healthQueryCounter) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	dsn := "file:" + dbPath + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	inner, err := sqlite.NewConnector(dsn)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	counter := &healthQueryCounter{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	ctx := context.Background()
	for i := range projects {
		id := fmt.Sprintf("p%02d", i)
		if err := store.EnsureProject(ctx, id, "/tmp/"+id, id); err != nil {
			t.Fatalf("EnsureProject(%s): %v", id, err)
		}
		// Each project records one search and is judged, so every project has rows on
		// both tables. A project with no rows would let a per-project loop look free.
		if _, err := store.CreateWithID(ctx, id, id+"MEM", memory.Memory{
			Content: retrievalHealthContent, Category: "gotcha", Source: "manual",
		}); err != nil {
			t.Fatalf("CreateWithID(%s): %v", id, err)
		}
		if err := store.RecordRetrieval(ctx, memory.RetrievalRecord{
			ProjectID: id, SessionID: "s1", Source: "search", Outcome: "answerable",
			Verdicts: []memory.RowVerdict{{ID: id + "MEM", Kept: true, Stage: "fit", Reason: "fit_response"}},
		}); err != nil {
			t.Fatalf("RecordRetrieval(%s): %v", id, err)
		}
	}
	judgeEveryProject(t, store, projects)
	counter.reset()
	return store, counter
}

// judgeEveryProject files one verdict per recorded call, through the real lifecycle
// audit, so the counted store holds verdicts on both tables.
func judgeEveryProject(t *testing.T, store *memory.Store, projects int) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	hasher, err := audit.NewHasher(key)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	for i := range projects {
		id := fmt.Sprintf("p%02d", i)
		s := audit.NewWithHasher(hasher)
		s.SetSessionID("s1")               // the session the fixture's calls were made in
		s.SetAt(time.Now().Add(time.Hour)) // written after the calls the fixture recorded
		s.AddProse(retrievalHealthContent)
		if _, err := audit.Run(context.Background(), store, id, s); err != nil {
			t.Fatalf("audit.Run(%s): %v", id, err)
		}
	}
}

// TestHealthRetrievalCostDoesNotGrowWithTheProjectCount: one project, then thirty, and
// the SAME number of retrieval statements.
//
// Thirty is not decoration: it is the number of checkouts on a working laptop, and the
// loop this replaced issued two table reads per project on a pool of one connection —
// so the cost a test asserts as "unchanged" is the cost that used to be sixty reads.
func TestHealthRetrievalCostDoesNotGrowWithTheProjectCount(t *testing.T) {
	one, oneCounter := countedHealthStore(t, 1)
	// Called once so the count is the block's, not the seeding's; the one-project
	// figures themselves are not what this test is about, the thirty-project ones are.
	ghostHealthText(t, New(one, slog.New(slog.NewTextHandler(io.Discard, nil)), "test"))
	oneCount := oneCounter.count()

	many, manyCounter := countedHealthStore(t, 30)
	manyText := ghostHealthText(t, New(many, slog.New(slog.NewTextHandler(io.Discard, nil)), "test"))
	manyCount := manyCounter.count()

	// Two statements: one GROUP BY per table. Asserted as a fixed number rather than as
	// an inequality between the two, because an inequality passes just as happily on a
	// regression to three reads per project as on the fix.
	if want := 2; manyCount != want {
		t.Errorf("ghost_health issued %d retrieval statement(s) over a 30-project store, want %d — one GROUP BY per table, so the cost does not follow the project count", manyCount, want)
	}
	if oneCount != manyCount {
		t.Errorf("the block issued %d retrieval statement(s) for one project and %d for thirty: its cost must not depend on how many checkouts are registered", oneCount, manyCount)
	}

	// And the thirty-project figures are the pooled sum, not the one-project ones: a
	// cost fix that stopped reporting anything would satisfy the count above.
	for _, want := range []string{"30 call(s)", "30 kept", "100% used (30 of 30 scored)"} {
		if !strings.Contains(manyText, want) {
			t.Errorf("the 30-project health block does not report %q:\n%s", want, manyText)
		}
	}
}

// TestHealthSaysItsScopeIsTheStore: a per-source line sitting under ghost_health's
// project count and memory total reads as a statement about the project the agent is
// working in, unless the scope is on the face of the block. And an agent that does want
// one project's figures is told where they are, rather than left to reinterpret a
// store-wide line as a project-wide one.
func TestHealthSaysItsScopeIsTheStore(t *testing.T) {
	store, _ := testStoreWithPath(t)
	seedRetrievalHealth(t, store)

	_, text := retrievalHealthServer(t, store)

	if !strings.Contains(text, "store-wide") {
		t.Errorf("the retrieval block does not say its scope is the whole store:\n%s", text)
	}
	if !strings.Contains(text, "ghost context --audit --project") {
		t.Errorf("the retrieval block does not name the per-project report an agent should use instead:\n%s", text)
	}
}
